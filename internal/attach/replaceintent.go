package attach

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// replaceIntentKind is the txn kind for an explicit session replacement. It is a
// DEDICATED journal, separate from the pair-attach journal, whose classifier is
// intentionally pair-attach-specific.
const replaceIntentKind = "session-replace"

func (l layout) replaceJournalDir(runDir string) string { return filepath.Join(runDir, "replace") }

// registryMutate is the Registry compare-and-swap the replacement step applies.
// Production uses (*state.RegistryStore).MutateLocked; tests inject an ambiguous or
// failing append without any global state.
type registryMutate func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error)

// ReplaceIntent is the frozen, bounded target of an explicit same-role session
// replacement, recorded read-only under the run guard. A retry with the same
// operation id never re-mints or reinterprets it. In 4c-1 the only durable effect is
// the Registry supersession; the RunState-activation step (a qualifying pair
// replacement at ownerless VERIFY) is added in the next slice.
type ReplaceIntent struct {
	RunID                    string         `json:"run_id"`
	TxnID                    string         `json:"txn_id"`
	OperationID              string         `json:"operation_id"`
	Role                     state.SlotRole `json:"role"`
	Agent                    state.Agent    `json:"agent"`
	SupersededGeneration     uint64         `json:"superseded_generation"` // the slot generation being replaced
	NewSessionID             string         `json:"new_session_id"`
	NewGeneration            uint64         `json:"new_generation"`             // superseded_generation + 1
	ExpectedRegistryRevision uint64         `json:"expected_registry_revision"` // the Registry revision the append CASes against
}

func (in ReplaceIntent) marshal() (json.RawMessage, error) { return json.Marshal(in) }

func mustMarshalReplace(in ReplaceIntent) []byte {
	b, _ := in.marshal()
	return b
}

func decodeReplaceIntent(payload json.RawMessage) (ReplaceIntent, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var in ReplaceIntent
	if err := dec.Decode(&in); err != nil {
		return ReplaceIntent{}, fmt.Errorf("attach: decode replace intent: %w", err)
	}
	if err := in.validate(); err != nil {
		return ReplaceIntent{}, err
	}
	return in, nil
}

func (in ReplaceIntent) validate() error {
	if !state.IsRunID(in.RunID) || !state.IsRunID(in.TxnID) {
		return fmt.Errorf("attach: replace intent run/txn id is not canonical")
	}
	if !state.IsOperationID(in.OperationID) {
		return fmt.Errorf("attach: replace intent operation_id is not a minted operation id")
	}
	if in.Role != state.SlotLead && in.Role != state.SlotPair {
		return fmt.Errorf("attach: replace intent role must be lead or pair")
	}
	if in.Agent != state.AgentClaude && in.Agent != state.AgentCodex {
		return fmt.Errorf("attach: replace intent agent is unknown")
	}
	if !state.IsSessionID(in.NewSessionID) {
		return fmt.Errorf("attach: replace intent new_session_id is not a canonical minted id")
	}
	if in.SupersededGeneration == 0 {
		return fmt.Errorf("attach: replace intent superseded_generation must be > 0")
	}
	if in.NewGeneration != in.SupersededGeneration+1 {
		return fmt.Errorf("attach: replace intent new_generation must be superseded_generation + 1")
	}
	if in.ExpectedRegistryRevision == 0 {
		return fmt.Errorf("attach: replace intent expected_registry_revision must be > 0")
	}
	return nil
}

// txnIntent projects the frozen replace intent into the generic txn envelope. A
// replacement does not bind a RunState revision in 4c-1, so ExpectedStateRevision is 0.
func (in ReplaceIntent) txnIntent() txn.Intent {
	return txn.Intent{
		Version:               txn.IntentVersion,
		Kind:                  replaceIntentKind,
		TxnID:                 in.TxnID,
		ExpectedStateRevision: 0,
		Payload:               mustMarshalReplace(in),
	}
}

// replacePlanFor builds the ordinary replacement plan: one idempotent, observable
// Registry step that supersedes the slot's current session with the frozen new
// session. mutate is the injected Registry mutation (production uses MutateLocked;
// tests inject an ambiguous append). The Status observes durable state — never the
// callback — so a crash after the append but before the journal advance repairs
// forward.
func replacePlanFor(registry *state.RegistryStore, runGuard *genstore.Guard, in ReplaceIntent, mutate registryMutate) (txn.Plan, error) {
	if err := in.validate(); err != nil {
		return txn.Plan{}, err
	}
	step := txn.Step{
		Name:   "registry-replace",
		Status: func() (txn.StepStatus, error) { return replaceStepStatus(registry, in) },
		Apply: func() error {
			_, err := mutate(registry, runGuard, in.ExpectedRegistryRevision, func(nextRev uint64, next *state.Registry) error {
				target := regSlot(*next, in.Role)
				if target == nil {
					return fmt.Errorf("attach: replacement slot vanished under the guard")
				}
				// The pre-state must be exactly the superseded generation and the new
				// session must not already be present, so a drifted store never double-appends.
				if uint64(len(target.Sessions)) != in.SupersededGeneration || target.CurrentSessionID == in.NewSessionID {
					return fmt.Errorf("attach: replacement pre-state drifted under the guard")
				}
				target.Sessions = append(target.Sessions, state.SessionRecord{
					SessionID: in.NewSessionID, Generation: in.NewGeneration, IssuedRegistryRevision: nextRev,
				})
				target.CurrentSessionID = in.NewSessionID
				return nil
			})
			return err
		},
	}
	return txn.Plan{Intent: in.txnIntent(), Steps: []txn.Step{step}}, nil
}

// replaceStepStatus observes whether the Registry supersession is durably applied.
func replaceStepStatus(registry *state.RegistryStore, in ReplaceIntent) (txn.StepStatus, error) {
	reg, ok, err := registry.Load()
	if err != nil {
		return "", err
	}
	if !ok || reg.RunID != in.RunID {
		return txn.StatusIndeterminate, nil
	}
	slot := regSlot(reg, in.Role)
	if slot == nil {
		return txn.StatusIndeterminate, nil
	}
	r := reg.Resolve(in.NewSessionID)
	switch {
	case r.Status == state.RegCurrent && r.Role == in.Role && r.Agent == in.Agent &&
		slot.CurrentSessionID == in.NewSessionID && uint64(len(slot.Sessions)) == in.NewGeneration:
		return txn.StatusApplied, nil
	case r.Status == state.RegUnknown && slot.CurrentSessionID != in.NewSessionID &&
		slot.Agent == in.Agent && uint64(len(slot.Sessions)) == in.SupersededGeneration && reg.Revision == in.ExpectedRegistryRevision:
		return txn.StatusNotApplied, nil
	default:
		return txn.StatusIndeterminate, nil
	}
}
