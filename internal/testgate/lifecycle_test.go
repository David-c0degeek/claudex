package testgate

import (
	"bytes"
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

// rebind recomputes the intent digest for a bent prepared attempt.
//
// The lifecycle now REFUSES an authorization whose bound digest is not the digest of the intent it
// describes, so a test that changes a field of the attempt has to rebind it - which is the rule working,
// not an inconvenience.
func rebind(t *testing.T, p PreparedAttempt) PreparedAttempt {
	t.Helper()
	_, digest, err := buildIntentRecord(p)
	if err != nil {
		t.Fatalf("rebinding the intent: %v", err)
	}
	p.IntentDigest = digest
	return p
}

// thePrepared binds the digest of the intent it actually describes, computed rather than asserted -
// which is the property the lifecycle now enforces, so a constant here would simply be refused.
var thePrepared = func() PreparedAttempt {
	p := preparedShape
	_, digest, err := buildIntentRecord(p)
	if err != nil {
		panic("fixture intent does not encode: " + err.Error())
	}
	p.IntentDigest = digest
	return p
}()

var preparedShape = PreparedAttempt{
	AttemptID:     "attempt-0001",
	StartRevision: 41,
	TestedCommit:  strings.Repeat("a", 40),
	TestedTree:    strings.Repeat("b", 40),
	Spec:          theSpec,
	// The frozen COMBINED retained-output ceiling, carried with the attempt because the lifecycle has to
	// enforce it and does not read policy.
	MaxOutputBytes: 32768,
	MaxRecordBytes: 65536,
}

var theTerminal = Terminal{
	Execution:      state.TestExecutionOK,
	TerminalReason: "exited 0",
	HasExitCode:    true,
	Author:         state.TerminalByRunner,
}

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
	// A runner that reuses its output buffer once the domain is gone. It handed those bytes over at
	// Wait; if the accepted terminal merely pointed at them, the evidence would change underneath
	// everything downstream that had already been told what it was.
	for i := range c.term.Stdout.Head {
		c.term.Stdout.Head[i] = 'R'
	}
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
	failReserve                      error
	reservedRevision                 uint64
	authorizedRevision               uint64
	boundRevision                    uint64
	publishedDigest                  string
	failArm, failBind                error
	armReturnsContainment            bool
	bindStatus, confirmStatus        CommitStatus
	finalizeStatus, confirmFinStatus CommitStatus
	failConfirmFin                   error
	confirmFinDigest                 string
	confirmFinAttemptID              string
	confirmFinExecution              state.TestExecution
	confirmFinIdentity               state.TestIdentity
	confirmFinReason                 string
	confirmFinAuthor                 state.TerminalAuthor
	confirmFinTree                   string
	confirmFinCommit                 string
	confirmFinStart                  uint64
	confirmFinNoEntry                bool
	failConfirm                      error
	failReauthorize, failObserve     error
	failPublishResult, failFinalize  error
	publishedResultDigest            string
	identity                         Identity

	// What each step actually received, so the facts can be proven to travel.
	sawIntentRecord      *IntentRecord
	sawAuthorizeRevision uint64
	sawArmSpec           ExecutionSpec
	sawObserveTerminal   Terminal
	sawResultTerminal    Terminal
	sawResultIdentity    Identity
	sawResultRecord      *ResultRecord
	sawFinalizeDigest    string
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
		reservedRevision:      41,
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
		ReserveStartRevision: func() (uint64, error) {
			t.Helper()
			h.r.requireGuard("reserve-start-revision", 1)
			h.r.requireNotYet("reserve-start-revision", "authorize")
			h.r.step("reserve-start-revision")
			return h.reservedRevision, h.failReserve
		},
		Authorize: func(startRevision uint64) (PreparedAttempt, error) {
			t.Helper()
			h.r.requireGuard("authorize", 1)
			h.r.requireBefore("authorize", "reserve-start-revision")
			h.r.requireNotYet("authorize", "publish-intent")
			h.r.step("authorize")
			h.sawAuthorizeRevision = startRevision
			// A CLONE of the package fixture. Returning it directly makes the fixture itself mutable
			// through every test that runs, so one mutation test would corrupt the data every other test
			// in this file asserts against - and the corruption would depend on execution order.
			p := clonePrepared(thePrepared)
			p.StartRevision = startRevision
			if h.authorizedRevision != 0 {
				p.StartRevision = h.authorizedRevision
			}
			return p, h.failAuthorize
		},
		PublishIntent: func(rec IntentRecord) (string, error) {
			t.Helper()
			h.r.requireGuard("publish-intent", 1)
			h.r.requireBefore("publish-intent", "authorize")
			h.r.requireNotYet("publish-intent", "arm")
			h.r.step("publish-intent")
			h.sawIntentRecord = &rec
			if h.publishedDigest != "" {
				return h.publishedDigest, h.failPublishIntent
			}
			// A PRODUCTION-SHAPED publisher encodes what it was handed and reports THAT record's digest.
			_, d, err := rec.Encode()
			if err != nil {
				return "", err
			}
			return d, h.failPublishIntent
		},
		ArmContainment: func(p PreparedAttempt) (Containment, error) {
			t.Helper()
			h.r.requireGuard("arm", 1)
			h.r.requireBefore("arm", "publish-intent")
			// Arming precedes the CAS, so every state carrying an active reference is a state in which
			// the containment already existed.
			h.r.requireNotYet("arm", "bind-active")
			h.r.step("arm")
			h.sawArmSpec = clonePrepared(p).Spec
			if h.failArm != nil {
				if h.armReturnsContainment {
					// A partial arm still hands back what exists, so the caller can release it.
					return h.cont, h.failArm
				}
				return nil, h.failArm
			}
			return h.cont, nil
		},
		BindActive: func(p PreparedAttempt) (Bind, error) {
			t.Helper()
			h.r.requireGuard("bind-active", 1)
			h.r.requireBefore("bind-active", "arm")
			h.r.requireNotYet("bind-active", "go")
			h.r.step("bind-active")
			rev := h.reservedRevision
			if h.boundRevision != 0 {
				rev = h.boundRevision
			}
			return Bind{Status: h.bindStatus, Revision: rev}, h.failBind
		},
		ConfirmBind: func(p PreparedAttempt) (Bind, error) {
			t.Helper()
			// Uncertainty is settled while the guard is STILL held; afterwards it belongs to recovery.
			h.r.requireGuard("confirm-bind", 1)
			h.r.requireBefore("confirm-bind", "bind-active")
			h.r.step("confirm-bind")
			rev := h.reservedRevision
			if h.boundRevision != 0 {
				rev = h.boundRevision
			}
			return Bind{Status: h.confirmStatus, Revision: rev}, h.failConfirm
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
		PublishResult: func(rec ResultRecord) (string, error) {
			t.Helper()
			h.r.requireGuard("publish-result", 1)
			h.r.requireBefore("publish-result", "observe-identity")
			h.r.requireNotYet("publish-result", "finalize")
			h.r.step("publish-result")
			h.sawResultRecord = &rec
			h.sawResultTerminal = Terminal{
				Execution: rec.Execution, TerminalReason: rec.TerminalReason, Author: rec.TerminalAuthor,
				HasExitCode: rec.HasExitCode, ExitCode: rec.ExitCode,
				Stdout: rec.Stdout, Stderr: rec.Stderr,
			}
			h.sawResultIdentity = Identity{Value: rec.Identity, Commit: rec.TestedCommit, Tree: rec.TestedTree}
			if h.publishedResultDigest != "" {
				return h.publishedResultDigest, h.failPublishResult
			}
			// A PRODUCTION-SHAPED publisher encodes what it was given and reports that record's digest.
			// Returning a constant demonstrated the very gap this seam is meant to close.
			_, d, err := rec.Encode()
			if err != nil {
				return "", err
			}
			return d, h.failPublishResult
		},
		FinalizeOutcome: func(_ PreparedAttempt, _ Terminal, _ Identity, digest string) (CommitStatus, error) {
			t.Helper()
			h.r.requireGuard("finalize", 1)
			h.r.requireBefore("finalize", "publish-result")
			h.r.step("finalize")
			h.sawFinalizeDigest = digest
			return h.finalizeStatus, h.failFinalize
		},
		ConfirmFinalize: func(p PreparedAttempt, term Terminal, id Identity, digest string) (FinalizeConfirmation, error) {
			t.Helper()
			// Settled while the second guard is STILL held, and by reading rather than retrying.
			h.r.requireGuard("confirm-finalize", 1)
			h.r.requireBefore("confirm-finalize", "finalize")
			h.r.step("confirm-finalize")
			conf := FinalizeConfirmation{Status: h.confirmFinStatus}
			if h.confirmFinStatus == BindCommitted {
				entry := state.FinalizedAttempt{
					AttemptID: p.AttemptID, ResultDigest: digest,
					TestedCommit: p.TestedCommit, TestedTree: p.TestedTree,
					StartRevision: p.StartRevision,
					Execution:     term.Execution, Identity: id.Value,
					TerminalReason: term.TerminalReason, TerminalAuthor: term.Author,
				}
				if h.confirmFinDigest != "" {
					entry.ResultDigest = h.confirmFinDigest
				}
				if h.confirmFinAttemptID != "" {
					entry.AttemptID = h.confirmFinAttemptID
				}
				if h.confirmFinExecution != "" {
					entry.Execution = h.confirmFinExecution
				}
				if h.confirmFinIdentity != "" {
					entry.Identity = h.confirmFinIdentity
				}
				if h.confirmFinReason != "" {
					entry.TerminalReason = h.confirmFinReason
				}
				if h.confirmFinAuthor != "" {
					entry.TerminalAuthor = h.confirmFinAuthor
				}
				if h.confirmFinTree != "" {
					entry.TestedTree = h.confirmFinTree
				}
				if h.confirmFinCommit != "" {
					entry.TestedCommit = h.confirmFinCommit
				}
				if h.confirmFinStart != 0 {
					entry.StartRevision = h.confirmFinStart
				}
				if h.confirmFinNoEntry {
					return FinalizeConfirmation{Status: BindCommitted}, h.failConfirmFin
				}
				conf.Entry = &entry
			}
			return conf, h.failConfirmFin
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
		"acquire-guard", "reserve-start-revision", "authorize", "publish-intent", "arm", "bind-active", "release-guard",
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
	}{{"arming", h.sawArmSpec}} {
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
	if h.sawResultIdentity.Value != h.identity.Value {
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
	h.cont.term = Terminal{Author: authorFor(state.TestExecutionSpawnFailed), Execution: state.TestExecutionSpawnFailed, TerminalReason: raw}

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
		{"a normal exit with no exit code", Terminal{Author: authorFor(state.TestExecutionOK), Execution: state.TestExecutionOK, TerminalReason: "x"}},
		{"a timeout carrying an exit code", Terminal{Author: authorFor(state.TestExecutionTimeout), Execution: state.TestExecutionTimeout, TerminalReason: "x", HasExitCode: true, ExitCode: 0}},
		{"no terminal detail at all", Terminal{Author: authorFor(state.TestExecutionOK), Execution: state.TestExecutionOK, TerminalReason: "  ", HasExitCode: true, ExitCode: 0}},
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
		{"ok with a non-zero code", Terminal{Author: authorFor(state.TestExecutionOK), Execution: state.TestExecutionOK, TerminalReason: "exited 17", HasExitCode: true, ExitCode: 17}},
		{"nonzero with a zero code", Terminal{Author: authorFor(state.TestExecutionNonzero), Execution: state.TestExecutionNonzero, TerminalReason: "exited 0", HasExitCode: true, ExitCode: 0}},
		{"a negative exit code", Terminal{Author: authorFor(state.TestExecutionNonzero), Execution: state.TestExecutionNonzero, TerminalReason: "exited -1", HasExitCode: true, ExitCode: -1}},
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
		Author:         state.TerminalByRunner,
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
	// Every seam that receives a PreparedAttempt records what it was handed and then scribbles over it,
	// which is the kind of in-place tidying a real canonicalizer or sorter would do. Wrapping only ONE
	// seam proves only that seam: the value flows through nine of them, and the clone that protects the
	// next collaborator is invisible to a test whose mutator runs after it. The confirmation seams run
	// only on their uncertain branches, so each gets its own arrangement rather than being asserted from
	// a path that never calls them.
	for _, tc := range []struct {
		name    string
		arrange func(*harness)
		expect  []string
	}{
		{"the ordinary path", func(*harness) {}, []string{
			"arming", "binding", "re-authorization",
			"identity observation", "finalization", "the returned result"}},
		{"an unresolved active bind", func(h *harness) {
			h.bindStatus, h.confirmStatus = BindUncertain, BindCommitted
		}, []string{"bind confirmation", "re-authorization", "identity observation",
			"finalization", "the returned result"}},
		{"an unresolved outcome append", func(h *harness) {
			h.finalizeStatus, h.confirmFinStatus = BindUncertain, BindCommitted
		}, []string{"finalize confirmation", "the returned result"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)
			seen := map[string]PreparedAttempt{}
			scribble := func(p PreparedAttempt) {
				for i := range p.Spec.Argv {
					p.Spec.Argv[i] = "MUTATED"
				}
				for i := range p.Spec.Env.Env {
					p.Spec.Env.Env[i].Value = []byte("MUTATED")
					if len(p.Spec.Env.Env[i].Name) > 0 {
						p.Spec.Env.Env[i].Name[0] = 'Z'
					}
				}
			}
			observe := func(who string, p PreparedAttempt) {
				seen[who] = clonePrepared(p) // a SNAPSHOT: recording p would alias the thing under audit.
				scribble(p)
			}

			// The authorizer keeps its own reference and mutates it LATER, once the lifecycle is under
			// way. Nothing else can detect a missing clone at the authorization boundary, because the
			// very next seam clones defensively and hides it.
			var retained PreparedAttempt
			authorize := h.deps.Authorize
			h.deps.Authorize = func(startRevision uint64) (PreparedAttempt, error) {
				p, err := authorize(startRevision)
				retained = p
				return p, err
			}

			var reportedDigest string
			var publishedView ExecutionView
			publishIntent, arm := h.deps.PublishIntent, h.deps.ArmContainment
			bind, confirmBind := h.deps.BindActive, h.deps.ConfirmBind
			reauth, observeID := h.deps.Reauthorize, h.deps.ObserveIdentity
			publishResult, finalize := h.deps.PublishResult, h.deps.FinalizeOutcome
			confirmFin := h.deps.ConfirmFinalize
			h.deps.PublishIntent = func(rec IntentRecord) (string, error) {
				// The intent seam receives the RECORD the lifecycle built, not the prepared attempt, so
				// there is no prepared value here to audit. What it can still show is the authorizer
				// reaching back through the value it handed over.
				scribble(retained)
				d, err := publishIntent(rec)
				reportedDigest = d
				publishedView = rec.View
				return d, err
			}
			h.deps.ArmContainment = func(p PreparedAttempt) (Containment, error) {
				observe("arming", p)
				return arm(p)
			}
			h.deps.BindActive = func(p PreparedAttempt) (Bind, error) {
				observe("binding", p)
				return bind(p)
			}
			h.deps.ConfirmBind = func(p PreparedAttempt) (Bind, error) {
				observe("bind confirmation", p)
				return confirmBind(p)
			}
			h.deps.Reauthorize = func(p PreparedAttempt) error {
				observe("re-authorization", p)
				return reauth(p)
			}
			h.deps.ObserveIdentity = func(p PreparedAttempt, term Terminal) (Identity, error) {
				observe("identity observation", p)
				return observeID(p, term)
			}
			h.deps.PublishResult = func(rec ResultRecord) (string, error) {
				// The record is built from the prepared attempt inside Run, so this seam sees the
				// RESULT of that build rather than the value itself. Auditing it as a prepared attempt
				// would be auditing something the seam never receives.
				return publishResult(rec)
			}
			h.deps.FinalizeOutcome = func(p PreparedAttempt, term Terminal, id Identity, digest string) (CommitStatus, error) {
				observe("finalization", p)
				return finalize(p, term, id, digest)
			}
			h.deps.ConfirmFinalize = func(p PreparedAttempt, term Terminal, id Identity, digest string) (FinalizeConfirmation, error) {
				observe("finalize confirmation", p)
				return confirmFin(p, term, id, digest)
			}

			res, err := Run(h.deps)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			seen["the returned result"] = res.Prepared
			for _, who := range tc.expect {
				got, ok := seen[who]
				if !ok {
					t.Fatalf("%s was never audited; the coverage claim is wider than the test: %v", who, keysOf(seen))
				}
				if !slices.Equal(got.Spec.Argv, theSpec.Argv) {
					t.Fatalf("%s received argv %q, want the authorized %q", who, got.Spec.Argv, theSpec.Argv)
				}
				if got.Spec.Cwd != theSpec.Cwd {
					t.Fatalf("%s received cwd %q, want %q", who, got.Spec.Cwd, theSpec.Cwd)
				}
				if !reflect.DeepEqual(got.Spec.Env, theSpec.Env) {
					t.Fatalf("%s received a mutated environment: %+v", who, got.Spec.Env)
				}
			}
			// PUBLISHED and ARMED must both be the authorized execution. Asserting only the second would
			// leave the two free to differ - a green test with the intent describing command B while
			// command A runs. The digest the publisher REPORTED must therefore still describe the bytes
			// its own recorder snapshotted.
			// PUBLISHED and ARMED must describe the same execution. Asserting only the second would
			// leave the two free to differ - a green test with the intent describing command B while
			// command A runs.
			if reportedDigest != thePrepared.IntentDigest {
				t.Fatalf("the publisher reported digest %q, the attempt bound %q", reportedDigest, thePrepared.IntentDigest)
			}
			armedView, verr := seen["arming"].Spec.View()
			if verr != nil {
				t.Fatalf("View: %v", verr)
			}
			gotView, gerr := publishedView.Digest()
			wantView, werr := armedView.Digest()
			if gerr != nil || werr != nil {
				t.Fatalf("digesting: %v / %v", gerr, werr)
			}
			if gotView != wantView {
				t.Fatalf("the intent describes execution %s but arming ran %s", gotView, wantView)
			}
			// And the package fixture is untouched, or every other test here has been running on corrupt
			// data.
			if !slices.Equal(theSpec.Argv, []string{"go", "test", "./..."}) {
				t.Fatalf("the shared fixture was mutated: %q", theSpec.Argv)
			}
		})
	}
}

func keysOf(m map[string]PreparedAttempt) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestAMutatingCollaboratorCannotChangeTheAcceptedTerminal.
//
// The exit fact is checked for agreement with the verdict ONCE, at acceptance. While it travelled as a
// pointer, an identity collaborator could set the pointed value to 17 after ok-with-0 had been accepted:
// outcome derivation still produced a pass, result publication recorded a command that failed, and
// nothing downstream could notice, because the ledger does not store the exit code. The runner's own
// copy of that pointer was live the whole time too. The audit that caught this class for the prepared
// value never touched the terminal.
func TestAMutatingCollaboratorCannotChangeTheAcceptedTerminal(t *testing.T) {
	h := newHarness(t)
	accepted := Terminal{
		Execution: theTerminal.Execution, TerminalReason: theTerminal.TerminalReason,
		HasExitCode: true, ExitCode: 0, Author: state.TerminalByRunner,
		Stdout: StreamRecord{Present: true, SourceBytes: 12, RedactedBytes: 3,
			SHA256: sha256Hex([]byte("abc")), Head: []byte("abc")},
	}
	h.cont.term = accepted

	seen := map[string]Terminal{}
	observeID, publishResult := h.deps.ObserveIdentity, h.deps.PublishResult
	finalize := h.deps.FinalizeOutcome
	h.deps.ObserveIdentity = func(p PreparedAttempt, term Terminal) (Identity, error) {
		seen["identity observation"] = term
		term.ExitCode, term.Execution = 17, state.TestExecutionTimeout
		term.TerminalReason = "MUTATED"
		// The stream excerpt is the one part of a terminal that is a SLICE, so this is the only field a
		// collaborator can rewrite through a value copy - and it happens BEFORE the record is built.
		for i := range term.Stdout.Head {
			term.Stdout.Head[i] = 'Z'
		}
		return observeID(p, term)
	}
	h.deps.PublishResult = func(rec ResultRecord) (string, error) {
		seen["result publication"] = Terminal{
			Execution: rec.Execution, TerminalReason: rec.TerminalReason, Author: rec.TerminalAuthor,
			HasExitCode: rec.HasExitCode, ExitCode: rec.ExitCode,
		}
		// Mutated AFTER the real publisher has seen it: a publisher that corrupts the record before
		// encoding simply cannot produce a digest for it now, which is the boundary doing its job rather
		// than the property this test is about.
		d, err := publishResult(rec)
		rec.ExitCode = 17
		return d, err
	}
	h.deps.FinalizeOutcome = func(p PreparedAttempt, term Terminal, id Identity, digest string) (CommitStatus, error) {
		seen["finalization"] = term
		// The LAST collaborator to see the terminal, and the returned Result is built after it. Without
		// its own copy, whatever it leaves behind is what the caller is told happened.
		for i := range term.Stdout.Head {
			term.Stdout.Head[i] = 'Q'
		}
		return finalize(p, term, id, digest)
	}

	res, err := Run(h.deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen["the returned result"] = res.Terminal
	if !bytes.Equal(res.Terminal.Stdout.Head, []byte("abc")) {
		t.Fatalf("the returned result carries evidence a later collaborator rewrote: %q", res.Terminal.Stdout.Head)
	}
	for who, got := range seen {
		if got.Execution != accepted.Execution || !got.HasExitCode || got.ExitCode != 0 {
			t.Fatalf("%s received %+v, want the accepted %+v", who, got, accepted)
		}
		if got.TerminalReason != state.CanonicalTerminalReason(accepted.TerminalReason) {
			t.Fatalf("%s received terminal reason %q", who, got.TerminalReason)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("only %d seams were audited: %v", len(seen), seen)
	}
	// The evidence the record bound must be what was ACCEPTED, not what a later collaborator left behind.
	if !bytes.Equal(h.sawResultRecord.Stdout.Head, []byte("abc")) {
		t.Fatalf("a collaborator rewrote the stream evidence before it was recorded: %q", h.sawResultRecord.Stdout.Head)
	}
	// The runner is entitled to reuse its own buffer once the domain is gone - and it does, in Close.
	// What must NOT happen is that reuse reaching the evidence this process accepted.
	if !bytes.Equal(res.Terminal.Stdout.Head, []byte("abc")) {
		t.Fatalf("the runner's buffer reuse reached the returned result: %q", res.Terminal.Stdout.Head)
	}
	// The runner still holds whatever it returned, and it must not be a handle into the accepted fact.
	if h.cont.term.ExitCode != 0 || h.cont.term.Execution != accepted.Execution {
		t.Fatalf("the runner's own copy was rewritten: %+v", h.cont.term)
	}
	if res.Outcome != state.OutcomePass {
		t.Fatalf("outcome = %q, want pass for an accepted ok/0 with an unchanged identity", res.Outcome)
	}
}

// TestThePublishedIntentMustBeTheAuthorizedOne.
//
// Isolating later collaborators from mutation is necessary and not sufficient: a publisher that writes
// a different command produces an intent describing execution B while arming runs execution A, and
// nothing downstream would notice. The publisher therefore reports the digest of the exact bytes it
// wrote, and the lifecycle compares it to what was authorized.
func TestThePublishedIntentMustBeTheAuthorizedOne(t *testing.T) {
	h := newHarness(t)
	h.publishedDigest = strings.Repeat("f", 64)

	_, err := Run(h.deps)
	if !errors.Is(err, ErrOrphanedIntent) {
		t.Fatalf("err = %v, want ErrOrphanedIntent", err)
	}
	if !strings.Contains(err.Error(), "not the authorized") {
		t.Fatalf("err = %v, want it to name the mismatch", err)
	}
	if h.r.did("arm") || h.r.did("bind-active") {
		t.Fatalf("an attempt proceeded on top of an intent that describes something else: %v", h.r.steps)
	}
}

// TestConfirmationMustFindTHISLifecyclesOutcome.
//
// Given only the attempt id, confirmation could answer no better than "some finalization exists" — so
// an entry written by another party, carrying a different result, would have been reported as this
// lifecycle's success, returning a digest nothing in the ledger references.
func TestConfirmationMustFindTHISLifecyclesOutcome(t *testing.T) {
	t.Run("a committed entry with a different result digest", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinDigest = strings.Repeat("9", 64)

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if !strings.Contains(err.Error(), "is not the one published here") {
			t.Fatalf("err = %v, want it to name the mismatch", err)
		}
	})

	t.Run("a committed entry belonging to another attempt", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinAttemptID = "somebody-elses-attempt"

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	// A bound entry can also record the SAME digest under a different verdict, which is a ledger that
	// disagrees with itself about what this attempt did. Comparing only the digest would accept it.
	t.Run("a committed entry recording a different execution", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinExecution = state.TestExecutionTimeout

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	t.Run("a committed entry recording a different identity", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinIdentity = state.TestIdentityUnobserved

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	// The reason does not steer routing anywhere, which is exactly why it was left out of the first
	// comparison. It is still the text a human reads to understand the verdict, so a record pairing this
	// attempt's digest with somebody else's account of how the command ended disagrees with itself.
	t.Run("a committed entry recording a different terminal reason", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinReason = "something else entirely"

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if !strings.Contains(err.Error(), "terminal reason") {
			t.Fatalf("err = %v, want it to name the field", err)
		}
	})

	// The ledger stores WHO wrote the account beside the account itself. An entry naming the same digest
	// and verdict while attributing them to a different authority is a record that disagrees with this
	// lifecycle about who observed the ending.
	t.Run("a committed entry recording a different terminal authority", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinAuthor = state.TerminalByRecovery

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if !strings.Contains(err.Error(), "terminal authority") {
			t.Fatalf("err = %v, want it to name the field", err)
		}
	})

	t.Run("a committed entry recording a different tested commit", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinCommit = strings.Repeat("d", 40)

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	t.Run("a committed entry recording a different tested tree", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinTree = strings.Repeat("c", 40)

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	t.Run("committed with no entry to show for it", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinNoEntry = true

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
	})

	t.Run("the matching entry is accepted", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted

		res, err := Run(h.deps)
		if err != nil {
			t.Fatalf("a matching confirmation was rejected: %v", err)
		}
		if res.ResultDigest == "" {
			t.Fatal("no result digest returned")
		}
	})
}

// TestAChangedIdentityMustNameRealGitObjects.
//
// Non-empty and different is not "a valid observed identity that differs": arbitrary strings were
// durably publishable and routed to `fail`, spending the code-fix budget on an observation that cannot
// name a git object at all.
func TestAChangedIdentityMustNameRealGitObjects(t *testing.T) {
	valid := strings.Repeat("9", 40)
	for _, tc := range []struct {
		name string
		id   Identity
	}{
		{"neither is an oid", Identity{Value: state.TestIdentityChanged, Commit: "x", Tree: "y"}},
		{"the commit is not an oid", Identity{Value: state.TestIdentityChanged, Commit: "x", Tree: valid}},
		{"the tree is not an oid", Identity{Value: state.TestIdentityChanged, Commit: valid, Tree: "y"}},
		{"upper-case hex", Identity{Value: state.TestIdentityChanged, Commit: strings.Repeat("A", 40), Tree: valid}},
		{"the wrong length", Identity{Value: state.TestIdentityChanged, Commit: strings.Repeat("9", 39), Tree: valid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.identity = tc.id
			_, err := Run(h.deps)
			if err == nil || !strings.Contains(err.Error(), "not git object ids") {
				t.Fatalf("err = %v, want a git-identity refusal", err)
			}
			if h.r.did("publish-result") {
				t.Fatalf("it reached the durable result seam: %v", h.r.steps)
			}
		})
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

// TestTheStartRevisionIsReservedAndConstrainsTheBinding.
//
// The intent binds the revision the attempt becomes active at, and it is published BEFORE the append
// that creates that revision. Deriving it as head+1 would be wrong whenever the generation store skips
// an occupied or quarantined slot - and the wrong number would already be inside a durable digest by the
// time anything could notice. So it is read under the guard, handed to authorization, and it CONSTRAINS
// the append: the binding commits at exactly that revision or does not commit at all.
func TestTheStartRevisionIsReservedAndConstrainsTheBinding(t *testing.T) {
	t.Run("the reserved revision reaches authorization and the prepared attempt", func(t *testing.T) {
		h := newHarness(t)
		res, err := Run(h.deps)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if h.sawAuthorizeRevision != h.reservedRevision {
			t.Fatalf("authorization saw revision %d, reserved %d", h.sawAuthorizeRevision, h.reservedRevision)
		}
		if res.Prepared.StartRevision != h.reservedRevision {
			t.Fatalf("the prepared attempt carries revision %d, reserved %d", res.Prepared.StartRevision, h.reservedRevision)
		}
	})

	t.Run("reservation fails before anything is written", func(t *testing.T) {
		h := newHarness(t)
		h.failReserve = errors.New("store unreadable")

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("err = %v, want ErrRefused", err)
		}
		if h.r.did("authorize") || h.r.did("publish-intent") {
			t.Fatalf("work proceeded without a reserved revision: %v", h.r.steps)
		}
	})

	// The authorizer receives the reservation so it can bind it into the intent it digests. Returning a
	// different one would publish an intent naming a revision this lifecycle never reserved, and the
	// digest would make that permanent.
	t.Run("authorization may not substitute its own revision", func(t *testing.T) {
		h := newHarness(t)
		h.authorizedRevision = 999

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("err = %v, want ErrRefused", err)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("err = %v, want it to name the reservation", err)
		}
		if h.r.did("publish-intent") {
			t.Fatalf("an intent naming an unreserved revision was published: %v", h.r.steps)
		}
	})

	// The ORDINARY intervention: the reserved slot was taken while the guard was held, so the constrained
	// CAS cannot commit and says so. Nothing is bound, the residue is the orphaned intent the design
	// treats as ignorable, and the containment is this process's to destroy - it must NOT be handed to
	// recovery, because there is no attempt for recovery to own.
	t.Run("an intervening append leaves the binding not committed", func(t *testing.T) {
		h := newHarness(t)
		h.bindStatus, h.confirmStatus = BindNotCommitted, BindNotCommitted

		res, err := Run(h.deps)
		if !errors.Is(err, ErrOrphanedIntent) {
			t.Fatalf("err = %v, want ErrOrphanedIntent", err)
		}
		if res.Recovery != nil {
			t.Fatalf("an unbound attempt was handed to recovery: %+v", res.Recovery)
		}
		if h.cont.closed != 1 {
			t.Fatalf("the containment was closed %d times, want once", h.cont.closed)
		}
	})

	// A DEPENDENCY-CONTRACT VIOLATION, and deliberately not the intervention row above.
	//
	// State refuses a new active attempt whose start revision is not the revision that created it, and
	// refuses it before serialization, so a record committed at some other revision cannot exist
	// durably. A collaborator reporting one is reporting something impossible, and this process cannot
	// tell which half of the claim is false - so the containment is handed over on the strength of the
	// uncertainty, NOT because durable state is allowed to disagree with the published intent.
	t.Run("a binding that claims to have committed at another revision is a contract violation", func(t *testing.T) {
		h := newHarness(t)
		h.boundRevision = 44

		res, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if !strings.Contains(err.Error(), "44") || !strings.Contains(err.Error(), "41") {
			t.Fatalf("err = %v, want it to name both revisions", err)
		}
		if !strings.Contains(err.Error(), "cannot hold that record") {
			t.Fatalf("err = %v, want it to say the state is impossible rather than merely unexpected", err)
		}
		assertHandedOver(t, res, h.cont)
	})

	t.Run("a bound ledger entry with a different start revision", func(t *testing.T) {
		h := newHarness(t)
		h.finalizeStatus = BindUncertain
		h.confirmFinStatus = BindCommitted
		h.confirmFinStart = 77

		_, err := Run(h.deps)
		if !errors.Is(err, ErrRecoveryOwned) {
			t.Fatalf("err = %v, want ErrRecoveryOwned", err)
		}
		if !strings.Contains(err.Error(), "start revision") {
			t.Fatalf("err = %v, want it to name the field", err)
		}
	})
}

// TestAnAbsentExitCodeHasExactlyOneShape.
//
// Checking only the boolean let {HasExitCode:false, ExitCode:17} through, and it then travelled to every
// result seam claiming to carry no code while carrying seventeen. Whether that seventeen is serialized
// would depend on each publisher remembering to consult the boolean first, which makes it a fact two
// readers can legitimately disagree about.
func TestAnAbsentExitCodeHasExactlyOneShape(t *testing.T) {
	for _, e := range []state.TestExecution{
		state.TestExecutionTimeout, state.TestExecutionCancelled,
		state.TestExecutionSpawnFailed, state.TestExecutionInterrupted,
	} {
		t.Run(string(e), func(t *testing.T) {
			h := newHarness(t)
			h.cont.term = Terminal{Execution: e, TerminalReason: "x", HasExitCode: false, ExitCode: 17}

			_, err := Run(h.deps)
			if err == nil || !strings.Contains(err.Error(), "no exit code is claimed") {
				t.Fatalf("err = %v, want a refusal of the second representation of absent", err)
			}
			if h.r.did("publish-result") {
				t.Fatalf("it reached the durable result seam: %v", h.r.steps)
			}
		})
	}
}

// authorFor names the authority that can truthfully report an execution, so fixtures do not have to
// restate the rule the production validator applies.
func authorFor(e state.TestExecution) state.TerminalAuthor {
	if e == state.TestExecutionInterrupted {
		return state.TerminalByCoordinator
	}
	return state.TerminalByRunner
}

// TestARecordThatCannotBePublishedIsRefusedBeforeItIsHandedOver.
//
// The record is assembled on the main path from what authorization froze, and it is validated THERE
// rather than left for the publisher to discover it cannot be written - by which point the attempt is
// bound and past being retried cheaply. Nothing this lifecycle builds is normally invalid, which is
// exactly why the backstop needs a vector: it is otherwise a branch no test ever enters.
func TestARecordThatCannotBePublishedIsRefusedBeforeItIsHandedOver(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*PreparedAttempt)
		want  string
	}{
		{"no argv to say which command ran", func(p *PreparedAttempt) { p.Spec.Argv = nil }, "has no argv"},
		{"no executable", func(p *PreparedAttempt) { p.Spec.Executable = "" }, "names no executable"},
		{"no working directory", func(p *PreparedAttempt) { p.Spec.Cwd = "" }, "names no working directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			authorize := h.deps.Authorize
			h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
				p, err := authorize(rev)
				tc.spoil(&p)
				return p, err
			}

			_, err := Run(h.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a record refusal containing %q", err, tc.want)
			}
			if h.r.did("publish-result") {
				t.Fatalf("an unpublishable record reached the durable seam: %v", h.r.steps)
			}
		})
	}
}

// TestThePublishedRecordCarriesWhatOnlyTheRunnerSaw.
//
// The stream evidence and the terminal authority exist nowhere else: the runner is the only party that
// watched the streams, and only it knows whether the ending it is reporting is its own observation or a
// live coordinator's account of losing the supervisor. If the record did not carry them, whoever
// published it would have to find them through the side channel this package exists to remove - which is
// precisely what the earlier three-argument publisher forced.
func TestThePublishedRecordCarriesWhatOnlyTheRunnerSaw(t *testing.T) {
	t.Run("the runner's own observation, with both streams", func(t *testing.T) {
		h := newHarness(t)
		out := StreamRecord{Present: true, SourceBytes: 12, RedactedBytes: 3,
			SHA256: sha256Hex([]byte("abc")), Head: []byte("abc")}
		errS := StreamRecord{Present: true, SourceBytes: 9, RedactedBytes: 2,
			SHA256: sha256Hex([]byte("hi")), Head: []byte("hi")}
		h.cont.term = Terminal{
			Execution: state.TestExecutionOK, TerminalReason: "exited 0", Author: state.TerminalByRunner,
			HasExitCode: true, Stdout: out, Stderr: errS,
		}

		if _, err := Run(h.deps); err != nil {
			t.Fatalf("Run: %v", err)
		}
		rec := h.sawResultRecord
		if rec == nil {
			t.Fatal("nothing was published")
		}
		if !bytes.Equal(rec.Stdout.Head, []byte("abc")) || !bytes.Equal(rec.Stderr.Head, []byte("hi")) {
			t.Fatalf("the record lost the stream evidence: %+v / %+v", rec.Stdout, rec.Stderr)
		}
		if rec.Stdout.SourceBytes != 12 || rec.Stdout.RedactedBytes != 3 {
			t.Fatalf("the record collapsed the separately named counts: %+v", rec.Stdout)
		}
		if rec.TerminalAuthor != state.TerminalByRunner {
			t.Fatalf("authority = %q, want the runner's", rec.TerminalAuthor)
		}
		// And the record it published is one that could actually be written.
		if _, _, err := rec.Encode(); err != nil {
			t.Fatalf("the published record cannot be encoded: %v", err)
		}
	})

	// A LIVE coordinator that watched its supervisor die authors `interrupted`. Nobody saw the command
	// end, so the runner cannot be the authority - and if the record simply assumed the runner, the
	// ledger would attribute an inference to an observer that never made it.
	t.Run("a live coordinator's account of an interruption", func(t *testing.T) {
		h := newHarness(t)
		h.cont.term = Terminal{
			Execution: state.TestExecutionInterrupted, TerminalReason: "the supervisor stopped responding",
			Author: state.TerminalByCoordinator,
		}
		h.identity = Identity{Value: state.TestIdentityUnobserved}

		res, err := Run(h.deps)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if h.sawResultRecord.TerminalAuthor != state.TerminalByCoordinator {
			t.Fatalf("authority = %q, want the coordinator's", h.sawResultRecord.TerminalAuthor)
		}
		if res.Outcome == state.OutcomePass || res.Outcome == state.OutcomeFail {
			t.Fatalf("an interruption routed to %q", res.Outcome)
		}
	})

	// The runner cannot author an interruption: had it been alive to report, it would have reported the
	// outcome instead.
	t.Run("the runner claiming an interruption is refused", func(t *testing.T) {
		h := newHarness(t)
		h.cont.term = Terminal{
			Execution: state.TestExecutionInterrupted, TerminalReason: "x", Author: state.TerminalByRunner,
		}
		_, err := Run(h.deps)
		if err == nil || !strings.Contains(err.Error(), "cannot have observed it") {
			t.Fatalf("err = %v, want an authority refusal", err)
		}
		if h.r.did("publish-result") {
			t.Fatalf("it reached the durable seam: %v", h.r.steps)
		}
	})
}

// TestTheBoundDigestMustIdentifyTheRecordThatWasPublished.
//
// The outcome CAS binds whatever digest publication reports. Without an independently computed
// expectation the ledger can name bytes nobody produced, and every later reader fetches something else
// or nothing at all - the same hole intent publication was corrected for, one step later.
func TestTheBoundDigestMustIdentifyTheRecordThatWasPublished(t *testing.T) {
	h := newHarness(t)
	h.publishedResultDigest = strings.Repeat("e", 64)

	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "canonical digest is") {
		t.Fatalf("err = %v, want a refusal naming both digests", err)
	}
	if h.r.did("finalize") {
		t.Fatalf("an unverifiable digest reached the outcome CAS: %v", h.r.steps)
	}
}

// TestTheCombinedRetainedOutputRespectsTheFrozenCeiling.
//
// The policy bounds the two streams TOGETHER. Checking them separately admits a record twice the size
// the operator allowed, and the bound has to be applied where the record is built, because the publisher
// does not hold the policy either.
func TestTheCombinedRetainedOutputRespectsTheFrozenCeiling(t *testing.T) {
	h := newHarness(t)
	authorize := h.deps.Authorize
	h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
		p, err := authorize(rev)
		p.MaxOutputBytes = 8
		return rebind(t, p), err
	}
	half := []byte("12345")
	h.cont.term = Terminal{
		Execution: state.TestExecutionOK, TerminalReason: "exited 0", Author: state.TerminalByRunner,
		HasExitCode: true,
		// Each stream is within the ceiling; together they are not.
		Stdout: StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 5, SHA256: sha256Hex(half), Head: half},
		Stderr: StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 5, SHA256: sha256Hex(half), Head: half},
	}

	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "over the frozen ceiling") {
		t.Fatalf("err = %v, want a combined-ceiling refusal", err)
	}
	if h.r.did("publish-result") {
		t.Fatalf("an oversized record reached the durable seam: %v", h.r.steps)
	}
}

// TestTheRecordBindsTheWorkingDirectoryTheAttemptRanIn.
//
// A result that names the command but not where it ran describes the same argv in a different directory
// and still looks consistent with everything else in the record.
func TestTheRecordBindsTheWorkingDirectoryTheAttemptRanIn(t *testing.T) {
	h := newHarness(t)
	if _, err := Run(h.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(h.sawResultRecord.View.Cwd) != theSpec.Cwd {
		t.Fatalf("the record says cwd %q, the attempt ran in %q", h.sawResultRecord.View.Cwd, theSpec.Cwd)
	}
	// ONE digest comparison, which is the point of the shared view: a field added to the execution
	// description is bound everywhere at once, rather than in the places somebody remembered.
	wantView, err := theSpec.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	got, gerr := h.sawResultRecord.View.Digest()
	want, werr := wantView.Digest()
	if gerr != nil || werr != nil {
		t.Fatalf("digesting: %v / %v", gerr, werr)
	}
	if got != want {
		t.Fatalf("the record describes execution %s, the attempt armed %s", got, want)
	}
}

// TestASkewedExcerptIsRefusedAtTheCollaboratorBoundary.
//
// The allocation is only a rule if something applies it, and the frozen budget lives with the attempt
// rather than in the record - so this is the boundary where it can be applied at all.
func TestASkewedExcerptIsRefusedAtTheCollaboratorBoundary(t *testing.T) {
	h := newHarness(t)
	authorize := h.deps.Authorize
	h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
		p, err := authorize(rev)
		p.MaxOutputBytes = 8
		return rebind(t, p), err
	}
	// The right total, divided the wrong way: all head, no tail.
	h.cont.term = Terminal{
		Execution: state.TestExecutionOK, TerminalReason: "exited 0", Author: state.TerminalByRunner,
		HasExitCode: true,
		Stdout: StreamRecord{Present: true, SourceBytes: 100, RedactedBytes: 100,
			SHA256: strings.Repeat("3c", 32), Head: Bytes("01234567"), Truncated: true},
	}

	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "allocates") {
		t.Fatalf("err = %v, want a split-rule refusal", err)
	}
	if h.r.did("publish-result") {
		t.Fatalf("a skewed excerpt reached the durable seam: %v", h.r.steps)
	}
}

// TestTheIntentIsBuiltAndDIGESTEDOnTheMainPath.
//
// The intent type existed and nothing on the live path constructed one: publication received the
// prepared attempt, and the reference bound whatever digest authorization claimed. That is the same
// self-reported-identity defect the result path had to be corrected for, on the other artifact.
func TestTheIntentIsBuiltAndDIGESTEDOnTheMainPath(t *testing.T) {
	h := newHarness(t)
	if _, err := Run(h.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec := h.sawIntentRecord
	if rec == nil {
		t.Fatal("no intent record was published")
	}
	if rec.AttemptID != thePrepared.AttemptID || rec.StartRevision != thePrepared.StartRevision ||
		rec.TestedCommit != thePrepared.TestedCommit || rec.TestedTree != thePrepared.TestedTree {
		t.Fatalf("the intent does not describe the attempt it is for: %+v", rec)
	}
	// The reserved revision is bound, so the attempt can be placed in the run's history at all.
	if rec.StartRevision == 0 {
		t.Fatal("the intent binds no start revision")
	}
	if rec.SpecDigest != theSpec.Digest {
		t.Fatalf("the intent binds spec %q, the attempt armed %q", rec.SpecDigest, theSpec.Digest)
	}
	// BOTH frozen bounds, because they measure different things and neither implies the other.
	if rec.MaxOutputBytes != thePrepared.MaxOutputBytes {
		t.Fatalf("the intent binds output ceiling %d, the attempt froze %d", rec.MaxOutputBytes, thePrepared.MaxOutputBytes)
	}
	if rec.MaxRecordBytes != thePrepared.MaxRecordBytes {
		t.Fatalf("the intent binds record ceiling %d, the attempt froze %d", rec.MaxRecordBytes, thePrepared.MaxRecordBytes)
	}
	_, digest, err := rec.Encode()
	if err != nil {
		t.Fatalf("the published intent cannot be encoded: %v", err)
	}
	if digest != thePrepared.IntentDigest {
		t.Fatalf("the reference binds %q but the published intent is %q", thePrepared.IntentDigest, digest)
	}
}

// TestAnAuthorizationThatMISDESCRIBESItsIntentIsRefused.
//
// The reference's intent digest used to be taken on trust. If it names something other than the intent
// this attempt is about to publish, the durable reference points at bytes nobody wrote.
func TestAnAuthorizationThatMISDESCRIBESItsIntentIsRefused(t *testing.T) {
	h := newHarness(t)
	authorize := h.deps.Authorize
	h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
		p, err := authorize(rev)
		p.IntentDigest = strings.Repeat("b", 64)
		return p, err
	}

	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "but the intent it describes is") {
		t.Fatalf("err = %v, want a refusal naming both digests", err)
	}
	if h.r.did("publish-intent") {
		t.Fatalf("a misdescribed intent was published: %v", h.r.steps)
	}
}

// TestTheCanonicalRecordRespectsItsOwnCeiling.
//
// The raw output bound does not prove the encoded record fits: the excerpt is base64 in the record, and
// the argv, environment names and terminal account around it are variable-length.
func TestTheCanonicalRecordRespectsItsOwnCeiling(t *testing.T) {
	h := newHarness(t)
	authorize := h.deps.Authorize
	h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
		p, err := authorize(rev)
		p.MaxRecordBytes = 200 // smaller than this attempt's own metadata
		return rebind(t, p), err
	}

	_, err := Run(h.deps)
	if err == nil || !strings.Contains(err.Error(), "over the frozen ceiling of 200") {
		t.Fatalf("err = %v, want a record-ceiling refusal", err)
	}
	if h.r.did("publish-result") {
		t.Fatalf("an unstorable record reached the durable seam: %v", h.r.steps)
	}
}

// TestAnIntentThatCannotBeBuiltIsARefusalNotATeardown.
//
// Nothing durable exists at that point - no intent, no containment, no active reference - so reporting
// it in the lifecycle class told an operator an attempt existed and was torn down, about a spec that was
// never written anywhere.
func TestAnIntentThatCannotBeBuiltIsARefusalNotATeardown(t *testing.T) {
	h := newHarness(t)
	authorize := h.deps.Authorize
	h.deps.Authorize = func(rev uint64) (PreparedAttempt, error) {
		p, err := authorize(rev)
		p.Spec.Digest = "not-a-digest"
		return p, err
	}

	_, err := Run(h.deps)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if errors.Is(err, ErrOrphanedIntent) || errors.Is(err, ErrRecoveryOwned) {
		t.Fatalf("err = %v claims residue exists", err)
	}
	if h.r.did("publish-intent") || h.r.did("arm") {
		t.Fatalf("work proceeded past an unbuildable intent: %v", h.r.steps)
	}
}
