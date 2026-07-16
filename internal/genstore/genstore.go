// Package genstore is the immutable-generation store that is the coordinator's
// root of trust (D017). State is an append-only sequence of self-validating
// generation records; a record is never modified once written.
//
// Each generation is a file NNNNNNNNNNNN.gen whose bytes are
// len(header)|header-json|payload|sha256(everything-before). A crash mid-write
// leaves a torn file that fails its checksum and is ignored; because a new
// generation is always a NEW file, writing it can never corrupt an existing
// valid generation — so the store does not rely on an atomic in-place replace
// (which Windows does not provide, D015).
//
// Recovery enumerates the generation files, keeps the self-intact ones, and
// verifies they form a consistent prev-digest chain. The highest record of that
// chain is the head; a torn newest generation is simply skipped, leaving the
// previous valid head. Records present but none valid — or a broken chain — is a
// fail-closed corruption error. There is no `current` pointer: enumeration at the
// coordinator's scale is cheap and removes a failure surface.
package genstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	trailerLen    = sha256.Size
)

// Sentinel errors (wrap with %w).
var (
	ErrConflict = errors.New("genstore: head changed (CAS conflict)")
	ErrBusy     = errors.New("genstore: store is locked by another process")
	ErrCorrupt  = errors.New("genstore: no valid generation chain")
)

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
	PayloadLen    int    `json:"payload_len"`
}

// Store persists generations under dir, serializing mutations with an OS lock.
type Store struct {
	dir      string
	lockPath string
}

// Open ensures dir exists and returns a store whose mutations are guarded by the
// advisory lock at lockPath.
func Open(dir, lockPath string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, lockPath: lockPath}, nil
}

func (s *Store) genPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%0*d%s", genFileDigits, gen, genFileExt))
}

func (s *Store) exists(gen uint64) bool {
	_, err := os.Stat(s.genPath(gen))
	return err == nil
}

// Latest returns the head record. It is lock-free: records are immutable, so a
// concurrent Append either has produced a complete new file or not. Returns
// (_, false, nil) for a truly empty store, and a wrapped ErrCorrupt when records
// are present but no valid chain can be formed.
func (s *Store) Latest() (Record, bool, error) {
	valid, present, err := s.enumerate()
	if err != nil {
		return Record{}, false, err
	}
	if !present {
		return Record{}, false, nil
	}
	head, err := headOf(valid)
	if err != nil {
		return Record{}, false, err
	}
	return head, true, nil
}

// Append writes the next generation. It is the store-owned compare-and-swap:
// under the lock it enumerates and validates the chain, requires the current head
// to equal expected, selects the next unused generation number (skipping any
// slot already occupied by an invalid/quarantined file, which leaves a
// documented gap), then invokes build with that number and the previous digest so
// the payload's own revision agrees with the filename and envelope generation.
func (s *Store) Append(expected Head, build func(next uint64, prevDigest string) ([]byte, error)) (Record, error) {
	lock, ok, err := oslock.TryAcquire(s.lockPath)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrBusy
	}
	defer lock.Release()

	valid, present, err := s.enumerate()
	if err != nil {
		return Record{}, err
	}
	var head Head
	if present {
		h, herr := headOf(valid)
		if herr != nil {
			return Record{}, herr
		}
		head = h.Head()
	}
	if expected != head {
		return Record{}, fmt.Errorf("%w: expected {gen:%d}, have {gen:%d}", ErrConflict, expected.Generation, head.Generation)
	}

	next := head.Generation + 1
	for s.exists(next) {
		next++ // skip a slot occupied by a quarantined/invalid file
	}

	payload, err := build(next, head.Digest)
	if err != nil {
		return Record{}, err
	}
	rec, file := encode(next, head.Digest, payload)

	if werr := atomicfile.Write(s.genPath(next), file, 0o644); werr != nil {
		var pce *atomicfile.PostCommitSyncError
		if errors.As(werr, &pce) {
			return rec, nil // committed and visible; only the durability sync failed
		}
		return Record{}, werr
	}
	return rec, nil
}

// enumerate lists the generation files, returning the self-intact records in
// ascending generation order and whether any generation files were present.
func (s *Store) enumerate() ([]Record, bool, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, false, err
	}
	type gf struct {
		gen  uint64
		name string
	}
	var gfs []gf
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), genFileExt) {
			continue
		}
		base := strings.TrimSuffix(e.Name(), genFileExt)
		g, perr := strconv.ParseUint(base, 10, 64)
		if perr != nil {
			continue
		}
		gfs = append(gfs, gf{gen: g, name: e.Name()})
	}
	if len(gfs) == 0 {
		return nil, false, nil
	}
	sort.Slice(gfs, func(i, j int) bool { return gfs[i].gen < gfs[j].gen })

	var valid []Record
	for _, g := range gfs {
		data, rerr := os.ReadFile(filepath.Join(s.dir, g.name))
		if rerr != nil {
			return nil, true, rerr // a present file we cannot read is an error, not "invalid"
		}
		if rec, okDecode := decode(g.gen, data); okDecode {
			valid = append(valid, rec)
		}
		// torn/invalid files are quarantined (skipped).
	}
	return valid, true, nil
}

// headOf verifies the valid records form a consistent prev-digest chain and
// returns the head (highest) record.
func headOf(valid []Record) (Record, error) {
	if len(valid) == 0 {
		return Record{}, fmt.Errorf("records present but none valid: %w", ErrCorrupt)
	}
	for i := 1; i < len(valid); i++ {
		if valid[i].PrevDigest != valid[i-1].Digest {
			return Record{}, fmt.Errorf("broken chain at generation %d: %w", valid[i].Generation, ErrCorrupt)
		}
	}
	return valid[len(valid)-1], nil
}

// encode builds the on-disk bytes and the Record for payload at gen.
func encode(gen uint64, prevDigest string, payload []byte) (Record, []byte) {
	pd := sha256.Sum256(payload)
	h := header{
		Format:        formatVersion,
		Generation:    gen,
		PrevDigest:    prevDigest,
		PayloadDigest: hex.EncodeToString(pd[:]),
		PayloadLen:    len(payload),
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
		Payload:       payload,
	}
	return rec, file
}

// decode validates a record file's bytes against the generation implied by its
// filename, returning ok=false for any torn/inconsistent record.
func decode(expectGen uint64, file []byte) (Record, bool) {
	if len(file) < 8+trailerLen {
		return Record{}, false
	}
	body := file[:len(file)-trailerLen]
	trailer := file[len(file)-trailerLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return Record{}, false
	}
	lenH := binary.BigEndian.Uint64(body[:8])
	if 8+lenH > uint64(len(body)) {
		return Record{}, false
	}
	hb := body[8 : 8+lenH]
	payload := body[8+lenH:]
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return Record{}, false
	}
	if h.Format != formatVersion || h.Generation != expectGen || h.PayloadLen != len(payload) {
		return Record{}, false
	}
	pd := sha256.Sum256(payload)
	if hex.EncodeToString(pd[:]) != h.PayloadDigest {
		return Record{}, false
	}
	return Record{
		Generation:    h.Generation,
		PrevDigest:    h.PrevDigest,
		PayloadDigest: h.PayloadDigest,
		Digest:        hex.EncodeToString(trailer),
		Payload:       payload,
	}, true
}
