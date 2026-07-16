package transport

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/protocol"
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
	// wrong digest, wrong envelope turn id, or schema-invalid).
	ErrArtifactMismatch = errors.New("transport: artifact does not match its key")
)

// ArtifactStore persists immutable, content-addressed artifacts under a confined
// os.Root, so a symlink in the tree cannot escape it. It is the durable
// ArtifactSink the submit path writes to before recording acceptance.
type ArtifactStore struct {
	root *os.Root
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
	return &ArtifactStore{root: r}, nil
}

// Close releases the root handle.
func (s *ArtifactStore) Close() error { return s.root.Close() }

// Put verifies the artifact against its key and publishes it atomically without
// replacing an existing one. A repeat write of the same content is idempotent
// (and durably re-confirmed); a same-key byte mismatch is a collision.
func (s *ArtifactStore) Put(turnID, digest string, canonical []byte) error {
	if err := s.verify(turnID, digest, canonical); err != nil {
		return err
	}
	rel := turnID + "/" + digest + ".json"
	if existing, err := s.read(rel); err == nil {
		if !bytes.Equal(existing, canonical) {
			return fmt.Errorf("%w: %s", ErrArtifactCollision, rel)
		}
		return s.syncDir(turnID) // durably confirm even on an idempotent re-put
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.root.Mkdir(turnID, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return s.publish(turnID, rel, canonical)
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

// verify enforces that the bytes are canonical JSON, hash to the digest, carry an
// envelope turn_id matching the path, and validate against their registered
// schema.
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
	if env.TurnID != turnID {
		return fmt.Errorf("%w: envelope turn id", ErrArtifactMismatch)
	}
	if _, err := protocol.Validate(env.MessageType, canonical); err != nil {
		return fmt.Errorf("%w: schema", ErrArtifactMismatch)
	}
	return nil
}

func (s *ArtifactStore) read(rel string) ([]byte, error) {
	f, err := s.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxArtifactBytes+1))
}

// publish links a fully-written, fsynced temp file into place under the root; a
// concurrent reader sees the complete file or nothing, and two publishers cannot
// overwrite each other.
func (s *ArtifactStore) publish(turnID, rel string, data []byte) (err error) {
	tmp := turnID + "/.claudex-tmp-" + randHex()
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, artifactPerm)
	if err != nil {
		return err
	}
	linked := false
	defer func() {
		if !linked {
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Link(tmp, rel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A concurrent publisher won; reconcile as idempotent or collision.
			existing, rerr := s.read(rel)
			if rerr != nil {
				return rerr
			}
			if !bytes.Equal(existing, data) {
				return fmt.Errorf("%w: %s", ErrArtifactCollision, rel)
			}
			return s.syncDir(turnID)
		}
		return err
	}
	linked = true
	_ = s.root.Remove(tmp)
	return s.syncDir(turnID)
}

// syncDir fsyncs the turn directory so the newly linked entry is durable. On
// Windows there is no directory fsync (see atomicfile), so it is a no-op there.
func (s *ArtifactStore) syncDir(turnID string) error {
	d, err := s.root.Open(turnID)
	if err != nil {
		return err
	}
	serr := syncDirHandle(d)
	_ = d.Close()
	if serr != nil {
		return &atomicfile.PostCommitSyncError{Path: turnID, Err: serr}
	}
	return nil
}

func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
