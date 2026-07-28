package testgate

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The instruments below are the point of this file.
//
// A test that runs the lifecycle and afterwards checks every collaborator was called would stay green
// if the result were published before the identity was observed, or the command started while the run
// guard was still held. That is presence, not order — and order is the entire reason this package
// exists. So each step asserts, AT THE INSTANT IT RUNS, what must already have happened and what must
// not have happened yet.

// recorder observes the sequence and the guard depth as it goes.
type recorder struct {
	t     *testing.T
	steps []string
	held  int // run-guard depth at this moment
}

func (r *recorder) step(name string) {
	r.t.Helper()
	r.steps = append(r.steps, name)
}

func (r *recorder) did(name string) bool { return slices.Contains(r.steps, name) }

// requireBefore asserts a prerequisite has already happened at the moment `now` runs.
func (r *recorder) requireBefore(now, prerequisite string) {
	r.t.Helper()
	if !r.did(prerequisite) {
		r.t.Errorf("%s ran before %s", now, prerequisite)
	}
}

// requireNotYet asserts a later step has NOT happened at the moment `now` runs.
func (r *recorder) requireNotYet(now, later string) {
	r.t.Helper()
	if r.did(later) {
		r.t.Errorf("%s ran after %s", now, later)
	}
}

func (r *recorder) requireGuard(now string, want int) {
	r.t.Helper()
	if r.held != want {
		r.t.Errorf("%s ran with the run guard depth %d, want %d", now, r.held, want)
	}
}

type fakeContainment struct {
	r        *recorder
	goErr    error
	waitErr  error
	closeErr error
	closed   int
}

func (c *fakeContainment) Go() error {
	c.r.t.Helper()
	// GO is the moment a command first exists. It must come after the state is bound and the guard is
	// released, and before any finalization.
	c.r.requireBefore("go", "bind-active")
	c.r.requireBefore("go", "release-guard")
	c.r.requireNotYet("go", "observe-identity")
	c.r.requireGuard("go", 0)
	c.r.step("go")
	return c.goErr
}

func (c *fakeContainment) Wait() error {
	c.r.t.Helper()
	// The command runs entirely OUTSIDE the lock. Waiting while holding the run guard would make the
	// gate a serialization point for the whole run.
	c.r.requireGuard("wait", 0)
	c.r.requireBefore("wait", "go")
	c.r.step("wait")
	return c.waitErr
}

func (c *fakeContainment) Close() error {
	c.r.t.Helper()
	c.closed++
	c.r.step("close")
	return c.closeErr
}

// harness builds a fully instrumented Deps whose every step checks its own position.
type harness struct {
	r    *recorder
	cont *fakeContainment
	deps Deps

	// Injected failures, one per step.
	failAcquire, failRelease            error
	failAuthorize, failPublishIntent    error
	failArm, failBind                   error
	failReauthorize, failObserve        error
	failPublishResult, failFinalize     error
	releaseCalls, acquireCalls          int
	failNthRelease, failNthAcquire      int
	observedDigest, observedTerminalRsn string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{r: &recorder{t: t}}
	h.cont = &fakeContainment{r: h.r}

	auth := Authorization{
		AttemptID:    "attempt-0001",
		TestedCommit: strings.Repeat("a", 40),
		TestedTree:   strings.Repeat("b", 40),
		IntentDigest: strings.Repeat("c", 64),
	}

	h.deps = Deps{
		AcquireGuard: func() error {
			h.acquireCalls++
			if h.failNthAcquire == h.acquireCalls && h.failAcquire != nil {
				return h.failAcquire
			}
			h.r.held++
			h.r.step("acquire-guard")
			return nil
		},
		ReleaseGuard: func() error {
			h.releaseCalls++
			if h.failNthRelease == h.releaseCalls && h.failRelease != nil {
				return h.failRelease
			}
			h.r.held--
			h.r.step("release-guard")
			return nil
		},
		Authorize: func() (Authorization, error) {
			t.Helper()
			// Authorization is the FIRST thing, and it happens under the guard.
			h.r.requireGuard("authorize", 1)
			h.r.requireNotYet("authorize", "publish-intent")
			h.r.step("authorize")
			return auth, h.failAuthorize
		},
		PublishIntent: func(Authorization) error {
			t.Helper()
			h.r.requireGuard("publish-intent", 1)
			h.r.requireBefore("publish-intent", "authorize")
			// The intent is durable before the containment exists, so a crash between them leaves an
			// orphan intent rather than an armed domain nothing describes.
			h.r.requireNotYet("publish-intent", "arm")
			h.r.step("publish-intent")
			return h.failPublishIntent
		},
		ArmContainment: func(Authorization) (Containment, error) {
			t.Helper()
			h.r.requireGuard("arm", 1)
			h.r.requireBefore("arm", "publish-intent")
			// THE correction: arming precedes the CAS, so every state carrying an active reference is a
			// state in which the containment already existed. The other order made the crash row
			// between them unrecoverable by construction.
			h.r.requireNotYet("arm", "bind-active")
			h.r.step("arm")
			if h.failArm != nil {
				return nil, h.failArm
			}
			return h.cont, nil
		},
		BindActive: func(Authorization) error {
			t.Helper()
			h.r.requireGuard("bind-active", 1)
			h.r.requireBefore("bind-active", "arm")
			h.r.requireNotYet("bind-active", "go")
			h.r.step("bind-active")
			return h.failBind
		},
		Reauthorize: func(Authorization) error {
			t.Helper()
			// Back under the guard, and only after the command has finished.
			h.r.requireGuard("reauthorize", 1)
			h.r.requireBefore("reauthorize", "wait")
			h.r.requireNotYet("reauthorize", "observe-identity")
			h.r.step("reauthorize")
			return h.failReauthorize
		},
		ObserveIdentity: func(Authorization) (Observation, error) {
			t.Helper()
			h.r.requireGuard("observe-identity", 1)
			h.r.requireBefore("observe-identity", "reauthorize")
			// THE other correction: the result cannot exist yet, because it states the identity
			// distinction this observation produces.
			h.r.requireNotYet("observe-identity", "publish-result")
			h.r.step("observe-identity")
			return Observation{
				Commit: auth.TestedCommit, Tree: auth.TestedTree,
				Execution: "ok", TerminalReason: "exited 0",
			}, h.failObserve
		},
		PublishResult: func(_ Authorization, obs Observation) (string, error) {
			t.Helper()
			h.r.requireGuard("publish-result", 1)
			h.r.requireBefore("publish-result", "observe-identity")
			// The record is durable BEFORE state names its digest, so state never references a result a
			// reader cannot fetch.
			h.r.requireNotYet("publish-result", "finalize")
			h.r.step("publish-result")
			h.observedTerminalRsn = obs.TerminalReason
			return strings.Repeat("d", 64), h.failPublishResult
		},
		FinalizeOutcome: func(_ Authorization, _ Observation, digest string) error {
			t.Helper()
			h.r.requireGuard("finalize", 1)
			h.r.requireBefore("finalize", "publish-result")
			h.r.step("finalize")
			h.observedDigest = digest
			return h.failFinalize
		},
	}
	return h
}

// TestTheOrderIsEnforced is the ordering proof: the instruments fail the test at the instant a step
// happens out of order, so a reordering is caught even though the end state looks identical.
func TestTheOrderIsEnforced(t *testing.T) {
	h := newHarness(t)
	res, err := Run(h.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"acquire-guard", "authorize", "publish-intent", "arm", "bind-active", "release-guard",
		"go", "wait",
		"acquire-guard", "reauthorize", "observe-identity", "publish-result", "finalize",
		"release-guard", "close",
	}
	if !slices.Equal(h.r.steps, want) {
		t.Fatalf("sequence =\n  %v\nwant\n  %v", h.r.steps, want)
	}
	if res.ResultDigest != strings.Repeat("d", 64) {
		t.Fatalf("result digest = %q", res.ResultDigest)
	}
	if h.observedDigest != res.ResultDigest {
		t.Fatalf("the outcome CAS named %q, not the published result's digest %q", h.observedDigest, res.ResultDigest)
	}
	if h.r.held != 0 {
		t.Fatalf("the run guard was left held at depth %d", h.r.held)
	}
}

// TestTheCommandRunsOutsideTheLock. A mechanical test can run for minutes; holding a lock the rest of
// the run needs for that long would make the gate a serialization point rather than a check.
func TestTheCommandRunsOutsideTheLock(t *testing.T) {
	h := newHarness(t)
	if _, err := Run(h.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The instruments already assert depth 0 at go and wait; this pins the structural claim as well —
	// exactly one release separates binding from starting.
	bind := slices.Index(h.r.steps, "bind-active")
	rel := slices.Index(h.r.steps, "release-guard")
	goAt := slices.Index(h.r.steps, "go")
	if !(bind < rel && rel < goAt) {
		t.Fatalf("the guard is not released between binding and GO: %v", h.r.steps)
	}
}

// TestAPreAttemptRefusalBindsNothing.
//
// The steps before the CAS are refusals in the strict sense: nothing was minted, nothing armed, no
// state changed, so a caller may retry freely. That promise is worth a distinct error class only if it
// is true, so each case asserts what did NOT happen as well as that the call failed.
func TestAPreAttemptRefusalBindsNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arrange   func(*harness)
		neverRan  []string
		wantArmed bool
	}{
		{
			"authorization refuses",
			func(h *harness) { h.failAuthorize = errors.New("not ownerless TESTS") },
			[]string{"publish-intent", "arm", "bind-active", "go"},
			false,
		},
		{
			"the intent cannot be published",
			func(h *harness) { h.failPublishIntent = errors.New("disk full") },
			[]string{"arm", "bind-active", "go"},
			false,
		},
		{
			"the containment cannot be armed",
			func(h *harness) { h.failArm = errors.New("no job object") },
			[]string{"bind-active", "go"},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			_, err := Run(h.deps)
			if !errors.Is(err, ErrPreAttempt) {
				t.Fatalf("err = %v, want ErrPreAttempt", err)
			}
			for _, s := range tc.neverRan {
				if h.r.did(s) {
					t.Fatalf("%s ran despite a pre-attempt refusal: %v", s, h.r.steps)
				}
			}
			if h.r.held != 0 {
				t.Fatalf("a refusal left the run guard held at depth %d", h.r.held)
			}
			if h.cont.closed != 0 && !tc.wantArmed {
				t.Fatalf("a containment was closed although none was armed")
			}
		})
	}
}

// TestAFailureAfterArmingStillTearsTheContainmentDown.
//
// Once the containment is armed there is a domain that outlives this function unless something releases
// it. A failure that returned early would leave it armed with nobody holding a handle — the exact shape
// the terminal sequence in internal/proctree had to be corrected for, arriving here one layer up.
func TestAFailureAfterArmingStillTearsTheContainmentDown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*harness)
	}{
		{"the active bind fails", func(h *harness) { h.failBind = errors.New("CAS conflict") }},
		{"the guard cannot be released", func(h *harness) {
			h.failRelease = errors.New("lock stuck")
			h.failNthRelease = 1
		}},
		{"the command cannot start", func(h *harness) { h.cont.goErr = errors.New("exec failed") }},
		{"the command cannot be awaited", func(h *harness) { h.cont.waitErr = errors.New("supervisor gone") }},
		{"the guard cannot be reacquired", func(h *harness) {
			h.failAcquire = errors.New("lock stuck")
			h.failNthAcquire = 2
		}},
		{"re-authorization fails", func(h *harness) { h.failReauthorize = errors.New("no longer active") }},
		{"the identity cannot be observed", func(h *harness) { h.failObserve = errors.New("git failed") }},
		{"the result cannot be published", func(h *harness) { h.failPublishResult = errors.New("disk full") }},
		{"the outcome CAS fails", func(h *harness) { h.failFinalize = errors.New("CAS conflict") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			_, err := Run(h.deps)
			if err == nil {
				t.Fatal("Run reported success despite an injected failure")
			}
			if h.cont.closed != 1 {
				t.Fatalf("the containment was closed %d times, want exactly 1: %v", h.cont.closed, h.r.steps)
			}
		})
	}
}

// TestTheGuardIsNeverLeftHeld sweeps the same failures for the other resource.
//
// A refusal that leaves the run locked is worse than the refusal: every other participant in the run
// then blocks on a gate that already gave up.
func TestTheGuardIsNeverLeftHeld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*harness)
	}{
		{"authorization refuses", func(h *harness) { h.failAuthorize = errors.New("x") }},
		{"the intent cannot be published", func(h *harness) { h.failPublishIntent = errors.New("x") }},
		{"the containment cannot be armed", func(h *harness) { h.failArm = errors.New("x") }},
		{"the active bind fails", func(h *harness) { h.failBind = errors.New("x") }},
		{"re-authorization fails", func(h *harness) { h.failReauthorize = errors.New("x") }},
		{"the identity cannot be observed", func(h *harness) { h.failObserve = errors.New("x") }},
		{"the result cannot be published", func(h *harness) { h.failPublishResult = errors.New("x") }},
		{"the outcome CAS fails", func(h *harness) { h.failFinalize = errors.New("x") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			if _, err := Run(h.deps); err == nil {
				t.Fatal("Run reported success despite an injected failure")
			}
			if h.r.held != 0 {
				t.Fatalf("the run guard was left held at depth %d: %v", h.r.held, h.r.steps)
			}
		})
	}
}

// TestIncompleteWiringIsRefusedBeforeAnythingHappens. A missing collaborator is a programming error, and
// discovering it halfway through would leave a partially executed lifecycle rather than a refusal.
func TestIncompleteWiringIsRefusedBeforeAnythingHappens(t *testing.T) {
	for _, drop := range []struct {
		name  string
		blank func(*Deps)
	}{
		{"AcquireGuard", func(d *Deps) { d.AcquireGuard = nil }},
		{"ReleaseGuard", func(d *Deps) { d.ReleaseGuard = nil }},
		{"Authorize", func(d *Deps) { d.Authorize = nil }},
		{"PublishIntent", func(d *Deps) { d.PublishIntent = nil }},
		{"ArmContainment", func(d *Deps) { d.ArmContainment = nil }},
		{"BindActive", func(d *Deps) { d.BindActive = nil }},
		{"Reauthorize", func(d *Deps) { d.Reauthorize = nil }},
		{"ObserveIdentity", func(d *Deps) { d.ObserveIdentity = nil }},
		{"PublishResult", func(d *Deps) { d.PublishResult = nil }},
		{"FinalizeOutcome", func(d *Deps) { d.FinalizeOutcome = nil }},
	} {
		t.Run(drop.name, func(t *testing.T) {
			h := newHarness(t)
			drop.blank(&h.deps)
			_, err := Run(h.deps)
			if !errors.Is(err, ErrPreAttempt) {
				t.Fatalf("err = %v, want ErrPreAttempt", err)
			}
			if !strings.Contains(err.Error(), drop.name) {
				t.Fatalf("err = %v, want it to name the missing collaborator %s", err, drop.name)
			}
			if len(h.r.steps) != 0 {
				t.Fatalf("incomplete wiring executed %v", h.r.steps)
			}
		})
	}
}
