package testgate

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
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

// weirdValue is invalid UTF-8 on purpose. Go's JSON encoder replaces such bytes, so a carrier that
// passed the environment as strings would silently deliver something else — and a digest alone could
// never have revealed it, because a digest is not the bytes.
var weirdValue = []byte{0xff, 0xfe, 'x', 0x80}

var theSpec = ExecutionSpec{
	Executable: "/usr/bin/go",
	Argv:       []string{"go", "test", "./..."},
	Cwd:        "/repo/.claudex/runs/run-a/worktree",
	Env: config.ResolvedExecution{
		Identity: config.NameByteExact,
		Env: []config.ResolvedVar{
			{Name: []byte("LONG"), Value: []byte(strings.Repeat("v", 300))},
			{Name: []byte("PATH"), Value: []byte("/usr/bin")},
			{Name: []byte("WEIRD"), Value: weirdValue},
		},
		ScratchHome:  "/repo/.claudex/runs/run-a/scratch/home",
		ScratchCache: "/repo/.claudex/runs/run-a/scratch/cache",
		ScratchTemp:  "/repo/.claudex/runs/run-a/scratch/tmp",
	},
	Digest: strings.Repeat("e", 64),
}

var thePrepared = PreparedAttempt{
	AttemptID:    "attempt-0001",
	TestedCommit: strings.Repeat("a", 40),
	TestedTree:   strings.Repeat("b", 40),
	IntentDigest: strings.Repeat("c", 64),
	Spec:         theSpec,
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
	bindStatus, confirmStatus        CommitStatus
	finalizeStatus, confirmFinStatus CommitStatus
	failConfirmFin                   error
	failConfirm                      error
	failReauthorize, failObserve     error
	failPublishResult, failFinalize  error
	identity                         Identity

	// What each step actually received, so the facts can be proven to travel.
	sawIntentSpec      ExecutionSpec
	sawArmSpec         ExecutionSpec
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
		finalizeStatus:        BindCommitted,
		confirmFinStatus:      BindCommitted,
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
			h.sawIntentSpec = p.Spec
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
			h.sawArmSpec = p.Spec
			if h.failArm != nil {
				if h.armReturnsContainment {
					// A partial arm still hands back what exists, so the caller can release it.
					return h.cont, h.failArm
				}
				return nil, h.failArm
			}
			return h.cont, nil
		},
		BindActive: func(PreparedAttempt) (CommitStatus, error) {
			t.Helper()
			h.r.requireGuard("bind-active", 1)
			h.r.requireBefore("bind-active", "arm")
			h.r.requireNotYet("bind-active", "go")
			h.r.step("bind-active")
			return h.bindStatus, h.failBind
		},
		ConfirmBind: func(PreparedAttempt) (CommitStatus, error) {
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
		FinalizeOutcome: func(_ PreparedAttempt, _ Terminal, _ Identity, digest string) (CommitStatus, error) {
			t.Helper()
			h.r.requireGuard("finalize", 1)
			h.r.requireBefore("finalize", "publish-result")
			h.r.step("finalize")
			h.sawFinalizeDigest = digest
			return h.finalizeStatus, h.failFinalize
		},
		ConfirmFinalize: func(PreparedAttempt) (CommitStatus, error) {
			t.Helper()
			// Settled while the second guard is STILL held, and by reading rather than retrying.
			h.r.requireGuard("confirm-finalize", 1)
			h.r.requireBefore("confirm-finalize", "finalize")
			h.r.step("confirm-finalize")
			return h.confirmFinStatus, h.failConfirmFin
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
	// The complete spec authorization resolved is the spec the intent and the containment received —
	// argv, cwd, AND the environment bytes. A digest could not have carried the last of those, which is
	// exactly why arming would otherwise have had to reach for a side channel.
	for _, got := range []struct {
		who  string
		spec ExecutionSpec
	}{{"intent publication", h.sawIntentSpec}, {"arming", h.sawArmSpec}} {
		if got.spec.Executable != theSpec.Executable || !slices.Equal(got.spec.Argv, theSpec.Argv) {
			t.Fatalf("%s saw executable/argv %q %q, want %q %q", got.who,
				got.spec.Executable, got.spec.Argv, theSpec.Executable, theSpec.Argv)
		}
		if got.spec.Cwd != theSpec.Cwd {
			t.Fatalf("%s saw cwd %q, want %q", got.who, got.spec.Cwd, theSpec.Cwd)
		}
		if !reflect.DeepEqual(got.spec.Env, theSpec.Env) {
			t.Fatalf("%s did not receive the frozen environment: %+v", got.who, got.spec.Env)
		}
		// The two values that a stringly carrier would have destroyed, asserted by content.
		var weird, long []byte
		for _, e := range got.spec.Env.Env {
			switch string(e.Name) {
			case "WEIRD":
				weird = e.Value
			case "LONG":
				long = e.Value
			}
		}
		if !reflect.DeepEqual(weird, weirdValue) {
			t.Fatalf("%s received %v for the invalid-UTF-8 value, want %v", got.who, weird, weirdValue)
		}
		if len(long) != 300 {
			t.Fatalf("%s received a %d-byte value, want 300", got.who, len(long))
		}
	}
	// The runner's terminal facts reach the identity observation and the result rather than being
	// invented by them.
	if h.sawObserveTerminal.Execution != theTerminal.Execution {
		t.Fatalf("the identity observation saw %+v, want the runner's terminal", h.sawObserveTerminal)
	}
	if h.sawResultTerminal.Execution != theTerminal.Execution {
		t.Fatalf("the result saw %+v, want the runner's terminal", h.sawResultTerminal)
	}
	if h.sawResultIdentity != h.identity {
		t.Fatalf("the result saw identity %+v, want %+v", h.sawResultIdentity, h.identity)
	}
	if h.sawFinalizeDigest != res.ResultDigest {
		t.Fatalf("the outcome CAS named %q, not the published digest %q", h.sawFinalizeDigest, res.ResultDigest)
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

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if h.cont.closed != 0 {
			t.Fatalf("the containment was closed %d times; closing it destroys the proof recovery needs", h.cont.closed)
		}
		// Not closing it is only half the job. Ownership has to move to somebody LIVE, or the handle is
		// merely abandoned in an unreachable local — which on Windows loses the sole handle to the
		// unnamed job, the very fact being preserved.
		assertHandedOver(t, res, h.cont)
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

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if h.cont.closed != 0 {
			t.Fatalf("the containment was closed %d times despite unresolved uncertainty", h.cont.closed)
		}
		assertHandedOver(t, res, h.cont)
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
		{"the command cannot start", func(h *harness) { h.cont.goErr = errors.New("exec failed") }, false},
		{"the command cannot be awaited", func(h *harness) { h.cont.waitErr = errors.New("supervisor gone") }, false},
		{"the guard cannot be reacquired", func(h *harness) {
			h.failAcquire, h.failNthAcquire = errors.New("lock stuck"), 2
		}, false},
		{"re-authorization fails", func(h *harness) { h.failReauthorize = errors.New("no longer active") }, false},
		{"the identity cannot be observed", func(h *harness) { h.failObserve = errors.New("git failed") }, false},
		{"the result cannot be published", func(h *harness) { h.failPublishResult = errors.New("disk full") }, false},
		{"the outcome CAS fails", func(h *harness) {
			// The status matters as much as the error: an error whose confirmation reports COMMITTED is
			// a success, not a failure, so the fixture has to say the outcome was not applied.
			h.failFinalize = errors.New("CAS conflict")
			h.confirmFinStatus = BindNotCommitted
		}, false},
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
		{"ConfirmFinalize", func(d *Deps) { d.ConfirmFinalize = nil }},
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

// assertHandedOver proves ownership MOVED rather than merely not being exercised.
func assertHandedOver(t *testing.T, res Result, want Containment) {
	t.Helper()
	if res.Recovery == nil {
		t.Fatal("no recovery handoff: the containment was abandoned in an unreachable local, not transferred")
	}
	if res.Recovery.Containment != want {
		t.Fatalf("the handoff carries %v, not the armed containment", res.Recovery.Containment)
	}
	if res.Recovery.Prepared.AttemptID != thePrepared.AttemptID {
		t.Fatalf("the handoff names attempt %q, not the prepared %q", res.Recovery.Prepared.AttemptID, thePrepared.AttemptID)
	}
	if strings.TrimSpace(res.Recovery.Reason) == "" {
		t.Fatal("the handoff states no reason, so the recipient must infer it from an error string")
	}
}

// TestAnUnknownBindStatusIsRecoveryOwned.
//
// An unrecognised status does NOT establish that no active reference exists, so treating it as an
// ordinary failure tore the containment down on the strength of an answer nobody understood.
func TestAnUnknownBindStatusIsRecoveryOwned(t *testing.T) {
	h := newHarness(t)
	h.bindStatus = CommitStatus(99)
	h.confirmStatus = CommitStatus(99)

	res, err := Run(h.deps)
	if !errors.Is(err, ErrRecoveryOwned) {
		t.Fatalf("err = %v, want ErrRecoveryOwned", err)
	}
	if h.cont.closed != 0 {
		t.Fatalf("an unknown status tore the containment down %d times", h.cont.closed)
	}
	assertHandedOver(t, res, h.cont)
}

// TestABoundAttemptSurvivesAFailedGuardRelease.
//
// The state is definitely bound and the guard may still be held. Closing the containment there would
// volunteer for the Windows post-CAS row with no readable fact — destroying the sole handle over a lock
// failure that says nothing at all about the domain.
func TestABoundAttemptSurvivesAFailedGuardRelease(t *testing.T) {
	h := newHarness(t)
	h.failRelease, h.failNthRelease = errors.New("lock stuck"), 1

	res, err := Run(h.deps)
	if !errors.Is(err, ErrRecoveryOwned) {
		t.Fatalf("err = %v, want ErrRecoveryOwned", err)
	}
	if h.cont.closed != 0 {
		t.Fatalf("the containment was closed %d times over a lock failure", h.cont.closed)
	}
	assertHandedOver(t, res, h.cont)
	if h.r.did("go") {
		t.Fatal("a command started although the guard was never released")
	}
}

// TestTheRunnerFactIsCanonicalizedBeforeItCanReachDisk.
//
// "Already canonical" was only a comment, so a containment returning a token-shaped spawn detail reached
// the durable result seam raw. The state boundary can refuse such a record afterwards, but it cannot
// un-write it — so the canonical form has to be established the moment the fact is accepted, which is
// also before the digest is computed over it.
func TestTheRunnerFactIsCanonicalizedBeforeItCanReachDisk(t *testing.T) {
	const secret = "sk-ant-abcdefghijklmnopqrstuvwx"
	raw := "exec failed: token=" + secret

	h := newHarness(t)
	h.cont.term = Terminal{Execution: state.TestExecutionSpawnFailed, TerminalReason: raw}

	res, err := Run(h.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(h.sawResultTerminal.TerminalReason, secret) {
		t.Fatalf("the result publisher saw the raw credential: %q", h.sawResultTerminal.TerminalReason)
	}
	if h.sawResultTerminal.TerminalReason != state.CanonicalTerminalReason(raw) {
		t.Fatalf("the publisher saw %q, want the canonical form", h.sawResultTerminal.TerminalReason)
	}
	// Length-changing, as it must be: the digest is computed over these bytes, so canonicalizing later
	// would have left it identifying text that no longer exists.
	if len(h.sawResultTerminal.TerminalReason) == len(raw) {
		t.Fatal("canonicalization did not change the text; the vector proves nothing")
	}
	if h.sawObserveTerminal.TerminalReason != h.sawResultTerminal.TerminalReason {
		t.Fatal("the identity observation and the result saw different terminal text")
	}
	if res.Outcome != state.OutcomeIndeterminate {
		t.Fatalf("outcome = %q, want indeterminate for a spawn failure", res.Outcome)
	}
}

// TestAMalformedRunnerFactIsRefusedBeforePublication. The shape is checked where the fact enters, so an
// impossible pair never reaches a durable record.
func TestAMalformedRunnerFactIsRefusedBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name string
		term Terminal
	}{
		{"an unknown execution", Terminal{Execution: "probably-fine", TerminalReason: "x"}},
		{"a normal exit with no exit code", Terminal{Execution: state.TestExecutionOK, TerminalReason: "x"}},
		{"a timeout carrying an exit code", Terminal{Execution: state.TestExecutionTimeout, TerminalReason: "x", ExitCode: intp(0)}},
		{"no terminal detail at all", Terminal{Execution: state.TestExecutionOK, TerminalReason: "  ", ExitCode: intp(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cont.term = tc.term
			if _, err := Run(h.deps); err == nil {
				t.Fatal("a malformed runner fact was accepted")
			}
			if h.r.did("publish-result") {
				t.Fatalf("a malformed fact reached the durable result seam: %v", h.r.steps)
			}
			if h.cont.closed != 1 {
				t.Fatalf("the containment was closed %d times, want 1", h.cont.closed)
			}
		})
	}
}

// TestTheOutcomeCASReportsAnAuthoritativeStatus.
//
// The same erased-status seam as the active binding: "it failed" does not say whether the outcome was
// applied. Confirmation READS rather than retries, because re-applying an outcome that did commit would
// bind a second verdict to one attempt.
func TestTheOutcomeCASReportsAnAuthoritativeStatus(t *testing.T) {
	t.Run("uncertain then confirmed committed succeeds", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted

		if _, err := Run(h.deps); err != nil {
			t.Fatalf("a confirmed outcome was rejected: %v", err)
		}
		if !h.r.did("confirm-finalize") {
			t.Fatal("the uncertainty was never put to the confirmation seam")
		}
	})
	t.Run("not committed is reported as such", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindNotCommitted

		if _, err := Run(h.deps); err == nil || !strings.Contains(err.Error(), "not applied") {
			t.Fatalf("err = %v, want a not-applied refusal", err)
		}
	})
	t.Run("still uncertain after confirmation", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindUncertain

		if _, err := Run(h.deps); err == nil || !strings.Contains(err.Error(), "visible but unconfirmed") {
			t.Fatalf("err = %v, want an unconfirmed report", err)
		}
	})
}

// TestTheExitFactMustAgreeWithTheVerdict.
//
// Presence alone accepted `ok` with exit 17 — which the outcome function turns into a PASS — and
// `nonzero` with exit 0. Both are self-contradictory records, and the first advances the run on the
// strength of a command that failed.
func TestTheExitFactMustAgreeWithTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		term Terminal
	}{
		{"ok with a non-zero code", Terminal{Execution: state.TestExecutionOK, TerminalReason: "exited 17", ExitCode: intp(17)}},
		{"nonzero with a zero code", Terminal{Execution: state.TestExecutionNonzero, TerminalReason: "exited 0", ExitCode: intp(0)}},
		{"a negative exit code", Terminal{Execution: state.TestExecutionNonzero, TerminalReason: "exited -1", ExitCode: intp(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cont.term = tc.term
			if _, err := Run(h.deps); err == nil {
				t.Fatal("a contradictory exit fact was accepted")
			}
			if h.r.did("publish-result") {
				t.Fatalf("it reached the durable result seam: %v", h.r.steps)
			}
		})
	}
}

// TestAnOversizedTerminalDetailIsRefusedBeforePublication.
//
// The ledger bounds this field, and enforcing that only at the state CAS would let the oversized record
// become durable first and be refused afterwards — when it can no longer be un-written. The bound is
// measured AFTER canonicalization, because canonicalization changes the length.
func TestAnOversizedTerminalDetailIsRefusedBeforePublication(t *testing.T) {
	h := newHarness(t)
	h.cont.term = Terminal{
		Execution:      state.TestExecutionTimeout,
		TerminalReason: strings.Repeat("x", state.MaxTerminalReasonBytes+1),
	}
	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "after canonicalization") {
		t.Fatalf("err = %v, want a post-canonicalization bound refusal", err)
	}
	if h.r.did("publish-result") {
		t.Fatalf("an oversized record reached the durable seam: %v", h.r.steps)
	}
}

// TestTheIdentityMustAgreeWithItsOwnEvidence.
//
// The outcome function validates the enum, not what the enum CLAIMS. Without this, `unchanged` could
// name a commit and tree different from the ones the attempt was authorized against — and an `ok`
// execution would then become a pass certifying a tree nobody compared.
func TestTheIdentityMustAgreeWithItsOwnEvidence(t *testing.T) {
	other := strings.Repeat("9", 40)
	for _, tc := range []struct {
		name string
		id   Identity
		want string
	}{
		{"unchanged naming a different tree",
			Identity{Value: state.TestIdentityUnchanged, Commit: thePrepared.TestedCommit, Tree: other},
			"different commit/tree"},
		{"unchanged naming a different commit",
			Identity{Value: state.TestIdentityUnchanged, Commit: other, Tree: thePrepared.TestedTree},
			"different commit/tree"},
		{"changed naming exactly the authorized identity",
			Identity{Value: state.TestIdentityChanged, Commit: thePrepared.TestedCommit, Tree: thePrepared.TestedTree},
			"exactly the authorized"},
		{"changed observing nothing",
			Identity{Value: state.TestIdentityChanged},
			"observed no commit/tree"},
		{"unobserved carrying an identity it cannot have observed",
			Identity{Value: state.TestIdentityUnobserved, Commit: other, Tree: other},
			"cannot have observed"},
		{"an unknown identity value",
			Identity{Value: "probably-fine"},
			"unknown identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.identity = tc.id
			_, err := Run(h.deps)
			if err == nil {
				t.Fatal("a self-contradictory identity was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if h.r.did("publish-result") {
				t.Fatalf("it reached the durable result seam: %v", h.r.steps)
			}
		})
	}
}

// TestAMutatingCollaboratorCannotChangeWhatLaterStepsReceive.
//
// The same prepared value goes to intent publication, arming, binding, result publication and the
// handoff. Shared shallow copies mean a plausible canonicalizer or sorter in the FIRST collaborator
// rewrites the backing array the LATER ones read — after the digest identifying those bytes was already
// chosen. The read-only fakes elsewhere in this file cannot detect that, so this one mutates on purpose.
func TestAMutatingCollaboratorCannotChangeWhatLaterStepsReceive(t *testing.T) {
	h := newHarness(t)
	original := h.deps.PublishIntent
	h.deps.PublishIntent = func(p PreparedAttempt) error {
		// Exactly the kind of in-place tidying a real implementation might do.
		for i := range p.Spec.Argv {
			p.Spec.Argv[i] = "MUTATED"
		}
		for i := range p.Spec.Env.Env {
			p.Spec.Env.Env[i].Value = []byte("MUTATED")
			if len(p.Spec.Env.Env[i].Name) > 0 {
				p.Spec.Env.Env[i].Name[0] = 'Z'
			}
		}
		p.Spec.Cwd = "/mutated"
		return original(p)
	}

	if _, err := Run(h.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(h.sawArmSpec.Argv, theSpec.Argv) {
		t.Fatalf("arming received mutated argv %q, want the authorized %q", h.sawArmSpec.Argv, theSpec.Argv)
	}
	if h.sawArmSpec.Cwd != theSpec.Cwd {
		t.Fatalf("arming received mutated cwd %q", h.sawArmSpec.Cwd)
	}
	if !reflect.DeepEqual(h.sawArmSpec.Env, theSpec.Env) {
		t.Fatalf("arming received a mutated environment: %+v", h.sawArmSpec.Env)
	}
	// And the package's own fixture must be untouched, or every other test in this file has been
	// running against corrupted data.
	if !slices.Equal(theSpec.Argv, []string{"go", "test", "./..."}) {
		t.Fatalf("the shared fixture was mutated: %q", theSpec.Argv)
	}
}

// TestTheHandoffCarriesATypedGuardDisposition.
//
// Recovery cannot act on prose. Reacquiring a guard this process still holds risks self-deadlock;
// proceeding under one that was actually released risks unlocked mutation. A failed release establishes
// NEITHER, so the honest value is `unknown` and the recipient must block rather than choose.
func TestTheHandoffCarriesATypedGuardDisposition(t *testing.T) {
	t.Run("an unresolved bind with a clean release", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus, h.confirmStatus = BindUncertain, BindUncertain

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		assertHandedOver(t, res, h.cont)
		if res.Recovery.Guard != GuardReleased {
			t.Fatalf("guard disposition = %v, want released", res.Recovery.Guard)
		}
	})

	// The COMBINATION, which neither condition alone exercises: the bind is unresolved AND the release
	// then fails, so nothing is known about either the state or the lock.
	t.Run("an unresolved bind whose release also fails", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus, h.confirmStatus = BindUncertain, BindUncertain
		h.failRelease, h.failNthRelease = errors.New("lock stuck"), 1

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		assertHandedOver(t, res, h.cont)
		if res.Recovery.Guard != GuardUnknown {
			t.Fatalf("guard disposition = %v, want unknown", res.Recovery.Guard)
		}
		if h.cont.closed != 0 {
			t.Fatalf("the containment was closed %d times", h.cont.closed)
		}
	})

	t.Run("a bound attempt whose release fails", func(t *testing.T) {
		h := newHarness(t)
		h.failRelease, h.failNthRelease = errors.New("lock stuck"), 1

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		assertHandedOver(t, res, h.cont)
		if res.Recovery.Guard != GuardUnknown {
			t.Fatalf("guard disposition = %v, want unknown", res.Recovery.Guard)
		}
	})
}

// TestRunNeverClaimsToStillHoldTheGuard pins the disclosure attached to GuardHeld.
//
// The value exists so a recipient's handling is TOTAL over the vocabulary, but this function always
// attempts release and therefore never produces it. Asserting that keeps the claim checked rather than
// merely written down — and if a future path does hand off while holding, this fails and forces the
// documentation to be updated with it.
func TestRunNeverClaimsToStillHoldTheGuard(t *testing.T) {
	for _, arrange := range []func(*harness){
		func(h *harness) { h.bindStatus, h.confirmStatus = BindUncertain, BindUncertain },
		func(h *harness) { h.failRelease, h.failNthRelease = errors.New("x"), 1 },
		func(h *harness) { h.bindStatus = CommitStatus(99); h.confirmStatus = CommitStatus(99) },
		func(h *harness) {
			h.bindStatus, h.confirmStatus = BindUncertain, BindUncertain
			h.failRelease, h.failNthRelease = errors.New("x"), 1
		},
	} {
		h := newHarness(t)
		arrange(h)
		res, _ := Run(h.deps)
		if res.Recovery != nil && res.Recovery.Guard == GuardHeld {
			t.Fatal("Run handed off claiming to still hold the guard; the documented disclosure is now false")
		}
	}
}
