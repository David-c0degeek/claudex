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
	"sort"
	"strconv"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

// The prunable chain: an immutable, independently-sequenced structural CERTIFICATE names a
// retained-chain floor so recovery no longer demands an unbroken chain from generation 1.
// A certificate is <cert_seq>.anchor, framed and checksummed exactly like a generation
// record; its body binds a root generation + digest that must be an ACTUAL retained member.
const (
	anchorFormatVersion = 1
	certFileExt         = ".anchor"
	certFileDigits      = 12           // zero-padded like <gen>.gen
	maxCertSeq          = 999999999999 // the 12-digit sequence ceiling (mirrors maxGeneration)
	maxCertSize         = 4096         // certificate bodies are tiny JSON
	maxReadAttempts     = 8            // bounded lock-free read-bracket retries
)

var (
	// ErrInvalidKeep means PruneKeep was called with K < 1.
	ErrInvalidKeep = errors.New("genstore: keep count must be >= 1")
	// ErrConcurrentPrune is a TRANSIENT read outcome: a lock-free read raced a concurrent
	// prune and its bounded retries were exhausted. It is NEVER ErrCorrupt — the caller
	// retries (the store is not corrupt).
	ErrConcurrentPrune = errors.New("genstore: read raced a concurrent prune; retry")
	// ErrUnsupportedAnchor means the highest self-intact certificate carries an
	// anchor_format_version this build does not support: fail closed, upgrade required.
	ErrUnsupportedAnchor = errors.New("genstore: certificate anchor_format_version is unsupported; upgrade required")
)

// certBody is a certificate's checksummed payload. The root generation is PAYLOAD, not
// filename authority (the filename authorizes the sequence allocation).
type certBody struct {
	AnchorFormatVersion int    `json:"anchor_format_version"`
	CertificateSequence uint64 `json:"certificate_sequence"`
	RootGeneration      uint64 `json:"root_generation"`
	RootDigest          string `json:"root_digest"`
}

// certRecord is a self-intact certificate. For a supported valid version, Root/RootDigest
// are the strict-decoded binding; otherwise they are zero (only Seq/Version/Digest are
// meaningful, so the selection can fail closed rather than silently ignore it).
type certRecord struct {
	Seq        uint64
	Version    int
	Root       uint64
	RootDigest string
	Digest     string // the certificate file's trailer digest
}

// certClass classifies a certificate file. A checksum-INTACT file is a self-intact CANDIDATE
// (never skipped) — only a checksum-torn/oversize file is skipped. This is what stops a
// checksum-intact-but-malformed HIGHEST certificate from silently rolling back to a lower one.
type certClass int

const (
	certTorn             certClass = iota // checksum/framing invalid or oversize → skip (reserve the sequence)
	certUnsupported                       // self-intact, unknown anchor_format_version → fail closed if highest
	certSupportedValid                    // self-intact, version 1, valid body → usable
	certSupportedCorrupt                  // self-intact, version 1, INVALID body → ErrCorrupt if highest
)

// certCandidate is a self-intact certificate plus its classification.
type certCandidate struct {
	rec   certRecord
	class certClass
}

func (s *Store) certPath(seq uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%0*d%s", certFileDigits, seq, certFileExt))
}

// parseCanonicalCert accepts exactly a 12-digit certificate name that round-trips.
func parseCanonicalCert(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, certFileExt)
	if len(base) != certFileDigits {
		return 0, false
	}
	seq, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	if fmt.Sprintf("%0*d%s", certFileDigits, seq, certFileExt) != name {
		return 0, false
	}
	return seq, true
}

// encodeCert frames a certificate exactly like a generation record: uint64(len(json)) |
// json | sha256(all-preceding). Returns the file bytes and the trailer digest.
func encodeCert(cb certBody) ([]byte, string) {
	jb, _ := json.Marshal(cb)
	body := make([]byte, 8+len(jb))
	binary.BigEndian.PutUint64(body[:8], uint64(len(jb)))
	copy(body[8:], jb)
	trailer := sha256.Sum256(body)
	file := make([]byte, 0, len(body)+trailerLen)
	file = append(file, body...)
	file = append(file, trailer[:]...)
	return file, hex.EncodeToString(trailer[:])
}

// decodeCert classifies a certificate file at expectSeq. A checksum/framing-invalid or
// oversize file is certTorn (skipped). A checksum-INTACT file is a self-intact candidate:
// certUnsupported (unknown version), certSupportedValid (version 1, valid body), or
// certSupportedCorrupt (version 1 but a body that is unparseable, fails strict decode,
// disagrees with the filename sequence, has a zero root, or a malformed root digest — or any
// checksum-intact body that is not a supported version's valid JSON). A supportedCorrupt
// candidate is authoritative corruption, NEVER skipped/rolled-back.
func decodeCert(expectSeq uint64, file []byte) (certRecord, certClass) {
	if len(file) < 8+trailerLen || len(file) > maxCertSize {
		return certRecord{}, certTorn
	}
	body := file[:len(file)-trailerLen]
	trailer := file[len(file)-trailerLen:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], trailer) {
		return certRecord{}, certTorn
	}
	lenJ := binary.BigEndian.Uint64(body[:8])
	// The JSON must consume the WHOLE pre-trailer body: this format has no payload, so any
	// trailing bytes after the JSON are corruption. A length that overflows the body is torn.
	if lenJ > uint64(len(body)-8) || lenJ > maxCertSize {
		return certRecord{}, certTorn
	}
	digest := hex.EncodeToString(trailer)
	if lenJ != uint64(len(body)-8) {
		// Checksum-intact but with trailing bytes after the JSON → self-intact corruption.
		return certRecord{Seq: expectSeq, Digest: digest}, certSupportedCorrupt
	}
	jb := body[8 : 8+lenJ]

	// Loose version probe first: a FUTURE body shape must fail closed as unsupported, not be
	// skipped as torn by the strict decoder's unknown-field rejection.
	var probe struct {
		AnchorFormatVersion int `json:"anchor_format_version"`
	}
	if json.Unmarshal(jb, &probe) != nil {
		// Checksum-intact but not a JSON object → not a legitimate certificate of any version.
		return certRecord{Seq: expectSeq, Digest: digest}, certSupportedCorrupt
	}
	if probe.AnchorFormatVersion != anchorFormatVersion {
		return certRecord{Seq: expectSeq, Version: probe.AnchorFormatVersion, Digest: digest}, certUnsupported
	}

	var cb certBody
	if !strictCertBody(jb, &cb) {
		return certRecord{Seq: expectSeq, Version: anchorFormatVersion, Digest: digest}, certSupportedCorrupt
	}
	if cb.AnchorFormatVersion != anchorFormatVersion || cb.CertificateSequence != expectSeq {
		return certRecord{Seq: expectSeq, Version: anchorFormatVersion, Digest: digest}, certSupportedCorrupt // mislabeled (name/body sequence disagree)
	}
	if cb.RootGeneration == 0 || !isHexDigest(cb.RootDigest) {
		return certRecord{Seq: expectSeq, Version: anchorFormatVersion, Digest: digest}, certSupportedCorrupt
	}
	return certRecord{Seq: expectSeq, Version: anchorFormatVersion, Root: cb.RootGeneration, RootDigest: cb.RootDigest, Digest: digest}, certSupportedValid
}

func strictCertBody(jb []byte, cb *certBody) bool {
	dec := json.NewDecoder(bytes.NewReader(jb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cb); err != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

func isHexDigest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// enumerateCerts lists the certificate files, returning the SELF-INTACT candidates
// (ascending by sequence) with their class, the set of occupied sequences, and whether any
// canonical certificate files were present. It MIRRORS enumerate's taxonomy: a non-canonical
// name / symlink / non-regular entry / unreadable namespace observation fails closed
// (ErrCorrupt-class); only a checksum-TORN or oversize certificate is SKIPPED (its sequence
// stays reserved) — a checksum-intact certificate is always a candidate (so a malformed
// highest one fails closed rather than rolling back). A file that vanishes mid-scan is a
// concurrent prune deleting an OBSOLETE (lower) certificate — the highest is never deleted —
// so a not-exist is skipped.
func (s *Store) enumerateCerts() (candidates []certCandidate, occupied map[uint64]bool, present bool, err error) {
	entries, e := os.ReadDir(s.dir)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, map[uint64]bool{}, false, nil
		}
		return nil, nil, false, e
	}
	occupied = map[uint64]bool{}
	var seqs []uint64
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, certFileExt) {
			continue
		}
		seq, ok := parseCanonicalCert(name)
		if !ok {
			return nil, nil, true, fmt.Errorf("non-canonical certificate file %q: %w", name, ErrCorrupt)
		}
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
			return nil, nil, true, fmt.Errorf("certificate file %q is not a regular file: %w", name, ErrCorrupt)
		}
		info, ierr := ent.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				continue // an obsolete certificate deleted concurrently
			}
			return nil, nil, true, ierr
		}
		if !info.Mode().IsRegular() {
			return nil, nil, true, fmt.Errorf("certificate file %q is not a regular file: %w", name, ErrCorrupt)
		}
		occupied[seq] = true
		if info.Size() <= int64(maxCertSize) {
			seqs = append(seqs, seq)
		}
	}
	if len(occupied) == 0 {
		return nil, occupied, false, nil
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, seq := range seqs {
		data, rerr := os.ReadFile(s.certPath(seq))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // an obsolete certificate deleted concurrently
			}
			return nil, nil, true, rerr
		}
		rec, class := decodeCert(seq, data)
		if class == certTorn {
			continue // skip torn, but the sequence stays reserved (occupied)
		}
		candidates = append(candidates, certCandidate{rec: rec, class: class})
	}
	return candidates, occupied, true, nil
}

// selectCert returns the HIGHEST self-intact certificate by sequence. supportedValid → return
// it; unsupported → ErrUnsupportedAnchor; supportedCorrupt → ErrCorrupt. In no case does it
// roll back to a LOWER certificate. No self-intact candidate → (no certificate), so recovery
// uses the gen-1 base case.
func (s *Store) selectCert() (certRecord, bool, error) {
	candidates, _, _, err := s.enumerateCerts()
	if err != nil {
		return certRecord{}, false, err
	}
	if len(candidates) == 0 {
		return certRecord{}, false, nil
	}
	highest := candidates[len(candidates)-1] // enumerateCerts is ascending by sequence
	switch highest.class {
	case certSupportedValid:
		return highest.rec, true, nil
	case certUnsupported:
		return certRecord{}, false, fmt.Errorf("%w: certificate sequence %d has anchor_format_version %d (need %d)",
			ErrUnsupportedAnchor, highest.rec.Seq, highest.rec.Version, anchorFormatVersion)
	default: // certSupportedCorrupt
		return certRecord{}, false, fmt.Errorf("certificate sequence %d is checksum-intact but its body is invalid: %w",
			highest.rec.Seq, ErrCorrupt)
	}
}

// loadChain scans the generation files (mirroring the original enumerate), returning the
// self-intact records ascending, the occupied slot set, and whether generations are present.
// It is cert-aware for lock-free readers: a generation that vanishes mid-scan is IGNORED when
// strictly below ignoreBelow (the certified root — legitimately pruned) and otherwise signals
// transient=true (a root-or-above member disappeared, so the caller retries). A readPause seam
// (nil in production) lets a test interleave a concurrent prune at each step.
func (s *Store) loadChain(ignoreBelow uint64) (valid []Record, occupied map[uint64]bool, present bool, transient bool, err error) {
	entries, e := os.ReadDir(s.dir)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, map[uint64]bool{}, false, false, nil
		}
		return nil, nil, false, false, e
	}
	s.pause("after-readdir")
	occupied = map[uint64]bool{}
	var gens []uint64
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, genFileExt) {
			continue // certs, stale temps: outside the generation namespace
		}
		g, ok := parseCanonicalGen(name)
		if !ok {
			return nil, nil, true, false, fmt.Errorf("non-canonical generation file %q: %w", name, ErrCorrupt)
		}
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
			return nil, nil, true, false, fmt.Errorf("generation file %q is not a regular file: %w", name, ErrCorrupt)
		}
		info, ierr := ent.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				if g < ignoreBelow {
					continue
				}
				return nil, nil, true, true, nil
			}
			return nil, nil, true, false, ierr
		}
		if !info.Mode().IsRegular() {
			return nil, nil, true, false, fmt.Errorf("generation file %q is not a regular file: %w", name, ErrCorrupt)
		}
		occupied[g] = true
		if info.Size() <= int64(maxRecordSize) {
			gens = append(gens, g)
		}
	}
	if len(occupied) == 0 {
		return nil, occupied, false, false, nil
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	for _, g := range gens {
		s.pause(fmt.Sprintf("before-read-%d", g))
		data, rerr := os.ReadFile(s.genPath(g))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				if g < ignoreBelow {
					continue // legitimately pruned below the certified root
				}
				return nil, nil, true, true, nil // a root-or-above member vanished mid-scan
			}
			return nil, nil, true, false, rerr
		}
		if rec, ok := decode(g, data); ok {
			valid = append(valid, rec)
		}
	}
	return valid, occupied, true, false, nil
}

func (s *Store) pause(phase string) {
	if s.readPause != nil {
		s.readPause(phase)
	}
}

// WithReadPause installs a lock-free-read interleaving seam (called at each ReadDir/ReadFile
// step), so a test can run a concurrent prune mid-read and prove the bracket retries. nil in
// production.
func (s *Store) WithReadPause(fn func(phase string)) *Store { s.readPause = fn; return s }

// certifiedChainSlice returns the contiguous validated chain from the certified root (or
// generation 1 when there is no certificate) to the head. Records BELOW the certified root
// are not required. A missing/mismatched certified root is ErrCorrupt with NO fallback — this
// INCLUDES a store with a surviving certificate but ZERO generations (the certified root
// cannot be present), which must fail closed rather than be reported as an empty store.
func certifiedChainSlice(valid []Record, present bool, hasCert bool, cert certRecord) ([]Record, error) {
	if !present {
		if hasCert {
			return nil, fmt.Errorf("certified root generation %d is not present (no generations remain): %w", cert.Root, ErrCorrupt)
		}
		return nil, nil // genuinely empty: no certificate and no generations
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("records present but none valid: %w", ErrCorrupt)
	}
	if !hasCert {
		if valid[0].Generation != 1 || valid[0].PrevDigest != "" {
			return nil, fmt.Errorf("chain root is generation %d with prev %q (want generation 1, empty prev): %w",
				valid[0].Generation, shortDigest(valid[0].PrevDigest), ErrCorrupt)
		}
		return linkedFrom(valid, 0)
	}
	idx := -1
	for i := range valid {
		if valid[i].Generation == cert.Root {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, fmt.Errorf("certified root generation %d is not a present chain member: %w", cert.Root, ErrCorrupt)
	}
	if valid[idx].Digest != cert.RootDigest {
		return nil, fmt.Errorf("certified root generation %d digest mismatch: %w", cert.Root, ErrCorrupt)
	}
	return linkedFrom(valid, idx)
}

// linkedFrom validates that valid[start:] is a digest-linked chain and returns it.
func linkedFrom(valid []Record, start int) ([]Record, error) {
	chain := valid[start:]
	for i := 1; i < len(chain); i++ {
		if chain[i].PrevDigest != chain[i-1].Digest {
			return nil, fmt.Errorf("broken chain at generation %d: %w", chain[i].Generation, ErrCorrupt)
		}
	}
	return chain, nil
}

// chainHeadCertified returns the head of the certified chain (or the gen-1 chain).
func chainHeadCertified(valid []Record, present bool, hasCert bool, cert certRecord) (Record, bool, error) {
	chain, err := certifiedChainSlice(valid, present, hasCert, cert)
	if err != nil {
		return Record{}, false, err
	}
	if len(chain) == 0 {
		return Record{}, false, nil
	}
	return chain[len(chain)-1], true, nil
}

// loadGuarded reads the certified head material under a held guard (no concurrent prune, so
// loadChain's transient can never fire).
func (s *Store) loadGuarded() (valid []Record, occupied map[uint64]bool, present bool, cert certRecord, hasCert bool, err error) {
	cert, hasCert, err = s.selectCert()
	if err != nil {
		return
	}
	rootGen := uint64(0)
	if hasCert {
		rootGen = cert.Root
	}
	valid, occupied, present, _, err = s.loadChain(rootGen)
	return
}

// readHeadBracketed is the lock-free head read with the identity read-bracket. It selects the
// highest certificate + notes its identity, validates root..head, then re-selects and compares
// identity; a concurrent prune (a higher certificate published, or a root-or-above member
// deleted mid-scan) triggers a bounded retry. On exhaustion it returns the TYPED TRANSIENT
// ErrConcurrentPrune — NEVER ErrCorrupt.
func (s *Store) readHeadBracketed() (Record, bool, error) {
	for attempt := 0; attempt < maxReadAttempts; attempt++ {
		cert, hasCert, err := s.selectCert()
		if err != nil {
			return Record{}, false, err
		}
		rootGen := uint64(0)
		if hasCert {
			rootGen = cert.Root
		}
		valid, _, present, transient, lerr := s.loadChain(rootGen)
		if lerr != nil {
			return Record{}, false, lerr
		}
		if transient {
			continue
		}
		cert2, hasCert2, err2 := s.selectCert()
		if err2 != nil {
			return Record{}, false, err2
		}
		if hasCert != hasCert2 || cert != cert2 {
			continue // a concurrent prune published a higher certificate
		}
		return chainHeadCertified(valid, present, hasCert, cert)
	}
	return Record{}, false, ErrConcurrentPrune
}

// PruneKeep bounds the retained chain to at most K members, under the run lock only. It runs
// two phases: (1) advance the certificate ONLY when the retained chain has MORE than K members
// (never a superfluous certificate), publishing a new floor certificate DURABLY while the
// prior certificate and all generations still exist; (2) ALWAYS reconcile idempotently against
// the highest durable certificate — confirm it durable, then delete generations below its
// root and obsolete certificates, then sync. Phase 2 runs whether or not phase 1 advanced, so
// a crash in a prior prune's deletion is finished on re-run without a new certificate. K==1
// keeps only the head; K must be >= 1.
func (s *Store) PruneKeep(g *Guard, K int) error {
	if err := s.checkGuard(g); err != nil {
		return err
	}
	if K < 1 {
		return fmt.Errorf("%w: got %d", ErrInvalidKeep, K)
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	if err := s.pruneAdvance(g, K); err != nil {
		return err
	}
	return s.pruneReconcile(g)
}

// PruneKeepIf prunes to keep ONLY when the retained chain has reached trigger members
// (hysteresis — amortizing the certificate/fsync cost across appends). trigger must be > keep
// >= 1. It is the caller-driven form used at the txn journal's next-transaction preflight; the
// per-mutation state stores use the equivalent pre-append check inside AppendLocked. Run-lock
// only.
func (s *Store) PruneKeepIf(g *Guard, keep, trigger int) error {
	if err := s.checkGuard(g); err != nil {
		return err
	}
	if keep < 1 || trigger <= keep {
		return fmt.Errorf("%w: keep=%d trigger=%d", ErrInvalidKeep, keep, trigger)
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	// ALWAYS finish any prior prune's partial cleanup FIRST (idempotent; cheap when already
	// reconciled). A prior prune can publish a floor certificate then fail before/during
	// deletion, which bounds the CERTIFIED chain to keep — so without this the hysteresis gate
	// below would skip and leave the below-root stragglers forever. Reconciling first restores
	// the pinned "retry finishes maintenance before any semantic write" property.
	if err := s.pruneReconcile(g); err != nil {
		return err
	}
	valid, _, present, cert, hasCert, err := s.loadGuarded()
	if err != nil {
		return err
	}
	chain, cerr := certifiedChainSlice(valid, present, hasCert, cert)
	if cerr != nil {
		return cerr
	}
	if len(chain) < trigger {
		return nil // below the trigger: no new floor needed (any stragglers already reconciled)
	}
	return s.PruneKeep(g, keep)
}

// pruneAdvance publishes a new floor certificate when the retained chain exceeds K members.
func (s *Store) pruneAdvance(g *Guard, K int) error {
	valid, _, present, cert, hasCert, err := s.loadGuarded()
	if err != nil {
		return err
	}
	// certifiedChainSlice fails closed on a certificate whose root is not present (including a
	// store with a cert but zero generations), rather than treating it as empty.
	chain, cerr := certifiedChainSlice(valid, present, hasCert, cert)
	if cerr != nil {
		return cerr
	}
	if len(chain) == 0 {
		return nil // genuinely empty store (no cert, no generations): nothing to prune
	}
	if len(chain) <= K {
		return nil // <= K members: no certificate needed
	}
	floor := chain[len(chain)-K] // the (len-K)-th ACTUAL member (gap-safe, not head-K+1)

	// Root monotonicity, enforced HERE under the guard (recovery cannot compare against a
	// deleted predecessor): the new floor never precedes the prior certified root.
	if hasCert && floor.Generation < cert.Root {
		return fmt.Errorf("genstore: prune floor generation %d precedes the certified root %d", floor.Generation, cert.Root)
	}

	_, occSeq, _, eerr := s.enumerateCerts()
	if eerr != nil {
		return eerr
	}
	var highestSeq uint64
	for seq := range occSeq {
		if seq > highestSeq {
			highestSeq = seq
		}
	}
	nextSeq := highestSeq + 1
	for occSeq[nextSeq] { // never reuse a torn/occupied sequence (mirrors AppendLocked's skip)
		nextSeq++
	}
	if nextSeq > maxCertSeq {
		return fmt.Errorf("genstore: certificate sequence ceiling %d reached", uint64(maxCertSeq))
	}

	file, _ := encodeCert(certBody{
		AnchorFormatVersion: anchorFormatVersion,
		CertificateSequence: nextSeq,
		RootGeneration:      floor.Generation,
		RootDigest:          floor.Digest,
	})
	// Publish the certificate while the prior certificate and ALL generations still exist.
	if werr := s.write(s.certPath(nextSeq), file, filePerm); werr != nil {
		var ase *atomicfile.PostCommitSyncError
		if !errors.As(werr, &ase) {
			return werr // not visible → publish failed (a retry allocates nextSeq+1)
		}
		// Visible-but-unconfirmed: the ConfirmDurable below establishes durability.
	}
	return s.ConfirmDurable(g)
}

// pruneReconcile deletes everything obsoleted by the highest durable certificate. It is
// idempotent: it runs whether or not phase 1 advanced, finishing a prior prune's partial
// deletion on re-run.
func (s *Store) pruneReconcile(g *Guard) error {
	cert, hasCert, err := s.selectCert()
	if err != nil {
		return err
	}
	if !hasCert {
		return nil // never pruned: nothing to confirm or delete
	}
	entries, e := os.ReadDir(s.dir)
	if e != nil {
		return e
	}
	// Scan for the stragglers a prior prune may have left below the durable certificate,
	// validating the COMPLETE generation/certificate namespace BEFORE any removal. Reconcile
	// runs before loadGuarded, so it must itself fail closed here — mirroring loadChain's
	// ordering: a name in a namespace (by suffix) must be canonical, then regular, or it is
	// ErrCorrupt. A non-canonical `bad.gen` must abort the scan before any deletion, not slip
	// through to a later validation after the store has been mutated.
	var belowRoot, obsoleteCerts []uint64
	for _, ent := range entries {
		name := ent.Name()
		switch {
		case strings.HasSuffix(name, genFileExt):
			gen, ok := parseCanonicalGen(name)
			if !ok {
				return fmt.Errorf("non-canonical generation file %q: %w", name, ErrCorrupt)
			}
			if verr := requireRegular(ent, name, "generation"); verr != nil {
				return verr
			}
			if gen < cert.Root {
				belowRoot = append(belowRoot, gen)
			}
		case strings.HasSuffix(name, certFileExt):
			seq, ok := parseCanonicalCert(name)
			if !ok {
				return fmt.Errorf("non-canonical certificate file %q: %w", name, ErrCorrupt)
			}
			if verr := requireRegular(ent, name, "certificate"); verr != nil {
				return verr
			}
			if seq < cert.Seq {
				obsoleteCerts = append(obsoleteCerts, seq)
			}
		}
	}
	// Pin 4 + durability: confirm the selected certificate DURABLE and re-confirm the store
	// directory ALWAYS, even with no visible stragglers — a prior cleanup may have deleted every
	// straggler but failed its FINAL sync, which a visible-empty scan cannot distinguish from a
	// durably-completed cleanup, so the retry must re-run the real barrier to establish the
	// documented durable retention bound. This is also the pre-delete confirm.
	if err := s.ConfirmDurable(g); err != nil {
		return err
	}
	if len(belowRoot) == 0 && len(obsoleteCerts) == 0 {
		return nil // nothing to delete; the directory is re-confirmed durable above
	}
	// The selected certificate's EXACT identity must still hold before any deletion.
	data, rerr := os.ReadFile(s.certPath(cert.Seq))
	if rerr != nil {
		return rerr
	}
	rec, class := decodeCert(cert.Seq, data)
	if class != certSupportedValid || rec != cert {
		return fmt.Errorf("genstore: certificate %d changed during reconcile: %w", cert.Seq, ErrCorrupt)
	}
	// Delete every generation strictly below the certified root, then every obsolete
	// (lower-sequence) certificate; the selected certificate is retained.
	for _, gen := range belowRoot {
		if derr := os.Remove(s.genPath(gen)); derr != nil && !os.IsNotExist(derr) {
			return derr
		}
	}
	for _, seq := range obsoleteCerts {
		if derr := os.Remove(s.certPath(seq)); derr != nil && !os.IsNotExist(derr) {
			return derr
		}
	}
	// Finally sync the store directory so the deletions are durable (retryable, converges).
	return s.ConfirmDurable(g)
}

// requireRegular fails closed (ErrCorrupt) on a canonical-named namespace file that is a
// symlink or not a regular file, mirroring enumerate/loadChain — so a prune never deletes or
// trusts an irregular entry.
func requireRegular(ent os.DirEntry, name, kind string) error {
	if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
		return fmt.Errorf("%s file %q is not a regular file: %w", kind, name, ErrCorrupt)
	}
	info, ierr := ent.Info()
	if ierr != nil {
		return ierr
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s file %q is not a regular file: %w", kind, name, ErrCorrupt)
	}
	return nil
}
