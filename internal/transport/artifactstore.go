package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

const (
	artifactPerm     = 0o600
	maxArtifactBytes = 1 << 20
)

var (
	// ErrArtifactCollision means a same-key write carries different bytes.
	ErrArtifactCollision = errors.New("transport: artifact digest collision")
	// ErrBadArtifactKey means the turn id or digest is not a safe key.
	ErrBadArtifactKey = errors.New("transport: invalid artifact key")
	// ErrArtifactMismatch means the bytes do not match their key (not canonical,
	// not redacted, wrong digest, wrong envelope turn id, not a submit artifact
	// type, or schema-invalid).
	ErrArtifactMismatch = errors.New("transport: artifact does not match its key")
)

// artifactMessageTypes is the allowlist of submit artifact types, derived from
// the single TurnSpec source, so a non-artifact message (an assignment, a
// receipt) can never be persisted as an artifact.
var artifactMessageTypes = func() map[string]bool {
	m := make(map[string]bool)
	for _, spec := range turnSpecs {
		m[spec.ArtifactMessageType] = true
	}
	return m
}()

// ArtifactStore persists immutable, content-addressed submit artifacts under a
// confined os.Root (so a symlink cannot escape it) via internal/atomicfile's
// rooted, no-clobber publication. It is the durable ArtifactSink the submit path
// writes to before recording acceptance.
type ArtifactStore struct {
	root *os.Root
	// syncInRoot is the durable re-confirmation barrier for an EXISTING artifact file
	// (idempotent Put re-confirm and the recovery-side Confirm). A seam so a test can
	// prove callers halt on a failed barrier; production wires atomicfile.SyncInRoot.
	syncInRoot func(root *os.Root, rel string) error
}

// ArtifactStore is the real ArtifactSink from the submit path.
var _ ArtifactSink = (*ArtifactStore)(nil)

// NewArtifactStore roots an artifact store at dir.
func NewArtifactStore(dir string) (*ArtifactStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &ArtifactStore{root: r, syncInRoot: atomicfile.SyncInRoot}, nil
}

// WithSyncInRoot overrides the durable re-confirmation seam. Test-only: it exists to
// inject a barrier failure, never to weaken production durability.
func (s *ArtifactStore) WithSyncInRoot(fn func(root *os.Root, rel string) error) *ArtifactStore {
	s.syncInRoot = fn
	return s
}

// Close releases the root handle.
func (s *ArtifactStore) Close() error { return s.root.Close() }

// Put verifies the artifact against its key and publishes it without replacing an
// existing one. A repeat write of the same content is idempotent and durably
// re-confirmed; a same-key byte mismatch is a collision.
func (s *ArtifactStore) Put(turnID, digest string, canonical []byte) error {
	if err := s.verify(turnID, digest, canonical); err != nil {
		return err
	}
	if err := atomicfile.MkdirInRoot(s.root, turnID, 0o700); err != nil {
		return err
	}
	rel := turnID + "/" + digest + ".json"
	err := atomicfile.InstallInRoot(s.root, rel, canonical, artifactPerm)
	if errors.Is(err, fs.ErrExist) {
		existing, rerr := s.read(rel)
		if rerr != nil {
			return rerr
		}
		if !bytes.Equal(existing, canonical) {
			return ErrArtifactCollision
		}
		return s.syncInRoot(s.root, rel) // durably re-confirm the existing file
	}
	return err // nil, or a committed *PostCommitSyncError the submit path treats as not-durable
}

// Confirm re-reads an existing artifact by its exact key, re-verifies every key check
// (digest, canonicalization, redaction, envelope, schema), and durably re-confirms the
// file — the recovery-side barrier: a vanished, corrupted, or visible-but-durability-
// unconfirmed artifact is never trusted by a transaction about to make its acceptance
// durable.
func (s *ArtifactStore) Confirm(turnID, digest string) error {
	if _, err := s.Get(turnID, digest); err != nil {
		return err
	}
	return s.syncInRoot(s.root, turnID+"/"+digest+".json")
}

// Get reads an artifact and re-verifies every key check before returning bytes.
func (s *ArtifactStore) Get(turnID, digest string) ([]byte, error) {
	if !state.IsRunID(turnID) || !state.IsHex64(digest) {
		return nil, ErrBadArtifactKey
	}
	b, err := s.read(turnID + "/" + digest + ".json")
	if err != nil {
		return nil, err
	}
	if err := s.verify(turnID, digest, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *ArtifactStore) read(rel string) ([]byte, error) {
	return atomicfile.ReadInRoot(s.root, rel, maxArtifactBytes)
}

// verify enforces that the bytes are canonical, already-redacted JSON that hash
// to the digest, carry an envelope turn_id matching the path, name one of the
// allowlisted submit artifact types, and validate against that schema.
func (s *ArtifactStore) verify(turnID, digest string, canonical []byte) error {
	if !state.IsRunID(turnID) {
		return fmt.Errorf("%w: turn id", ErrBadArtifactKey)
	}
	if !state.IsHex64(digest) {
		return fmt.Errorf("%w: digest", ErrBadArtifactKey)
	}
	if len(canonical) == 0 || len(canonical) > maxArtifactBytes {
		return fmt.Errorf("%w: size", ErrBadArtifactKey)
	}
	c, err := canonjson.Canonicalize(canonical)
	if err != nil || !bytes.Equal(c, canonical) {
		return fmt.Errorf("%w: not canonical JSON", ErrArtifactMismatch)
	}
	// Persistence boundary: the bytes must already be redacted (rewriting would
	// change the digest).
	r, err := canonjson.Canonicalize(redact.Bytes(canonical))
	if err != nil || !bytes.Equal(r, canonical) {
		return fmt.Errorf("%w: not redacted", ErrArtifactMismatch)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("%w: digest", ErrArtifactMismatch)
	}
	var env struct {
		MessageType string `json:"message_type"`
		TurnID      string `json:"turn_id"`
	}
	if err := json.Unmarshal(canonical, &env); err != nil {
		return fmt.Errorf("%w: envelope", ErrArtifactMismatch)
	}
	if !artifactMessageTypes[env.MessageType] {
		return fmt.Errorf("%w: not a submit artifact type", ErrArtifactMismatch)
	}
	if env.TurnID != turnID {
		return fmt.Errorf("%w: envelope turn id", ErrArtifactMismatch)
	}
	if _, err := protocol.Validate(env.MessageType, canonical); err != nil {
		return fmt.Errorf("%w: schema", ErrArtifactMismatch)
	}
	return nil
}
