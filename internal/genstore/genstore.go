// Package genstore is the coordinator's root of trust: an append-only
// sequence of self-validating immutable generation records. A record is never
// modified once written, so writing a new generation cannot corrupt a valid one
// and no atomic in-place replace is required (Windows does not provide one).
//
// Each generation is a file NNNNNNNNNNNN.gen (exactly 12 canonical digits) whose
// bytes are uint64(len(header)) | header-json | payload | sha256(all-preceding).
// A crash mid-write leaves a torn file that fails its checksum and is ignored.
//
// Recovery enumerates the generation files, keeps the self-intact ones, and
// verifies they form a chain rooted at generation 1 (empty prev-digest) with each
// later record linking to the previous by digest (gaps from quarantined slots are
// allowed). A torn newest generation is skipped, leaving the previous head.
// Records present with no valid rooted chain — a missing/torn root, an orphaned
// suffix, a broken link, or a non-canonical/irregular generation file — is a
// fail-closed corruption error.
//
// Locking is composable: a Guard holds one per-run mutation lock that can span
// several stores' appends (the transaction the journal and state share). Open is
// side-effect-free; the store directory is created lazily under the guard.
package genstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/oslock"
)

const (
	formatVersion = 1
	genFileExt    = ".gen"
	genFileDigits = 12
	maxGeneration = 999999999999 // the 12-digit filename ceiling
	trailerLen    = sha256.Size
	maxHeaderSize = 4096     // header JSON cap
	maxRecordSize = 16 << 20 // per-file cap, guards read-allocation from a corrupt file
	// maxPayloadSize is the largest payload whose complete framed record is
	// guaranteed to fit under maxRecordSize (using the worst-case header size).
	maxPayloadSize = maxRecordSize - 8 - maxHeaderSize - trailerLen
	dirPerm        = os.FileMode(0o700)
	filePerm       = os.FileMode(0o600)
)

// Sentinel errors (wrap with %w).
var (
	ErrConflict  = errors.New("genstore: head changed (CAS conflict)")
	ErrBusy      = errors.New("genstore: store is locked by another process")
	ErrCorrupt   = errors.New("genstore: no valid generation chain")
	ErrWrongLock = errors.New("genstore: guard is for a different lock")
	ErrAmbiguous = errors.New("genstore: write outcome could not be reconciled")
	ErrTooLarge  = errors.New("genstore: record exceeds the maximum size")
)

// PostCommitError reports that the generation was committed but the lock release
// failed afterwards. A caller must treat the write as done and must NOT retry.
type PostCommitError struct {
	Generation uint64
	Err        error
}

func (e *PostCommitError) Error() string {
	return fmt.Sprintf("genstore: generation %d committed but lock release failed: %v", e.Generation, e.Err)
}
func (e *PostCommitError) Unwrap() error   { return e.Err }
func (e *PostCommitError) Committed() bool { return true }

// PostCommitSyncError reports that a generation was written and is VISIBLE as the
// head, but its durability is UNCONFIRMED: the directory sync that makes the entry
// power-safe failed. It is DISTINCT from PostCommitError (which reports an
// already-durable record whose only failure was releasing the lock). A caller must
// NOT rewrite the visible record and must NOT treat it as durable success: it must
// re-confirm durability (ConfirmDurable, re-runnable across a restart) before
// depending on the record or advancing a transaction past it. Only the
// proven-candidate reconcile branch produces this; an ambiguous outcome stays
// ErrAmbiguous.
type PostCommitSyncError struct {
	Generation uint64
	Err        error
}

func (e *PostCommitSyncError) Error() string {
	return fmt.Sprintf("genstore: generation %d committed but durability is unconfirmed: %v", e.Generation, e.Err)
}
func (e *PostCommitSyncError) Unwrap() error { return e.Err }

// IsDurabilityUnconfirmed reports whether err is the genstore visible-but-unconfirmed
// durability condition. It matches ONLY *genstore.PostCommitSyncError — never an
// ErrAmbiguous outcome that merely wraps a lower-level *atomicfile.PostCommitSyncError,
// since an ambiguous write is not a proven-visible record. Direct atomicfile callers
// classify the atomicfile type themselves.
func IsDurabilityUnconfirmed(err error) bool {
	if errors.Is(err, ErrAmbiguous) {
		return false
	}
	var pce *PostCommitSyncError
	return errors.As(err, &pce)
}

// Guard is a held per-run mutation lock. One guard can serialize appends across
// several stores that share the same lock path (the run's transaction).
type Guard struct {
	lock     *oslock.Lock
	lockPath string
}

// Acquire takes the mutation lock non-blockingly. ok=false means another process
// holds it.
func Acquire(lockPath string) (g *Guard, ok bool, err error) {
	l, ok, err := oslock.TryAcquire(lockPath)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Guard{lock: l, lockPath: lockPath}, true, nil
}

// Release releases the guard's lock.
func (g *Guard) Release() error {
	if g == nil || g.lock == nil {
		return nil
	}
	err := g.lock.Release()
	g.lock = nil
	return err
}

// Head identifies the current tip of the chain for compare-and-swap.
type Head struct {
	Generation uint64
	Digest     string
}

// Record is one immutable generation.
type Record struct {
	Generation    uint64
	PrevDigest    string
	PayloadDigest string
	Digest        string // digest of the whole record; the next record's PrevDigest
	Payload       []byte
}

// Head returns this record's head identity.
func (r Record) Head() Head { return Head{Generation: r.Generation, Digest: r.Digest} }

type header struct {
	Format        int    `json:"format"`
	Generation    uint64 `json:"generation"`
	PrevDigest    string `json:"prev_digest"`
	PayloadDigest string `json:"payload_digest"`
	PayloadLen    uint64 `json:"payload_len"`
}

// Store persists generations under dir, guarded by the lock at lockPath.
type Store struct {
	dir      string
	lockPath string
	write    func(path string, data []byte, perm os.FileMode) error
	syncDir  func(dir string) error
	release  func(*Guard) error
}

// Open returns a store handle. It performs no I/O: the directory is created
// lazily under the guard by the first append, so a read-only Latest never
// mutates disk.
func Open(dir, lockPath string) *Store {
	return &Store{
		dir:      dir,
		lockPath: lockPath,
		write:    atomicfile.Write,
		syncDir:  atomicfile.SyncDir,
		release:  func(g *Guard) error { return g.Release() },
	}
}

// ensureDir creates the store directory private and durable, and tightens an
// existing one. The store directory's own entry is made durable in its parent
// (atomicfile.MkdirAllDurable fsyncs each created level's parent), so a power loss
// after the first append cannot remove the whole store while leaving its parent.
func (s *Store) ensureDir() error {
	if err := atomicfile.MkdirAllDurable(s.dir, dirPerm); err != nil {
		return err
	}
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("genstore: store path %q is not a directory", s.dir)
	}
	if runtime.GOOS != "windows" {
		// MkdirAll does not tighten a pre-existing broad directory.
		if err := os.Chmod(s.dir, dirPerm); err != nil {
			return err
		}
	}
	return nil
}

// confirmAttempts bounds ConfirmDurable's retry of a transient directory-sync
// failure under the held guard.
const confirmAttempts = 3

// ConfirmDurable re-confirms that this store's visible records are power-safe: it
// fsyncs the store directory (its generation entries) AND the store directory's own
// entry in its parent, retrying a transient failure a bounded number of times. It is
// idempotent and re-runnable — recovery calls it before trusting a visible record,
// since a PostCommitSyncError leaves the entry visible but unconfirmed and that
// provenance does not survive a crash. It MUST NOT create a missing store: a missing
// directory while confirming an applied effect is a durability failure, never a
// silent create.
func (s *Store) ConfirmDurable(g *Guard) error {
	if err := s.checkGuard(g); err != nil {
		return err
	}
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("genstore: confirm durable %q: %w", s.dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("genstore: store path %q is not a directory", s.dir)
	}
	parent := filepath.Dir(s.dir)
	var last error
	for i := 0; i < confirmAttempts; i++ {
		if last = s.syncDir(s.dir); last == nil {
			if last = s.syncDir(parent); last == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("genstore: confirm durable %q: %w", s.dir, last)
}

// LockPath is the mutation lock this store's guard must hold.
func (s *Store) LockPath() string { return s.lockPath }

// WithSyncDir overrides the directory-sync seam and returns the store, so a test can
// drive ConfirmDurable / durability failures deterministically. Production wires
// atomicfile.SyncDir in Open; this exists only to inject failures.
func (s *Store) WithSyncDir(fn func(dir string) error) *Store { s.syncDir = fn; return s }

func (s *Store) genPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%0*d%s", genFileDigits, gen, genFileExt))
}

// CheckGuard reports whether g is a valid guard for this store's lock, without
// performing any I/O — so a caller can validate the guard before running any
// side-effecting work under it.
func (s *Store) CheckGuard(g *Guard) error { return s.checkGuard(g) }

func (s *Store) checkGuard(g *Guard) error {
	if g == nil || g.lock == nil {
		return errors.New("genstore: nil or released guard")
	}
	if g.lockPath != s.lockPath {
		return fmt.Errorf("%w: guard=%q store=%q", ErrWrongLock, g.lockPath, s.lockPath)
	}
	return nil
}

// Latest returns the head record. It is lock-free: records are immutable, so a
// concurrent append either produced a complete new file or not. (_, false, nil)
// is a truly empty (or absent) store; a wrapped ErrCorrupt means records are
// present but no valid rooted chain exists.
func (s *Store) Latest() (Record, bool, error) {
	valid, _, present, err := s.enumerate()
	if err != nil {
		return Record{}, false, err
	}
	return chainHead(valid, present)
}

// Append is the convenience form: it acquires the guard, appends, and releases.
func (s *Store) Append(expected Head, build func(next uint64, prevDigest string) ([]byte, error)) (Record, error) {
	g, ok, err := Acquire(s.lockPath)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrBusy
	}
	rec, aerr := s.AppendLocked(g, expected, build)
	rerr := s.release(g)
	if aerr == nil && rerr != nil {
		// The record is committed, but the lock release failed: report it as a
		// committed error so a caller never retries a done transition.
		return rec, &PostCommitError{Generation: rec.Generation, Err: rerr}
	}
	return rec, aerr
}

// AppendLocked appends the next generation under an already-held guard, so a
// caller can compose several stores' appends into one critical section. The guard
// must hold this store's lock.
func (s *Store) AppendLocked(g *Guard, expected Head, build func(next uint64, prevDigest string) ([]byte, error)) (Record, error) {
	if err := s.checkGuard(g); err != nil {
		return Record{}, err
	}
	if err := s.ensureDir(); err != nil { // init under the lock
		return Record{}, err
	}

	valid, occupied, present, err := s.enumerate()
	if err != nil {
		return Record{}, err
	}
	headRec, hasHead, err := chainHead(valid, present)
	if err != nil {
		return Record{}, err
	}
	var head Head
	if hasHead {
		head = headRec.Head()
	}
	if expected != head {
		return Record{}, fmt.Errorf("%w: expected {gen:%d,digest:%s}, have {gen:%d,digest:%s}",
			ErrConflict, expected.Generation, shortDigest(expected.Digest), head.Generation, shortDigest(head.Digest))
	}

	next := head.Generation + 1
	for occupied[next] { // skip a slot already holding a (possibly quarantined) file
		next++
	}
	if next > maxGeneration {
		return Record{}, fmt.Errorf("genstore: generation ceiling %d reached", uint64(maxGeneration))
	}

	payload, err := build(next, head.Digest)
	if err != nil {
		return Record{}, err
	}
	// Reject an oversize payload BEFORE encoding/writing, so the old head stays
	// intact and no unreadable (later-quarantined) generation is ever committed.
	if len(payload) > maxPayloadSize {
		return Record{}, fmt.Errorf("%w: payload %d bytes exceeds limit %d", ErrTooLarge, len(payload), maxPayloadSize)
	}
	rec, file := encode(next, head.Digest, payload)

	if werr := s.write(s.genPath(next), file, filePerm); werr != nil {
		return s.reconcile(rec, head, werr)
	}
	return rec, nil
}

// reconcile interprets an ambiguous write outcome by re-validating the WHOLE
// chain exactly once (not just the candidate file). It never re-invokes the
// builder. The write committed only if the validated head is exactly the
// candidate; if the old validated head still stands, the write did not commit and
// the original error is returned; anything else (corrupt ancestor, broken
// namespace, a different head) is ambiguous.
func (s *Store) reconcile(candidate Record, oldHead Head, werr error) (Record, error) {
	valid, _, present, eerr := s.enumerate()
	if eerr == nil {
		if headRec, hasHead, cerr := chainHead(valid, present); cerr == nil {
			var head Head
			if hasHead {
				head = headRec.Head()
			}
			switch {
			case hasHead && head.Digest == candidate.Digest:
				// Committed: the candidate is the validated head. If the write's failure
				// was a committed-but-unsynced directory sync (visible, durability
				// unconfirmed), surface it as a durability error the caller must confirm
				// before depending on the record — never as clean success.
				var pse *atomicfile.PostCommitSyncError
				if errors.As(werr, &pse) {
					return candidate, &PostCommitSyncError{Generation: candidate.Generation, Err: werr}
				}
				return candidate, nil
			case head == oldHead:
				// Old head intact (including an empty store, where both are the
				// zero Head): the write did not commit — return the original error.
				return Record{}, werr
			}
		}
	}
	return Record{}, fmt.Errorf("%w: %w", ErrAmbiguous, werr)
}

// enumerate lists the generation files, returning the self-intact records
// (ascending), the set of occupied generation numbers, and whether any canonical
// generation files were present. A non-canonical, irregular, or unreadable
// generation file is a corruption error, not a silent empty store.
func (s *Store) enumerate() (valid []Record, occupied map[uint64]bool, present bool, err error) {
	entries, e := os.ReadDir(s.dir)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, map[uint64]bool{}, false, nil // absent store == empty
		}
		return nil, nil, false, e
	}

	occupied = map[uint64]bool{}
	var gens []uint64
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, genFileExt) {
			continue // e.g. a stale atomicfile temp; outside our namespace
		}
		g, ok := parseCanonicalGen(name)
		if !ok {
			return nil, nil, true, fmt.Errorf("non-canonical generation file %q: %w", name, ErrCorrupt)
		}
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
			return nil, nil, true, fmt.Errorf("generation file %q is not a regular file: %w", name, ErrCorrupt)
		}
		info, ierr := ent.Info()
		if ierr != nil {
			return nil, nil, true, ierr
		}
		if !info.Mode().IsRegular() {
			return nil, nil, true, fmt.Errorf("generation file %q is not a regular file: %w", name, ErrCorrupt)
		}
		occupied[g] = true
		if info.Size() <= int64(maxRecordSize) {
			gens = append(gens, g)
		}
		// An over-size file stays occupied (blocks the slot) but is never read.
	}
	if len(occupied) == 0 {
		return nil, occupied, false, nil
	}

	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	for _, g := range gens {
		data, rerr := os.ReadFile(s.genPath(g))
		if rerr != nil {
			return nil, nil, true, rerr
		}
		if rec, ok := decode(g, data); ok {
			valid = append(valid, rec)
		}
		// torn/invalid files are quarantined (skipped) but remain occupied.
	}
	return valid, occupied, true, nil
}

// chainHead validates that valid forms a chain rooted at generation 1 with an
// empty prev-digest and returns its head record.
func chainHead(valid []Record, present bool) (Record, bool, error) {
	if !present {
		return Record{}, false, nil
	}
	if len(valid) == 0 {
		return Record{}, false, fmt.Errorf("records present but none valid: %w", ErrCorrupt)
	}
	if valid[0].Generation != 1 || valid[0].PrevDigest != "" {
		return Record{}, false, fmt.Errorf("chain root is generation %d with prev %q (want generation 1, empty prev): %w",
			valid[0].Generation, shortDigest(valid[0].PrevDigest), ErrCorrupt)
	}
	for i := 1; i < len(valid); i++ {
		if valid[i].PrevDigest != valid[i-1].Digest {
			return Record{}, false, fmt.Errorf("broken chain at generation %d: %w", valid[i].Generation, ErrCorrupt)
		}
	}
	return valid[len(valid)-1], true, nil
}

// parseCanonicalGen accepts exactly a 12-digit generation name that round-trips.
func parseCanonicalGen(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, genFileExt)
	if len(base) != genFileDigits {
		return 0, false
	}
	g, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	if fmt.Sprintf("%0*d%s", genFileDigits, g, genFileExt) != name {
		return 0, false // e.g. leading '+', overflowed, or wrong width
	}
	return g, true
}

// encode builds the on-disk bytes and the Record for payload at gen. The Record's
// Payload is a copy so a caller mutating the input cannot desync it from the
// digest.
func encode(gen uint64, prevDigest string, payload []byte) (Record, []byte) {
	pd := sha256.Sum256(payload)
	h := header{
		Format:        formatVersion,
		Generation:    gen,
		PrevDigest:    prevDigest,
		PayloadDigest: hex.EncodeToString(pd[:]),
		PayloadLen:    uint64(len(payload)),
	}
	hb, _ := json.Marshal(h)

	body := make([]byte, 8+len(hb)+len(payload))
	binary.BigEndian.PutUint64(body[:8], uint64(len(hb)))
	copy(body[8:], hb)
	copy(body[8+len(hb):], payload)

	trailer := sha256.Sum256(body)
	file := make([]byte, 0, len(body)+trailerLen)
	file = append(file, body...)
	file = append(file, trailer[:]...)

	rec := Record{
		Generation:    gen,
		PrevDigest:    prevDigest,
		PayloadDigest: h.PayloadDigest,
		Digest:        hex.EncodeToString(trailer[:]),
		Payload:       append([]byte(nil), payload...),
	}
	return rec, file
}

// decode validates a record file against the generation implied by its filename.
// It is overflow- and resource-safe: the header length is bounds-checked without
// unsigned wraparound and capped.
func decode(expectGen uint64, file []byte) (Record, bool) {
	if len(file) < 8+trailerLen || len(file) > maxRecordSize {
		return Record{}, false
	}
	body := file[:len(file)-trailerLen]
	trailer := file[len(file)-trailerLen:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], trailer) {
		return Record{}, false
	}
	lenH := binary.BigEndian.Uint64(body[:8])
	// body has at least 8 bytes; compare before adding to avoid wraparound.
	if lenH > uint64(len(body)-8) || lenH > maxHeaderSize {
		return Record{}, false
	}
	hb := body[8 : 8+lenH]
	payload := body[8+lenH:]

	var h header
	if !strictHeader(hb, &h) {
		return Record{}, false
	}
	if h.Format != formatVersion || h.Generation != expectGen || h.PayloadLen != uint64(len(payload)) {
		return Record{}, false
	}
	if pd := sha256.Sum256(payload); hex.EncodeToString(pd[:]) != h.PayloadDigest {
		return Record{}, false
	}
	return Record{
		Generation:    h.Generation,
		PrevDigest:    h.PrevDigest,
		PayloadDigest: h.PayloadDigest,
		Digest:        hex.EncodeToString(trailer),
		Payload:       append([]byte(nil), payload...),
	}, true
}

// strictHeader decodes a single JSON header, rejecting unknown fields and
// trailing content. The record checksum already makes any accidental byte
// mutation evident, so duplicate-key/null detection is not repeated here.
func strictHeader(hb []byte, h *header) bool {
	dec := json.NewDecoder(bytes.NewReader(hb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(h); err != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
