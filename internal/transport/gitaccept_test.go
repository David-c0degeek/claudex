package transport

import (
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ObserveGitAccept reports Applied ONLY for the exact frozen acceptance — the exact
// accepted turn, git evidence, resulting engine effects, and issued identities. A
// valid-looking foreign append that reused the tuple with a different outcome or
// assignment is Foreign, so the transaction journal can never terminalize over it.
func TestObserveGitAcceptExactness(t *testing.T) {
	ev := state.GitCommitEvidence{
		Parent: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Commit: strings.Repeat("c", 40),
	}
	plan := GitAcceptPlan{
		RunID: "run-a", ExpectedStateRevision: 7, TurnID: "turn-1", Digest: dig("4"),
		Phase:        state.PhaseImplementStep,
		Decision:     engine.Decision{Next: state.PhaseCheckpoint},
		IssuedTurnID: "turn-2",
		GitCommit:    ev,
	}
	applied := func() state.RunState {
		e := ev
		return state.RunState{
			Revision: 8, Phase: state.PhaseCheckpoint, Lifecycle: state.LifecycleRunning,
			Assignment: &state.Ref{ID: "turn-2", IssuedRevision: 8},
			AcceptedTurns: map[string]state.AcceptedTurn{
				"turn-1": {
					ArtifactDigest: dig("4"),
					Receipt:        state.Receipt{TurnID: "turn-1", Revision: 8, ArtifactDigest: dig("4")},
					Phase:          state.PhaseImplementStep,
					GitCommit:      &e,
				},
			},
		}
	}

	if got := ObserveGitAccept(applied(), plan); got != GitAcceptApplied {
		t.Fatalf("exact acceptance = %v, want Applied", got)
	}

	foreign := map[string]func(rs *state.RunState){
		"different next assignment": func(rs *state.RunState) {
			rs.Assignment = &state.Ref{ID: "turn-x", IssuedRevision: 8}
		},
		"missing issued assignment": func(rs *state.RunState) { rs.Assignment = nil },
		"different resulting phase": func(rs *state.RunState) { rs.Phase = state.PhaseTests },
		"unissued gate present": func(rs *state.RunState) {
			rs.Gate = &state.Ref{ID: "gate-x", IssuedRevision: 8}
		},
		"different accepted phase": func(rs *state.RunState) {
			acc := rs.AcceptedTurns["turn-1"]
			acc.Phase = state.PhaseFix
			rs.AcceptedTurns["turn-1"] = acc
		},
		"different git evidence": func(rs *state.RunState) {
			e := ev
			e.Commit = strings.Repeat("d", 40)
			acc := rs.AcceptedTurns["turn-1"]
			acc.GitCommit = &e
			rs.AcceptedTurns["turn-1"] = acc
		},
		"different digest": func(rs *state.RunState) {
			acc := rs.AcceptedTurns["turn-1"]
			acc.ArtifactDigest = dig("5")
			rs.AcceptedTurns["turn-1"] = acc
		},
		"receipt not the head revision": func(rs *state.RunState) { rs.Revision = 9 },
	}
	for name, mutate := range foreign {
		t.Run(name, func(t *testing.T) {
			rs := applied()
			mutate(&rs)
			if got := ObserveGitAccept(rs, plan); got != GitAcceptForeign {
				t.Fatalf("%s = %v, want Foreign", name, got)
			}
		})
	}
}
