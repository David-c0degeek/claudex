package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/state"
)

// validArtifact builds a canonical implementation_report for turnID with roughly
// n padding entries, and returns its canonical bytes and digest.
func validArtifact(t *testing.T, turnID string, n int) ([]byte, string) {
	t.Helper()
	files := make([]string, n)
	for i := range files {
		files[i] = fmt.Sprintf(`"pkg/file-%05d.go"`, i)
	}
	body := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":%q,"state_revision":2,"human_context":null,"requires_human_decision":false,"decision_question":null,"files_changed":[%s],"deviations_from_plan":[],"notes":"n"}`,
		turnID, strings.Join(files, ","))
	canon, err := canonjson.Canonicalize([]byte(body))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	sum := sha256.Sum256(canon)
	return canon, hex.EncodeToString(sum[:])
}

func newStore(t *testing.T) *ArtifactStore {
	t.Helper()
	s, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestArtifactStorePutGet(t *testing.T) {
	s := newStore(t)
	body, d := validArtifact(t, "turn-1", 1)
	if err := s.Put("turn-1", d, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get("turn-1", d)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get mismatch err=%v", err)
	}
	if err := s.Put("turn-1", d, body); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
}

func TestArtifactStoreRejects(t *testing.T) {
	s := newStore(t)
	body, d := validArtifact(t, "turn-1", 1)
	// Bad key shapes.
	for _, bad := range []string{"../escape", "a/b", "a\\b", "..", ""} {
		if err := s.Put(bad, d, body); !errors.Is(err, ErrBadArtifactKey) {
			t.Fatalf("turn id %q err = %v, want ErrBadArtifactKey", bad, err)
		}
	}
	if err := s.Put("turn-1", strings.Repeat("Z", 64), body); !errors.Is(err, ErrBadArtifactKey) {
		t.Fatalf("non-hex digest want ErrBadArtifactKey, got err")
	}
	// Digest doesn't match the bytes.
	if err := s.Put("turn-1", strings.Repeat("a", 64), body); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("wrong digest err = %v, want ErrArtifactMismatch", err)
	}
	// Envelope turn_id disagrees with the path.
	other, od := validArtifact(t, "turn-OTHER", 1)
	if err := s.Put("turn-1", od, other); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("envelope turn mismatch err = %v, want ErrArtifactMismatch", err)
	}
	// Non-canonical / non-schema bytes.
	if err := s.Put("turn-1", digestOfBytes([]byte(`{"x":1}`)), []byte(`{"x":1}`)); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("non-schema bytes err = %v, want ErrArtifactMismatch", err)
	}
}

func TestArtifactStoreCollision(t *testing.T) {
	s := newStore(t)
	a, d := validArtifact(t, "turn-1", 1)
	if err := s.Put("turn-1", d, a); err != nil {
		t.Fatalf("put a: %v", err)
	}
	// Same key, different bytes: impossible via a real digest, but a caller could
	// present mismatched (turn,digest,bytes). Craft bytes that hash to d but
	// differ — use the stored digest with different content via a forced write.
	// Simpler: a different valid artifact keyed under the same (turn, digest).
	b, _ := validArtifact(t, "turn-1", 2)
	if err := s.Put("turn-1", d, b); !errors.Is(err, ErrArtifactMismatch) {
		// b's bytes don't hash to d, so verify rejects before publication.
		t.Fatalf("mismatched-content put err = %v, want ErrArtifactMismatch", err)
	}
}

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A concurrent reader never observes a torn artifact: a Get is either not-found
// or the complete, verified bytes. On Windows, transient sharing errors are
// bounded-retried by the store's confined reads.
func TestArtifactStoreConcurrentReadsNeverTorn(t *testing.T) {
	s := newStore(t)
	body, d := validArtifact(t, "turn-1", 4000) // a large valid artifact

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := s.Get("turn-1", d)
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						t.Errorf("reader saw a non-notfound error: %v", err)
						return
					}
					continue
				}
				if !bytes.Equal(got, body) {
					t.Errorf("reader observed a torn or wrong artifact")
					return
				}
			}
		}()
	}
	if err := s.Put("turn-1", d, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := s.Get("turn-1", d); err != nil {
			t.Fatalf("post-write get: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// The real ArtifactStore is the submit sink end-to-end: a Submit persists the
// redacted canonical artifact, which the store then verifies and returns.
func TestArtifactStoreAsSubmitSink(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newStore(t)
	adv := func(_ PreparedSubmit, gen uint64, next *state.RunState) error {
		next.Phase = state.PhaseCheckpoint
		next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		return nil
	}
	res, err := Submit(context.Background(), store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	got, err := sink.Get("turn-1", res.Receipt.ArtifactDigest)
	if err != nil {
		t.Fatalf("get persisted artifact: %v", err)
	}
	if digestOfBytes(got) != res.Receipt.ArtifactDigest {
		t.Fatalf("persisted artifact digest disagrees with the receipt")
	}
}

// Concurrent identical publishers yield exactly one artifact, no error.
func TestArtifactStoreConcurrentSamePut(t *testing.T) {
	s := newStore(t)
	body, d := validArtifact(t, "turn-1", 10)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = s.Put("turn-1", d, body) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publisher %d: %v", i, err)
		}
	}
	got, err := s.Get("turn-1", d)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("final get err=%v", err)
	}
}
