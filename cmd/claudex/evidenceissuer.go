package main

import (
	"context"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/reviewpacket"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// newEvidenceIssuer builds the review-packet issuer for a run and returns it with its cleanup. The
// attach protocol takes it as a seam, so the Git handle is owned here, at the process edge, and
// released on every path.
func newEvidenceIssuer(repoDir, runID string) (attach.EvidenceIssuer, func(), error) {
	loc, err := attach.ResolveRun(repoDir, runID)
	if err != nil {
		return nil, func() {}, err
	}
	g, err := gitx.New()
	if err != nil {
		return nil, func() {}, fmt.Errorf("review evidence: %w", err)
	}
	// A review turn past PLAN_DRAFT materializes accepted artifacts (the plan under critique, the
	// agreed plan, the implementation report), so the packet resolver needs the run's artifact store.
	store, err := transport.NewArtifactStore(loc.ArtifactsDir)
	if err != nil {
		g.Close()
		return nil, func() {}, fmt.Errorf("review evidence: %w", err)
	}
	cleanup := func() { store.Close(); g.Close() }
	return reviewpacket.NewIssuer(context.Background(), g, repoDir, loc.RunDir, loc.EvidenceDir, store), cleanup, nil
}
