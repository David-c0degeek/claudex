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

// stateMutate is the RunState compare-and-swap the activation step applies. Production
// uses (*state.Store).MutateLocked; tests inject a crash cut on the second store.
type stateMutate func(*state.Store, *genstore.Guard, uint64, func(uint64, *state.RunState) error) (state.RunState, error)

// ReplaceActivation is the optional SECOND effect of a replacement: a qualifying pair
// replacement at a running ownerless VERIFY issues the verifier assignment as a RunState
// transition. Its presence is an exact XOR with the ordinary Registry-only replacement.
// It freezes the locked RunState decision — the exact ownerless-VERIFY baseline (a full
// canonical digest, so collateral mutation cannot be blessed), the state revision the
// activation CASes against, the pre-minted verifier turn id, and the retained threshold.
type ReplaceActivation struct {
	ExpectedStateRevision uint64 `json:"expected_state_revision"`
	StateBaselineDigest   string `json:"state_baseline_digest"` // canonDigest of the frozen ownerless VERIFY RunState
	VerifierTurnID        string `json:"verifier_turn_id"`
	RequiredGeneration    uint64 `json:"required_generation"` // the retained threshold; NewGeneration >= this
}

// ReplaceIntent is the frozen, bounded target of an explicit same-role session
// replacement, recorded read-only under the run guard. A retry with the same operation
// id never re-mints or reinterprets it. Ordinary replacements carry only the Registry
// supersession; an Activation adds the exact ownerless-VERIFY verifier issuance.
type ReplaceIntent struct {
	RunID                    string             `json:"run_id"`
	TxnID                    string             `json:"txn_id"`
	OperationID              string             `json:"operation_id"`
	Role                     state.SlotRole     `json:"role"`
	Agent                    state.Agent        `json:"agent"`
	SupersededGeneration     uint64             `json:"superseded_generation"` // the slot generation being replaced
	NewSessionID             string             `json:"new_session_id"`
	NewGeneration            uint64             `json:"new_generation"`             // superseded_generation + 1
	ExpectedRegistryRevision uint64             `json:"expected_registry_revision"` // the Registry revision the append CASes against
	Activation               *ReplaceActivation `json:"activation,omitempty"`
}

// activated reports whether this replacement issues the ownerless-VERIFY verifier turn.
func (in ReplaceIntent) activated() bool { return in.Activation != nil }

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
	if in.SupersededGeneration == 0 || in.SupersededGeneration == ^uint64(0) {
		return fmt.Errorf("attach: replace intent superseded_generation is out of range")
	}
	if in.NewGeneration == 0 || in.NewGeneration != in.SupersededGeneration+1 {
		return fmt.Errorf("attach: replace intent new_generation must be superseded_generation + 1")
	}
	if in.ExpectedRegistryRevision == 0 {
		return fmt.Errorf("attach: replace intent expected_registry_revision must be > 0")
	}
	if a := in.Activation; a != nil {
		// Activation is a qualifying PAIR replacement only, with the fresh generation at
		// or past the retained threshold and every activation field present (an exact XOR
		// with the ordinary replacement).
		if in.Role != state.SlotPair {
			return fmt.Errorf("attach: only a pair replacement may activate VERIFY")
		}
		if a.ExpectedStateRevision == 0 {
			return fmt.Errorf("attach: activation expected_state_revision must be > 0")
		}
		if !state.IsHex64(a.StateBaselineDigest) {
			return fmt.Errorf("attach: activation state_baseline_digest is not a sha256")
		}
		if !state.IsRunID(a.VerifierTurnID) {
			return fmt.Errorf("attach: activation verifier_turn_id is not a canonical minted id")
		}
		if a.RequiredGeneration == 0 || in.NewGeneration < a.RequiredGeneration {
			return fmt.Errorf("attach: activation new_generation does not meet the retained threshold")
		}
	}
	return nil
}

// txnIntent projects the frozen replace intent into the generic txn envelope. An
// ordinary replacement binds no RunState revision (ExpectedStateRevision == 0); an
// activation binds the exact frozen state revision.
func (in ReplaceIntent) txnIntent() txn.Intent {
	var stateRev uint64
	if in.activated() {
		stateRev = in.Activation.ExpectedStateRevision
	}
	return txn.Intent{
		Version:               txn.IntentVersion,
		Kind:                  replaceIntentKind,
		TxnID:                 in.TxnID,
		ExpectedStateRevision: stateRev,
		Payload:               mustMarshalReplace(in),
	}
}

const (
	stepRegistryReplace = "registry-replace"
	stepVerifyActivate  = "verify-activate"
)

// stepNames is the exact ordered plan step list for this intent: one Registry step for
// an ordinary replacement, plus the verifier-issuance step for an activation. A forged
// step list is rejected before a head is trusted or stepped over.
func (in ReplaceIntent) stepNames() []string {
	if in.activated() {
		return []string{stepRegistryReplace, stepVerifyActivate}
	}
	return []string{stepRegistryReplace}
}

// bindReplaceHead binds a present replacement-journal head to the exact replacement
// envelope, payload, run, and plan step list, returning the frozen intent. It does NOT
// judge terminality or effect presence (the caller does), but a mis-bound head — wrong
// kind/version/txn/state-revision/run, or a forged step list — can never be trusted or
// silently stepped over by a new operation.
func bindReplaceHead(head txn.Record, runID string) (ReplaceIntent, error) {
	if head.Intent.Version != txn.IntentVersion || head.Intent.Kind != replaceIntentKind {
		return ReplaceIntent{}, fmt.Errorf("attach: replacement head kind/version mismatch")
	}
	in, err := decodeReplaceIntent(head.Intent.Payload)
	if err != nil {
		return ReplaceIntent{}, err
	}
	// The envelope's expected state revision is reconstructed from the activation fields
	// (0 for an ordinary replacement, the frozen state revision for an activation), so a
	// forged envelope that disagrees with its own payload fails closed.
	if head.TxnID() != in.TxnID || head.Intent.ExpectedStateRevision != in.txnIntent().ExpectedStateRevision || in.RunID != runID {
		return ReplaceIntent{}, fmt.Errorf("attach: replacement head identity mismatch")
	}
	want := in.stepNames()
	if len(head.StepIDs) != len(want) {
		return ReplaceIntent{}, fmt.Errorf("attach: replacement head step list mismatch")
	}
	for i := range want {
		if head.StepIDs[i] != want[i] {
			return ReplaceIntent{}, fmt.Errorf("attach: replacement head step list mismatch")
		}
	}
	return in, nil
}

// replaceSeamSet carries the injected store mutations both plan steps apply. Production
// uses the real MutateLocked; tests inject ambiguous/failing appends without global state.
type replaceSeamSet struct {
	reg   registryMutate
	state stateMutate
}

// replacePlanFor builds the replacement plan: one idempotent, observable Registry step
// that supersedes the slot's current session, plus — for an activation — a second
// RunState step that issues the verifier assignment. Every Status observes durable state
// (never the callback), so any crash cut repairs forward. Both steps run under the SAME
// run guard in order.
func replacePlanFor(registry *state.RegistryStore, runState *state.Store, runGuard *genstore.Guard, in ReplaceIntent, seams replaceSeamSet) (txn.Plan, error) {
	if err := in.validate(); err != nil {
		return txn.Plan{}, err
	}
	regStep := txn.Step{
		Name:   stepRegistryReplace,
		Status: func() (txn.StepStatus, error) { return replaceStepStatus(registry, in) },
		Apply: func() error {
			_, err := seams.reg(registry, runGuard, in.ExpectedRegistryRevision, func(nextRev uint64, next *state.Registry) error {
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
	if !in.activated() {
		return txn.Plan{Intent: in.txnIntent(), Steps: []txn.Step{regStep}}, nil
	}
	a := in.Activation
	actStep := txn.Step{
		Name:   stepVerifyActivate,
		Status: func() (txn.StepStatus, error) { return activateStepStatus(runState, in) },
		Apply: func() error {
			_, err := seams.state(runState, runGuard, a.ExpectedStateRevision, func(nextRev uint64, next *state.RunState) error {
				return applyActivation(next, a, nextRev)
			})
			return err
		},
	}
	return txn.Plan{Intent: in.txnIntent(), Steps: []txn.Step{regStep, actStep}}, nil
}

// applyActivation issues the verifier assignment onto the frozen ownerless VERIFY
// pre-state: the pre-state must be the EXACT frozen baseline (a full canonical digest,
// so no collateral field may have drifted), and the only change is the verifier
// assignment bound to the appended revision.
func applyActivation(next *state.RunState, a *ReplaceActivation, nextRev uint64) error {
	if next.Assignment != nil {
		return fmt.Errorf("attach: activation pre-state is not ownerless")
	}
	dig, derr := canonDigest(next)
	if derr != nil {
		return derr
	}
	if dig != a.StateBaselineDigest {
		return fmt.Errorf("attach: activation pre-state drifted from the frozen baseline")
	}
	next.Assignment = &state.Ref{ID: a.VerifierTurnID, IssuedRevision: nextRev}
	return nil
}

// activationLineageOK reports whether a completed activation's RunState effect is still
// intact enough that a LATER replacement may step over it: either the verifier
// assignment is still current, or its VERIFY turn was accepted (a legal verifier submit
// consumed it and advanced the run). Neither means the frozen effect vanished before any
// legal descendant — recovery-required. This deliberately does NOT require the transient
// assignment to persist forever, so a valid post-activation transition is not false
// recovery.
func activationLineageOK(runState *state.Store, in ReplaceIntent) (bool, error) {
	a := in.Activation
	rs, ok, err := runState.Load()
	if err != nil {
		return false, err
	}
	if !ok || rs.RunID != in.RunID {
		return false, nil
	}
	if rs.Assignment != nil && rs.Assignment.ID == a.VerifierTurnID {
		return true, nil
	}
	if _, accepted := rs.AcceptedTurns[a.VerifierTurnID]; accepted {
		return true, nil
	}
	return false, nil
}

// isOwnerlessVerify reports the exact running ownerless VERIFY shape (no assignment, a
// nonzero retained threshold) a pair activation targets.
func isOwnerlessVerify(rs state.RunState) bool {
	return rs.Phase == state.PhaseVerify && rs.Lifecycle == state.LifecycleRunning &&
		rs.Assignment == nil && rs.Verify != nil && rs.Verify.RequiredGeneration != 0
}

// activateStepStatus observes whether the verifier issuance is durably applied.
func activateStepStatus(runState *state.Store, in ReplaceIntent) (txn.StepStatus, error) {
	rs, ok, err := runState.Load()
	if err != nil {
		return "", err
	}
	return classifyActivation(rs, ok, in)
}

// classifyActivation is the pure participant authority. The baseline digest covers the
// WHOLE ownerless state (revision reset to the frozen expected revision, assignment
// cleared), so a recovered applied prefix cannot bless a collateral mutation of the
// ledger, counters, or the requirement: any such drift is Indeterminate, never Applied.
func classifyActivation(rs state.RunState, ok bool, in ReplaceIntent) (txn.StepStatus, error) {
	a := in.Activation
	if !ok || rs.RunID != in.RunID {
		return txn.StatusIndeterminate, nil
	}
	// NotApplied: the exact frozen ownerless baseline is still in place.
	if rs.Revision == a.ExpectedStateRevision && rs.Assignment == nil {
		if dig, derr := canonDigest(rs); derr != nil {
			return "", derr
		} else if dig == a.StateBaselineDigest {
			return txn.StatusNotApplied, nil
		}
		return txn.StatusIndeterminate, nil
	}
	// Applied: the appended revision with exactly the verifier assignment, and the rest
	// of the state normalizes back to the frozen baseline.
	if rs.Revision == a.ExpectedStateRevision+1 && rs.Phase == state.PhaseVerify &&
		rs.Assignment != nil && rs.Assignment.ID == a.VerifierTurnID && rs.Assignment.IssuedRevision == rs.Revision &&
		rs.Verify != nil && rs.Verify.RequiredGeneration == a.RequiredGeneration {
		norm := rs
		norm.Assignment = nil
		norm.Revision = a.ExpectedStateRevision
		if dig, derr := canonDigest(norm); derr != nil {
			return "", derr
		} else if dig == a.StateBaselineDigest {
			return txn.StatusApplied, nil
		}
	}
	return txn.StatusIndeterminate, nil
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
