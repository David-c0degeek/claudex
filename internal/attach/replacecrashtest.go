package attach

import (
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ReplaceStores is the store-mutation dependency surface of a session replacement. A nil
// field uses the production store mutator (MutateLocked); a non-nil field injects a wrapper.
// This is ordinary dependency injection — the SAME seam replaceAttach uses internally — not a
// fault-specific API: any fault behavior lives in the injected wrapper (test code), and
// ReplaceAttach remains the production entry (all fields nil).
type ReplaceStores struct {
	// RegistryMutate replaces the registry supersession mutator.
	RegistryMutate func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error)
	// StateMutate replaces the activation run-state mutator.
	StateMutate func(*state.Store, *genstore.Guard, uint64, func(uint64, *state.RunState) error) (state.RunState, error)
}

// ReplaceAttachWith runs the SAME replaceAttach authority as ReplaceAttach with the given
// store mutators injected (dependency injection for crash-recovery tests). ReplaceAttach is
// the production entry (real mutators).
func ReplaceAttachWith(req ReplaceRequest, stores ReplaceStores) (ReplaceResult, error) {
	seams := defaultReplaceSeams()
	if stores.RegistryMutate != nil {
		seams.mutate = stores.RegistryMutate
	}
	if stores.StateMutate != nil {
		seams.stateMutate = stores.StateMutate
	}
	return replaceAttach(req, seams)
}
