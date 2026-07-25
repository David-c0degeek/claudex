package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/state"
)

// syncFailSink models a sink that committed the visible bytes but could not
// durably sync them (a *PostCommitSyncError).
type syncFailSink struct{ calls int }

func (s *syncFailSink) Put(turnID, digest string, _ []byte) error {
	s.calls++
	return &atomicfile.PostCommitSyncError{Path: turnID + "/" + digest, Err: errors.New("dir sync failed")}
}

// countingSink records how many times Put was called and always succeeds.
type countingSink struct{ calls int }

func (s *countingSink) Put(_, _ string, _ []byte) error { s.calls++; return nil }

// A run that is not live (terminal, or recovering) refuses a submit BEFORE the
// sink is touched, so terminal/recovery state never writes an orphan artifact.
func TestSubmitRefusesNonLiveRunBeforeSink(t *testing.T) {
	adv := checkpointPrep()

	t.Run("cancelled", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		rc, err := store.Mutate(rev, func(_ uint64, next *state.RunState) error {
			next.Lifecycle = state.LifecycleCancelled // assignment lingers; run is not live
			return nil
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		sink := &countingSink{}
		_, err = submit(store, sink, "sess-2", report("turn-1", rc.Revision, "x"), ownerAuth("sess-2"), adv)
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("cancelled submit err = %v, want ErrNotAccepting", err)
		}
		if sink.calls != 0 {
			t.Fatalf("sink was called %d times on a cancelled run", sink.calls)
		}
	})

	t.Run("recovering", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		rr, err := store.Mutate(rev, func(gen uint64, next *state.RunState) error {
			next.Recovery = &state.Projection{Code: "resync", Reason: "reattached", NextAction: "await", AtRevision: gen}
			return nil
		})
		if err != nil {
			t.Fatalf("recovery: %v", err)
		}
		sink := &countingSink{}
		_, err = submit(store, sink, "sess-2", report("turn-1", rr.Revision, "x"), ownerAuth("sess-2"), adv)
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("recovering submit err = %v, want ErrNotAccepting", err)
		}
		if sink.calls != 0 {
			t.Fatalf("sink was called %d times on a recovering run", sink.calls)
		}
	})

	// A stale assignment (still present, but issued at an older revision after an
	// unrelated advance) cannot be turned into an acceptance.
	t.Run("stale assignment", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		r2, err := store.Mutate(rev, func(_ uint64, next *state.RunState) error {
			next.Counters.PlanRevisions++ // advance, leaving turn-1 assigned but stale
			return nil
		})
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		sink := &countingSink{}
		_, err = submit(store, sink, "sess-2", report("turn-1", r2.Revision, "x"), ownerAuth("sess-2"), adv)
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("stale-assignment submit err = %v, want ErrNotAccepting", err)
		}
		if sink.calls != 0 {
			t.Fatalf("sink was called %d times on a stale assignment", sink.calls)
		}
	})

	// A valid cancelled shape clears the assignment; ErrNotAccepting must still win
	// over ErrNoActiveTurn (the moved-up order), and the sink stays untouched.
	t.Run("cancelled with cleared assignment", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		rc, err := store.Mutate(rev, func(_ uint64, next *state.RunState) error {
			next.Lifecycle = state.LifecycleCancelled
			next.Assignment = nil
			next.Evidence = nil // the binding is consumed with the turn it authorized
			return nil
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		sink := &countingSink{}
		_, err = submit(store, sink, "sess-2", report("turn-1", rc.Revision, "x"), ownerAuth("sess-2"), adv)
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("cancelled-cleared submit err = %v, want ErrNotAccepting", err)
		}
		if sink.calls != 0 {
			t.Fatalf("sink was called %d times on a cancelled run", sink.calls)
		}
	})

	// A proper paused human gate (AWAIT_GUIDANCE, assignment cleared) also returns
	// ErrNotAccepting before the phase's absent turn spec could surface.
	t.Run("paused gate", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		rg, err := store.Mutate(rev, func(gen uint64, next *state.RunState) error {
			acceptAndHumanGate(next, gen, state.PhaseCheckpoint, "turn-1", dig("7"), "gate-1")
			return nil
		})
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		sink := &countingSink{}
		// turn-1 is the accepted gate source; submit a fresh turn so the not-accepting
		// refusal (not an already-accepted replay) is what surfaces.
		_, err = submit(store, sink, "sess-2", report("turn-2", rg.Revision, "x"), ownerAuth("sess-2"), adv)
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("paused-gate submit err = %v, want ErrNotAccepting", err)
		}
		if sink.calls != 0 {
			t.Fatalf("sink was called %d times on a paused gate", sink.calls)
		}
	})
}

// A sink that cannot durably persist the artifact must block acceptance: Submit
// surfaces the post-commit sync error and does NOT advance the run, so a later
// retry (against a healthy sink) re-confirms durability before accepting.
func TestSubmitDoesNotAdvanceOnSinkSyncFailure(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := &syncFailSink{}
	adv := checkpointPrep()
	_, err := submit(store, sink, "sess-2", report("turn-1", rev, "did it"), ownerAuth("sess-2"), adv)
	var pce *atomicfile.PostCommitSyncError
	if !errors.As(err, &pce) {
		t.Fatalf("submit err = %v, want *atomicfile.PostCommitSyncError", err)
	}
	rs, _, _ := store.Load()
	if rs.Revision != rev {
		t.Fatalf("state advanced to rev %d despite the sink sync failure", rs.Revision)
	}
	if _, ok := rs.AcceptedTurns["turn-1"]; ok {
		t.Fatalf("acceptance was recorded despite the sink sync failure")
	}
}

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

func newStoreDir(t *testing.T) (*ArtifactStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewArtifactStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func newStore(t *testing.T) *ArtifactStore {
	t.Helper()
	s, _ := newStoreDir(t)
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

// A same-key write whose bytes differ from an already-published artifact is a
// collision. verify() passes (the supplied bytes hash to d), so the no-clobber
// link fails and the store reads the existing, different bytes — reaching the
// collision branch without replacing anything.
func TestArtifactStoreCollisionReachesHandling(t *testing.T) {
	s, dir := newStoreDir(t)
	a, d := validArtifact(t, "turn-1", 1)
	// Pre-place different bytes at the exact target path via a side channel.
	if err := os.MkdirAll(filepath.Join(dir, "turn-1"), 0o700); err != nil {
		t.Fatalf("pre-mkdir: %v", err)
	}
	target := filepath.Join(dir, "turn-1", d+".json")
	if err := os.WriteFile(target, []byte("different-prior-bytes"), 0o600); err != nil {
		t.Fatalf("pre-place: %v", err)
	}
	// a hashes to d and verifies, but the target already holds other bytes.
	if err := s.Put("turn-1", d, a); !errors.Is(err, ErrArtifactCollision) {
		t.Fatalf("collision put err = %v, want ErrArtifactCollision", err)
	}
	// The prior file was not replaced.
	got, _ := os.ReadFile(target)
	if string(got) != "different-prior-bytes" {
		t.Fatalf("collision replaced the existing artifact: %q", got)
	}
}

// A canonical, correctly-hashed message that is NOT one of the submit artifact
// types is refused — only a real assignment-produced artifact may be stored.
func TestArtifactStoreRejectsNonArtifactType(t *testing.T) {
	s := newStore(t)
	body, d := canonArtifact(t, `{"message_type":"assignment","turn_id":"turn-1"}`)
	if err := s.Put("turn-1", d, body); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("non-artifact type err = %v, want ErrArtifactMismatch", err)
	}
}

// Persistence-boundary redaction: bytes carrying a secret in a free-text field
// are not already redacted, so the store refuses them rather than rewriting
// (which would change the digest).
func TestArtifactStoreRejectsUnredacted(t *testing.T) {
	s := newStore(t)
	secret := "sk-ant-" + "abcdefghijklmnopqrstuvwx"
	body, d := canonArtifact(t, `{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":2,"human_context":null,"requires_human_decision":false,"decision_question":null,"files_changed":[],"deviations_from_plan":[],"notes":"`+secret+`"}`)
	if err := s.Put("turn-1", d, body); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("unredacted put err = %v, want ErrArtifactMismatch", err)
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
	adv := checkpointPrep()
	res, err := submit(store, sink, "sess-2", report("turn-1", rev, "did it"), ownerAuth("sess-2"), adv)
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
