package main

import (
	"context"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/reviewpacket"
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
	return reviewpacket.NewIssuer(context.Background(), g, repoDir, loc.RunDir, loc.EvidenceDir),
		func() { g.Close() }, nil
}
