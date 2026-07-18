package attach

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// CrashFaults injects deterministic faults into a session replacement for CRASH-RECOVERY
// TESTS ONLY. Production is ReplaceAttach (no faults). This exists solely so cross-package
// integration tests can drive persisted crash cases through the SAME replaceAttach authority
// and its real per-call seams — it introduces NO second replacement authority.
type CrashFaults struct {
	// AmbiguousRegistry makes the registry supersession step land ambiguously (a visible-
	// but-unreconcilable append), so the replacement journal is left pending for recovery.
	AmbiguousRegistry bool
	// AmbiguousState makes the activation's run-state (verifier-issuance) step land
	// ambiguously, so the activation is left pending for recovery.
	AmbiguousState bool
}

// ReplaceAttachWithFaults runs the SAME replaceAttach authority as ReplaceAttach with the
// given crash faults injected into its per-call seams. TEST-ONLY: production callers use
// ReplaceAttach.
func ReplaceAttachWithFaults(req ReplaceRequest, faults CrashFaults) (ReplaceResult, error) {
	seams := defaultReplaceSeams()
	if faults.AmbiguousRegistry {
		seams.mutate = ambiguousRegistryMutate()
	}
	if faults.AmbiguousState {
		seams.stateMutate = ambiguousStateMutate()
	}
	return replaceAttach(req, seams)
}

func ambiguousRegistryMutate() registryMutate {
	return func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error) {
		return state.Registry{}, fmt.Errorf("%w: injected registry ambiguity", genstore.ErrAmbiguous)
	}
}

func ambiguousStateMutate() stateMutate {
	return func(*state.Store, *genstore.Guard, uint64, func(uint64, *state.RunState) error) (state.RunState, error) {
		return state.RunState{}, fmt.Errorf("%w: injected state ambiguity", genstore.ErrAmbiguous)
	}
}
