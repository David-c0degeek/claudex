package testgate

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// The instruments below are the point of this file.
//
// A test that runs the lifecycle and afterwards checks every collaborator was called would stay green
// if the result were published before the identity was observed, or the command started while the run
// guard was still held. That is presence, not order. The other half is that the FACTS travel: what was
// armed, published, observed, hashed and bound must be the same execution, so the instruments compare
// the values each step receives against the ones an earlier step produced.

type recorder struct {
	t     *testing.T
	steps []string
	held  int // run-guard depth at this moment
}

func (r *recorder) step(name string)     { r.t.Helper(); r.steps = append(r.steps, name) }
func (r *recorder) did(name string) bool { return slices.Contains(r.steps, name) }

func (r *recorder) requireBefore(now, prerequisite string) {
	r.t.Helper()
	if !r.did(prerequisite) {
		r.t.Errorf("%s ran before %s", now, prerequisite)
	}
}

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

var theCommand = ResolvedCommand{
	Executable: "/usr/bin/go",
	Argv:       []string{"go", "test", "./..."},
	EnvDigest:  strings.Repeat("e", 64),
}

var thePrepared = PreparedAttempt{
	AttemptID:    "attempt-0001",
	TestedCommit: strings.Repeat("a", 40),
	TestedTree:   strings.Repeat("b", 40),
	IntentDigest: strings.Repeat("c", 64),
	Command:      theCommand,
}

var theTerminal = Terminal{
	Execution:      state.TestExecutionOK,
	TerminalReason: "exited 0",
	ExitCode:       intp(0),
}

func intp(i int) *int { return &i }

type fakeContainment struct {
	r        *recorder
	goErr    error
	waitErr  error
	closeErr error
	term     Terminal
	closed   int
}

func (c *fakeContainment) Go() error {
	c.r.t.Helper()
	c.r.requireBefore("go", "bind-active")
	c.r.requireBefore("go", "release-guard")
	c.r.requireNotYet("go", "observe-identity")
	c.r.requireGuard("go", 0)
	c.r.step("go")
	return c.goErr
}

func (c *fakeContainment) Wait() (Terminal, error) {
	c.r.t.Helper()
	// The command runs entirely OUTSIDE the lock. Waiting while holding the run guard would make the
	// gate a serialization point for the whole run.
	c.r.requireGuard("wait", 0)
	c.r.requireBefore("wait", "go")
	c.r.step("wait")
	return c.term, c.waitErr
}

func (c *fakeContainment) Close() error {
	c.r.t.Helper()
	c.closed++
	c.r.step("close")
	return c.closeErr
}

type harness struct {
	r    *recorder
	cont *fakeContainment
	deps Deps

	failAcquire, failRelease         error
	failNthAcquire, failNthRelease   int
	acquireCalls, releaseCalls       int
	failAuthorize, failPublishIntent error
	failArm, failBind                error
	armReturnsContainment            bool
	bindStatus, confirmStatus        BindStatus
	failConfirm                      error
	failReauthorize, failObserve     error
	failPublishResult, failFinalize  error
	identity                         Identity

	// What each step actually received, so the facts can be proven to travel.
	sawIntentCommand   ResolvedCommand
	sawArmCommand      ResolvedCommand
	sawObserveTerminal Terminal
	sawResultTerminal  Terminal
	sawResultIdentity  Identity
	sawFinalizeDigest  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		r:                     &recorder{t: t},
		bindStatus:            BindCommitted,
		confirmStatus:         BindCommitted,
		armReturnsContainment: true,
		identity:              Identity{Value: state.TestIdentityUnchanged, Commit: thePrepared.TestedCommit, Tree: thePrepared.TestedTree},
	}
	h.cont = &fakeContainment{r: h.r, term: theTerminal}

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
		Authorize: func() (PreparedAttempt, error) {
			t.Helper()
			h.r.requireGuard("authorize", 1)
			h.r.requireNotYet("authorize", "publish-intent")
			h.r.step("authorize")
			return thePrepared, h.failAuthorize
		},
		PublishIntent: func(p PreparedAttempt) error {
			t.Helper()
			h.r.requireGuard("publish-intent", 1)
			h.r.requireBefore("publish-intent", "authorize")
			h.r.requireNotYet("publish-intent", "arm")
			h.r.step("publish-intent")
			h.sawIntentCommand = p.Command
			return h.failPublishIntent
		},
		ArmContainment: func(p PreparedAttempt) (Containment, error) {
			t.Helper()
			h.r.requireGuard("arm", 1)
			h.r.requireBefore("arm", "publish-intent")
			// Arming precedes the CAS, so every state carrying an active reference is a state in which
			// the containment already existed.
			h.r.requireNotYet("arm", "bind-active")
			h.r.step("arm")
			h.sawArmCommand = p.Command
			if h.failArm != nil {
				if h.armReturnsContainment {
					// A partial arm still hands back what exists, so the caller can release it.
					return h.cont, h.failArm
				}
				return nil, h.failArm
			}
			return h.cont, nil
		},
		BindActive: func(PreparedAttempt) (BindStatus, error) {
			t.Helper()
			h.r.requireGuard("bind-active", 1)
			h.r.requireBefore("bind-active", "arm")
			h.r.requireNotYet("bind-active", "go")
			h.r.step("bind-active")
			return h.bindStatus, h.failBind
		},
		ConfirmBind: func(PreparedAttempt) (BindStatus, error) {
			t.Helper()
			// Uncertainty is settled while the guard is STILL held; afterwards it belongs to recovery.
			h.r.requireGuard("confirm-bind", 1)
			h.r.requireBefore("confirm-bind", "bind-active")
			h.r.step("confirm-bind")
			return h.confirmStatus, h.failConfirm
		},
		Reauthorize: func(PreparedAttempt) error {
			t.Helper()
			h.r.requireGuard("reauthorize", 1)
			h.r.requireBefore("reauthorize", "wait")
			h.r.requireNotYet("reauthorize", "observe-identity")
			h.r.step("reauthorize")
			return h.failReauthorize
		},
		ObserveIdentity: func(_ PreparedAttempt, term Terminal) (Identity, error) {
			t.Helper()
			h.r.requireGuard("observe-identity", 1)
			h.r.requireBefore("observe-identity", "reauthorize")
			h.r.requireNotYet("observe-identity", "publish-result")
			h.r.step("observe-identity")
			h.sawObserveTerminal = term
			return h.identity, h.failObserve
		},
		PublishResult: func(_ PreparedAttempt, term Terminal, id Identity) (string, error) {
			t.Helper()
			h.r.requireGuard("publish-result", 1)
			h.r.requireBefore("publish-result", "observe-identity")
			h.r.requireNotYet("publish-result", "finalize")
			h.r.step("publish-result")
			h.sawResultTerminal, h.sawResultIdentity = term, id
			return strings.Repeat("d", 64), h.failPublishResult
		},
		FinalizeOutcome: func(_ PreparedAttempt, _ Terminal, _ Identity, digest string) error {
			t.Helper()
			h.r.requireGuard("finalize", 1)
			h.r.requireBefore("finalize", "publish-result")
			h.r.step("finalize")
			h.sawFinalizeDigest = digest
			return h.failFinalize
		},
	}
	return h
}

// TestTheOrderIsEnforced is the ordering proof: the instruments fail at the instant a step happens out
// of order, so a reordering is caught even though the end state looks identical.
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
	if h.r.held != 0 {
		t.Fatalf("the run guard was left held at depth %d", h.r.held)
	}
	if res.Outcome != state.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
}

// TestTheFactsTravelBetweenTheSteps.
//
// Sequencing the calls is only half the guarantee. The other half is that what was armed, published,
// observed, hashed and bound is the SAME execution — otherwise production wiring has to smuggle those
// values through side channels, which is the pattern this design rejects everywhere else.
func TestTheFactsTravelBetweenTheSteps(t *testing.T) {
	h := newHarness(t)
	res, err := Run(h.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The command authorization resolved is the command the intent and the containment received.
	if !slices.Equal(h.sawIntentCommand.Argv, theCommand.Argv) || h.sawIntentCommand.Executable != theCommand.Executable {
		t.Fatalf("intent publication saw %+v, want the authorized command", h.sawIntentCommand)
	}
	if !slices.Equal(h.sawArmCommand.Argv, theCommand.Argv) || h.sawArmCommand.EnvDigest != theCommand.EnvDigest {
		t.Fatalf("arming saw %+v, want the authorized command", h.sawArmCommand)
	}
	// The runner's terminal facts reach the identity observation and the result rather than being
	// invented by them.
	if h.sawObserveTerminal != theTerminal {
		t.Fatalf("the identity observation saw %+v, want the runner's terminal %+v", h.sawObserveTerminal, theTerminal)
	}
	if h.sawResultTerminal != theTerminal {
		t.Fatalf("the result saw %+v, want the runner's terminal", h.sawResultTerminal)
	}
	if h.sawResultIdentity != h.identity {
		t.Fatalf("the result saw identity %+v, want %+v", h.sawResultIdentity, h.identity)
	}
	// And the outcome CAS names the digest of the record that was actually published.
	if h.sawFinalizeDigest != res.ResultDigest {
		t.Fatalf("the outcome CAS named %q, not the published digest %q", h.sawFinalizeDigest, res.ResultDigest)
	}
	if res.Terminal != theTerminal || res.Identity != h.identity {
		t.Fatalf("the result value lost facts: %+v", res)
	}
}

// TestAnUnobservedIdentityStillProducesADurableOutcome.
//
// `unobserved` is a first-class identity value whose combinations deliberately produce indeterminate
// outcomes. An observation that could not be made must be RECORDED as unobserved, not silently recast
// as an unrecorded lifecycle failure — otherwise the attempt vanishes and the gate has no account of it.
func TestAnUnobservedIdentityStillProducesADurableOutcome(t *testing.T) {
	h := newHarness(t)
	h.identity = Identity{Value: state.TestIdentityUnobserved}

	res, err := Run(h.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !h.r.did("publish-result") || !h.r.did("finalize") {
		t.Fatalf("an unobserved identity skipped the durable record: %v", h.r.steps)
	}
	if res.Outcome != state.OutcomeIndeterminate {
		t.Fatalf("outcome = %q, want indeterminate", res.Outcome)
	}
	if res.Identity.Value != state.TestIdentityUnobserved {
		t.Fatalf("identity = %q, want unobserved", res.Identity.Value)
	}
}

// TestTheBindStatusDecidesWhoOwnsTheTeardown.
//
// Collapsing the CAS outcome into `error` was the defect: a clean conflict means no attempt exists and
// this process must destroy the containment, while a visible-but-unconfirmed append means one MAY
// exist — and closing the only handle there destroys the very proof recovery needs, producing exactly
// the post-CAS row with no readable fact that must BLOCK.
func TestTheBindStatusDecidesWhoOwnsTheTeardown(t *testing.T) {
	t.Run("a clean conflict is retryable and tears down", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus = BindNotCommitted

		_, err := Run(h.deps)
		if !errors.Is(err, ErrOrphanedIntent) {
			t.Fatalf("err = %v, want ErrOrphanedIntent", err)
		}
		if errors.Is(err, ErrRecoveryOwned) {
			t.Fatal("a clean conflict was reported as recovery-owned")
		}
		if h.cont.closed != 1 {
			t.Fatalf("the containment was closed %d times, want 1", h.cont.closed)
		}
		if h.r.did("go") {
			t.Fatal("a command started although nothing was bound")
		}
	})

	t.Run("an unconfirmed append is left to recovery", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus = BindUncertain
		h.confirmStatus = BindUncertain

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if h.cont.closed != 0 {
			t.Fatalf("the containment was closed %d times; closing it destroys the proof recovery needs", h.cont.closed)
		}
		if !h.r.did("confirm-bind") {
			t.Fatal("the uncertainty was never put to the confirmation seam")
		}
		if h.r.held != 0 {
			t.Fatalf("the run guard was left held at depth %d", h.r.held)
		}
	})

	t.Run("an uncertain append confirmed as committed proceeds", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus = BindUncertain
		h.confirmStatus = BindCommitted

		if _, err := Run(h.deps); err != nil {
			t.Fatalf("a confirmed append was not allowed to proceed: %v", err)
		}
		if !h.r.did("go") {
			t.Fatalf("the command never started: %v", h.r.steps)
		}
	})

	t.Run("an uncertain append confirmed as not committed is retryable", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus = BindUncertain
		h.confirmStatus = BindNotCommitted

		_, err := Run(h.deps)
		if !errors.Is(err, ErrOrphanedIntent) {
			t.Fatalf("err = %v, want ErrOrphanedIntent", err)
		}
		if h.cont.closed != 1 {
			t.Fatalf("the containment was closed %d times, want 1", h.cont.closed)
		}
	})

	t.Run("a confirmation that itself fails is recovery-owned", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus = BindUncertain
		h.failConfirm = errors.New("store unreadable")

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if h.cont.closed != 0 {
			t.Fatalf("the containment was closed %d times despite unresolved uncertainty", h.cont.closed)
		}
	})
}

// TestTheResidueClassesAreHonest.
//
// The classes differ in WHAT SURVIVES, and each promise is only worth having if it is true. The strict
// class claimed too much once: an arming failure happens AFTER a durable intent publication, which the
// design calls an orphan intent, so calling it "nothing was written" was a lie.
func TestTheResidueClassesAreHonest(t *testing.T) {
	t.Run("authorization wrote nothing", func(t *testing.T) {
		h := newHarness(t)
		h.failAuthorize = errors.New("not ownerless TESTS")

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("err = %v, want ErrRefused", err)
		}
		for _, s := range []string{"publish-intent", "arm", "bind-active", "go"} {
			if h.r.did(s) {
				t.Fatalf("%s ran despite a strict refusal: %v", s, h.r.steps)
			}
		}
	})

	t.Run("an arming failure admits the orphan intent", func(t *testing.T) {
		h := newHarness(t)
		h.failArm = errors.New("no job object")
		h.armReturnsContainment = false

		_, err := Run(h.deps)
		if errors.Is(err, ErrRefused) {
			t.Fatal("an arming failure claimed the strict no-residue class, but the intent is already durable")
		}
		if !errors.Is(err, ErrOrphanedIntent) {
			t.Fatalf("err = %v, want ErrOrphanedIntent", err)
		}
		if !h.r.did("publish-intent") {
			t.Fatal("the fixture never published an intent, so it proves nothing about residue")
		}
		if h.r.did("bind-active") || h.r.did("go") {
			t.Fatalf("an arming failure bound or started something: %v", h.r.steps)
		}
	})

	t.Run("an intent publication failure admits it too", func(t *testing.T) {
		h := newHarness(t)
		h.failPublishIntent = errors.New("durability unconfirmed")

		_, err := Run(h.deps)
		if errors.Is(err, ErrRefused) {
			t.Fatal("a publication failure claimed the strict class; durability may be unconfirmed either way")
		}
		if !errors.Is(err, ErrOrphanedIntent) {
			t.Fatalf("err = %v, want ErrOrphanedIntent", err)
		}
	})

	t.Run("a partial arm still releases what exists", func(t *testing.T) {
		h := newHarness(t)
		h.failArm = errors.New("armed then failed")
		h.armReturnsContainment = true

		if _, err := Run(h.deps); err == nil {
			t.Fatal("Run reported success despite a failed arm")
		}
		if h.cont.closed != 1 {
			t.Fatalf("a partially armed containment was closed %d times, want 1", h.cont.closed)
		}
	})
}

// TestAFailureAfterBindingStillTearsTheContainmentDown.
//
// Once an attempt exists there is a domain that outlives this function unless something releases it. A
// failure that returned early would leave it armed with nobody holding a handle — the asymmetry the
// terminal sequence in internal/proctree had to be corrected for three times, one layer up.
func TestAFailureAfterBindingStillTearsTheContainmentDown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*harness)
		// guardMayBeHeld marks the one case where releasing the guard is ITSELF what failed. The guard
		// really is still held then, and no amount of care here can change that — asserting otherwise
		// would be asserting something the code cannot deliver. It is carved out explicitly rather than
		// by weakening the sweep, so the exception stays visible.
		guardMayBeHeld bool
	}{
		{"the guard cannot be released", func(h *harness) {
			h.failRelease, h.failNthRelease = errors.New("lock stuck"), 1
		}, true},
		{"the command cannot start", func(h *harness) { h.cont.goErr = errors.New("exec failed") }, false},
		{"the command cannot be awaited", func(h *harness) { h.cont.waitErr = errors.New("supervisor gone") }, false},
		{"the guard cannot be reacquired", func(h *harness) {
			h.failAcquire, h.failNthAcquire = errors.New("lock stuck"), 2
		}, false},
		{"re-authorization fails", func(h *harness) { h.failReauthorize = errors.New("no longer active") }, false},
		{"the identity cannot be observed", func(h *harness) { h.failObserve = errors.New("git failed") }, false},
		{"the result cannot be published", func(h *harness) { h.failPublishResult = errors.New("disk full") }, false},
		{"the outcome CAS fails", func(h *harness) { h.failFinalize = errors.New("CAS conflict") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			if _, err := Run(h.deps); err == nil {
				t.Fatal("Run reported success despite an injected failure")
			}
			if h.cont.closed != 1 {
				t.Fatalf("the containment was closed %d times, want exactly 1: %v", h.cont.closed, h.r.steps)
			}
			if !tc.guardMayBeHeld && h.r.held != 0 {
				t.Fatalf("the run guard was left held at depth %d: %v", h.r.held, h.r.steps)
			}
		})
	}
}

// TestIncompleteWiringIsRefusedBeforeAnythingHappens. A missing collaborator is a programming error, and
// discovering it halfway would leave a partially executed lifecycle rather than a refusal.
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
		{"ConfirmBind", func(d *Deps) { d.ConfirmBind = nil }},
		{"Reauthorize", func(d *Deps) { d.Reauthorize = nil }},
		{"ObserveIdentity", func(d *Deps) { d.ObserveIdentity = nil }},
		{"PublishResult", func(d *Deps) { d.PublishResult = nil }},
		{"FinalizeOutcome", func(d *Deps) { d.FinalizeOutcome = nil }},
	} {
		t.Run(drop.name, func(t *testing.T) {
			h := newHarness(t)
			drop.blank(&h.deps)
			_, err := Run(h.deps)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
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
