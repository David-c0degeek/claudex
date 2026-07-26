package coordinator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/reviewpacket"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// ErrNotThisSessionsTurn means the run has a live turn, but it belongs to the other role. It is a
// normal, expected answer — a session polls and waits — not a failure of the run.
var ErrNotThisSessionsTurn = errors.New("coordinator: the active turn belongs to the other session")

// ErrSessionSuperseded means this session was replaced; it owns nothing and must not act.
var ErrSessionSuperseded = errors.New("coordinator: session has been superseded")

// Pull projects the assignment a session must act on, over a coherent, aggregate-gated snapshot, and
// mirrors it into that session's inbox.
//
// It MINTS NOTHING and MUTATES NO RUN STATE. Every value comes from persisted state read at one
// revision inside the bracket, so two pulls of the same state produce the same assignment. The turn
// id is whichever the preceding transition issued; pull never creates a turn, and it never
// synthesizes evidence — a read-only turn is actionable only through the packet its issuing
// authority already published, which pull RE-VERIFIES before projecting.
func Pull(repoDir, runID, sessionID string) (transport.Assignment, error) {
	if !state.IsSessionID(sessionID) {
		return transport.Assignment{}, fmt.Errorf("coordinator: session_id is not a canonical minted session id")
	}
	loc, err := attach.ResolveRun(repoDir, runID)
	if err != nil {
		return transport.Assignment{}, err
	}
	a, err := readCoherent(repoDir, runID, func(loc attach.RunLocation) (transport.Assignment, error) {
		return pullFrom(repoDir, loc, sessionID)
	})
	if err != nil {
		return transport.Assignment{}, err
	}
	// The inbox is a rebuildable projection of the assignment, not a second authority: it is written
	// AFTER the bracket because it is an idempotent mirror keyed by session, and a repeated pull of
	// the same state rewrites identical bytes.
	store, err := transport.NewSessionStore(loc.SessionDir)
	if err != nil {
		return transport.Assignment{}, err
	}
	defer store.Close()
	if werr := store.WriteAssignment(sessionID, a); werr != nil {
		return transport.Assignment{}, werr
	}
	return a, nil
}

// pullFrom runs inside the coherent bracket: state, the registry, and the session's view are all
// read from the same gated snapshot, so the role a turn is projected for can never be torn from the
// state that issued it.
func pullFrom(repoDir string, loc attach.RunLocation, sessionID string) (transport.Assignment, error) {
	rs, ok, err := state.Open(loc.StateDir, loc.RunLock).Load()
	if err != nil {
		return transport.Assignment{}, err
	}
	if !ok {
		return transport.Assignment{}, transport.ErrNoRun
	}
	reg, gok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
	if err != nil {
		return transport.Assignment{}, err
	}
	if !gok || reg.RunID != loc.RunID {
		return transport.Assignment{}, fmt.Errorf("coordinator: no durable registration for the run")
	}
	resolved := reg.Resolve(sessionID)
	switch resolved.Status {
	case state.RegUnknown:
		return transport.Assignment{}, fmt.Errorf("%w: session is not registered for the run", transport.ErrSessionView)
	case state.RegReplaced:
		return transport.Assignment{}, ErrSessionSuperseded
	}
	if rs.Assignment == nil {
		return transport.Assignment{}, transport.ErrNoActiveTurn
	}
	view, verr := transport.ResolveSessionView(reg, rs.Phase, rs.Assignment.ID, sessionID)
	if verr != nil {
		return transport.Assignment{}, verr
	}
	if !view.OwnsActiveTurn {
		// The run is healthy and someone is working; this session simply is not the one.
		return transport.Assignment{}, ErrNotThisSessionsTurn
	}

	role := transport.RolePair
	if resolved.Role == state.SlotLead {
		role = transport.RoleLead
	}
	in := transport.PullInputs{
		SessionID: sessionID,
		Role:      role,
		// Binding guidance is the set of human decisions that stay binding for the rest of the run.
		// Human gates are not built yet, so there are none — an empty set is the honest projection,
		// not a placeholder to be filled from something else.
		BindingGuidance:       nil,
		CurrentPairGeneration: view.PairGeneration,
	}
	if transport.EditableTurn(role, rs.Phase) {
		wt := filepath.Join(loc.RunDir, "worktree")
		in.Worktree = &wt
	} else {
		ref, eerr := verifiedEvidence(repoDir, loc, rs)
		if eerr != nil {
			return transport.Assignment{}, eerr
		}
		in.Evidence = &ref
	}
	return transport.BuildAssignment(rs, in)
}

// verifiedEvidence re-verifies the packet run state bound to this turn and projects its locator.
//
// pull NEVER synthesizes evidence: the binding is the issuing authority's, and this only proves the
// packet on disk still is what was bound. The expected identity is RE-DERIVED from authoritative run
// state rather than persisted alongside the binding — for every reachable read-only assignment the
// source is a pure function of state (the latest accepted commit, else the base), state cannot
// advance past the assignment before it is consumed, and RootDigest already commits the manifest's
// own source, so a second stored copy would only add a coherence invariant every issuance and
// recovery writer would have to keep synchronized.
func verifiedEvidence(repoDir string, loc attach.RunLocation, rs state.RunState) (transport.EvidenceRef, error) {
	ev := rs.Evidence
	if ev == nil {
		// Unreachable while the v7 invariant holds; treated as corruption rather than assumed away.
		return transport.EvidenceRef{}, fmt.Errorf("%w: a read-only %s turn has no evidence binding", transport.ErrAssignmentInvalid, rs.Phase)
	}
	src, err := expectedSource(repoDir, rs, loc)
	if err != nil {
		return transport.EvidenceRef{}, err
	}
	lim := rs.EffectivePolicy.Limits
	ref := evidence.EvidenceRef{ManifestRelPath: ev.ManifestRelPath, RootDigest: ev.RootDigest}
	expect := evidence.Expectation{
		RunID:  rs.RunID,
		TurnID: ev.TurnID,
		Phase:  string(rs.Phase),
		Source: src,
	}
	bounds := evidence.Bounds{
		MaxTotalBytes: lim.EvidenceMaxTotalBytes,
		MaxFileBytes:  lim.EvidenceMaxFileBytes,
		MaxRequests:   lim.EvidenceMaxRequests,
	}
	if verr := evidence.VerifyRef(loc.EvidenceDir, ref, bounds, expect); verr != nil {
		return transport.EvidenceRef{}, fmt.Errorf("%w: %v", ErrEvidence, verr)
	}
	return transport.EvidenceRef{ManifestRelPath: ev.ManifestRelPath, RootDigest: ev.RootDigest}, nil
}

// expectedSource re-derives the {commit, tree} the live turn's packet must be cut from, through the
// SAME function the producer used. The tree is read from the object store because run state stores
// only the base COMMIT — deriving it here rather than persisting it is the ruling recorded in the
// review-evidence design: the source is a pure function of state, and a second stored copy would add a coherence
// invariant every issuance and recovery writer would have to keep synchronized.
func expectedSource(repoDir string, rs state.RunState, loc attach.RunLocation) (evidence.SourceObject, error) {
	g, err := gitx.New()
	if err != nil {
		return evidence.SourceObject{}, err
	}
	defer g.Close()
	return reviewpacket.ExpectedSource(context.Background(), reviewpacket.Deps{
		RunDir: loc.RunDir, RepoDir: repoDir, Git: g,
	}, rs)
}
