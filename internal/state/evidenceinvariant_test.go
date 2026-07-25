package state

import (
	"errors"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// The v7 invariant is an IFF in both directions, so every arm is checked: a read-only turn without a
// packet is as invalid as a worktree turn with one, and no assignment means no binding at all.
func TestEvidenceBindingInvariant(t *testing.T) {
	cases := map[string]struct {
		mut  func(rev uint64, n *RunState)
		want string
	}{
		"read-only assignment without a binding": {
			func(rev uint64, n *RunState) {
				n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
			},
			"has no evidence binding",
		},
		"binding with no assignment": {
			func(rev uint64, n *RunState) {
				n.Evidence = testBinding("turn-1", rev)
			},
			"present with no assignment",
		},
		"binding naming another turn": {
			func(rev uint64, n *RunState) {
				n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
				n.Evidence = testBinding("turn-2", rev)
			},
			"names turn",
		},
		"binding at another revision": {
			func(rev uint64, n *RunState) {
				n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
				b := testBinding("turn-1", rev)
				b.IssuedRevision = rev - 1
				n.Evidence = b
			},
			"issued at revision",
		},
		"binding at an undevised path": {
			func(rev uint64, n *RunState) {
				n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
				b := testBinding("turn-1", rev)
				b.ManifestRelPath = "somewhere/else.json"
				n.Evidence = b
			},
			"not the derived packet manifest",
		},
		"binding with a malformed root digest": {
			func(rev uint64, n *RunState) {
				n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
				b := testBinding("turn-1", rev)
				b.RootDigest = "not-a-digest"
				n.Evidence = b
			},
			"not a sha256",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			init := mustInit(t, s)
			_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
				n.Phase = PhasePlanDraft
				n.StartedUnix = n.CreatedUnix
				n.DeadlineUnix = n.CreatedUnix + n.EffectivePolicy.Limits.MaxWallSeconds
				tc.mut(rev, n)
				if n.Assignment != nil {
					n.FirstTurn = &Ref{ID: n.Assignment.ID, IssuedRevision: rev}
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// An IMPLEMENT_STEP/FIX turn carries a mutable worktree, so a packet alongside it would be a second,
// disagreeing statement of what the turn may see.
func TestEditPhaseAssignmentRejectsEvidence(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s)
	_, err := s.Mutate(impl.Revision, func(rev uint64, n *RunState) error {
		n.Assignment = &Ref{ID: "impl-turn", IssuedRevision: rev}
		n.Evidence = testBinding("impl-turn", rev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "carries a worktree") {
		t.Fatalf("err = %v, want a worktree-turn refusal", err)
	}
	// The same assignment WITHOUT a binding is accepted.
	if _, err := s.Mutate(impl.Revision, func(rev uint64, n *RunState) error {
		n.Assignment = &Ref{ID: "impl-turn", IssuedRevision: rev}
		return nil
	}); err != nil {
		t.Fatalf("worktree turn without a binding rejected: %v", err)
	}
}

// The packet cannot be swapped underneath a LIVE turn. Re-pointing a live turn at different review
// material would silently change what was asked for, so it is refused: the assignment is unchanged
// and therefore still carries its original issued revision, which the rebound packet must share —
// and a changed binding must be bound to the RESULTING revision.
func TestEvidenceBindingCannotChangeUnderALiveAssignment(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	draft := issueFirstTurn(t, s, init, "turn-1")

	_, err := s.Mutate(draft.Revision, func(rev uint64, n *RunState) error {
		// The assignment is untouched (same id, same issued revision) but the packet is swapped.
		b := testBinding("turn-1", n.Assignment.IssuedRevision)
		b.RootDigest = testRootDigest("some-other-packet")
		n.Evidence = b
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not the resulting") {
		t.Fatalf("err = %v, want a swap refusal", err)
	}
}

// v7 is a semantic format change, not an additive one: v6 never carried an evidence binding, so a v6
// generation's read-only assignments would silently violate the invariant if they were accepted. A
// v6 document is therefore refused outright, with version remediation rather than a vague error.
func TestDecodeRejectsV6State(t *testing.T) {
	v6 := []byte(`{"schema_version":6,"run_id":"r","revision":2,"accepted_turns":{}}`)
	if _, err := decodeRunState(genstore.Record{Generation: 2, Payload: v6}); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("v6 decode err = %v, want ErrUnsupportedSchema", err)
	}
	if RunStateVersion != 7 {
		t.Fatalf("RunStateVersion = %d, want 7", RunStateVersion)
	}
}
