package testgate

import (
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

func validRecord() ResultRecord {
	return ResultRecord{
		AttemptID: theAttemptID, TestedCommit: theCommit, TestedTree: theTree,
		ResolvedExecutable: "/usr/bin/go", ResolvedArgv: []string{"go", "test", "./..."},
		EnvNames: []string{"PATH"}, EnvDigest: strings.Repeat("5e", 32),
		Execution: state.TestExecutionOK, Identity: state.TestIdentityUnchanged,
		TerminalReason: "exited 0", TerminalAuthor: state.TerminalByRunner,
		HasExitCode: true, ExitCode: 0,
	}
}

// TestAResultRecordIsRefusedWhenItContradictsItself.
//
// The validator was reachable only through the recovery plan, which builds a correct record every time -
// so most of these rules had never been executed by any test. A validator nothing exercises is a
// validator nobody knows works, and the plan test would keep passing if half of it were deleted.
func TestAResultRecordIsRefusedWhenItContradictsItself(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*ResultRecord)
		want   string
	}{
		{"no attempt", func(r *ResultRecord) { r.AttemptID = "" }, "names no attempt"},
		{"a commit that is not an oid", func(r *ResultRecord) { r.TestedCommit = "x" }, "not git object ids"},
		{"a tree that is not an oid", func(r *ResultRecord) { r.TestedTree = "x" }, "not git object ids"},
		// A verifier is expected to be able to see WHICH argv ran; a record without it cannot answer
		// the question it exists to answer.
		{"no executable", func(r *ResultRecord) { r.ResolvedExecutable = "" }, "records no command"},
		{"no argv", func(r *ResultRecord) { r.ResolvedArgv = nil }, "records no command"},
		{"an environment digest that is not a digest", func(r *ResultRecord) { r.EnvDigest = "nope" }, "is not a sha256"},
		{"an unknown execution", func(r *ResultRecord) { r.Execution = "probably-fine" }, "unknown execution"},
		{"an unknown identity", func(r *ResultRecord) { r.Identity = "probably-fine" }, "unknown identity"},
		{"no terminal authority", func(r *ResultRecord) { r.TerminalAuthor = "" }, "no known authority"},
		{"an invented terminal authority", func(r *ResultRecord) { r.TerminalAuthor = "the operator" }, "no known authority"},
		// Only an interrupted attempt has no runner left to report an ending, so only it may carry a
		// recovery-authored account. Anything else would present an inference as an observation.
		{"recovery authoring an ok account", func(r *ResultRecord) { r.TerminalAuthor = state.TerminalByRecovery },
			"carries a recovery-authored terminal account"},
		{"a non-canonical account", func(r *ResultRecord) { r.TerminalReason = "token=" + strings.Repeat("a", 40) },
			"not canonical"},
		{"an oversized account", func(r *ResultRecord) { r.TerminalReason = strings.Repeat("x", state.MaxTerminalReasonBytes+1) },
			"limit"},
		{"an absent exit code carrying a value", func(r *ResultRecord) { r.HasExitCode, r.ExitCode = false, 17 },
			"no exit code is claimed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := validRecord()
			tc.break_(&rec)
			err := rec.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if err := validRecord().validate(); err != nil {
		t.Fatalf("the baseline record was refused, so every case above may be passing for that reason: %v", err)
	}
}

// TestAStreamRecordIsRefusedWhenItsCountsContradictItself.
//
// The design names three quantities apart precisely because redaction CHANGES LENGTH, and a producer and
// a verifier could otherwise agree on a schema while attesting different things. These rules are what
// stop the three collapsing back into one.
func TestAStreamRecordIsRefusedWhenItsCountsContradictItself(t *testing.T) {
	valid := StreamRecord{
		Present: true, SourceBytes: 4096, RedactedBytes: 4000,
		SHA256: strings.Repeat("3c", 32), Retained: []byte("abc"), Truncated: true,
	}
	if err := valid.validate("stdout"); err != nil {
		t.Fatalf("the baseline stream was refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		s    StreamRecord
		want string
	}{
		// Absent has ONE shape, or "no evidence" and "evidence nobody looked at" become
		// indistinguishable and every reader has to remember to check the flag first.
		{"absent carrying counts", StreamRecord{SourceBytes: 12}, "absent but still carries evidence"},
		{"absent carrying a digest", StreamRecord{SHA256: strings.Repeat("3c", 32)}, "absent but still carries evidence"},
		{"absent carrying bytes", StreamRecord{Retained: []byte("x")}, "absent but still carries evidence"},
		{"absent claiming truncation", StreamRecord{Truncated: true}, "absent but still carries evidence"},
		{"a digest that is not a digest", func() StreamRecord { s := valid; s.SHA256 = "nope"; return s }(), "is not a sha256"},
		// The excerpt is drawn FROM the redacted stream, so it cannot be longer than it.
		{"an excerpt longer than the stream", func() StreamRecord {
			s := valid
			s.RedactedBytes, s.Retained, s.Truncated = 2, []byte("abcd"), false
			return s
		}(), "retains 4 bytes of a 2-byte redacted stream"},
		// Truncation is a FACT about the excerpt, not a free-standing flag. Left independent, a producer
		// could keep everything and still claim truncation, or drop bytes and deny it.
		{"claiming truncation while keeping everything", func() StreamRecord {
			s := valid
			s.RedactedBytes, s.Retained = 3, []byte("abc")
			return s
		}(), "claims truncated=true"},
		{"denying truncation while dropping bytes", func() StreamRecord {
			s := valid
			s.Truncated = false
			return s
		}(), "claims truncated=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.validate("stdout")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	// A present stream that kept the whole redacted stream is legitimate and must not be refused.
	whole := StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 3, SHA256: strings.Repeat("3c", 32), Retained: []byte("abc")}
	if err := whole.validate("stderr"); err != nil {
		t.Fatalf("an untruncated stream was refused: %v", err)
	}
	if whole.RetainedBytes() != 3 {
		t.Fatalf("RetainedBytes = %d, want the length of the excerpt", whole.RetainedBytes())
	}
}

// TestTheRecordCarriesItsOwnBytesRatherThanTheCallersBackingArray.
//
// The retained excerpt is the one piece of evidence here that is bytes rather than scalars, so it is the
// one place a shallow copy shares a backing array - the aliasing class slice 3b had to close nine times.
func TestTheRecordCarriesItsOwnBytesRatherThanTheCallersBackingArray(t *testing.T) {
	original := []byte("keep-me")
	src := StreamRecord{
		Present: true, SourceBytes: 9, RedactedBytes: 7,
		SHA256: strings.Repeat("3c", 32), Retained: original,
	}
	clone := cloneStream(src)
	for i := range original {
		original[i] = 'X'
	}
	if string(clone.Retained) != "keep-me" {
		t.Fatalf("the clone shares the caller's backing array: %q", clone.Retained)
	}

	rec := validRecord()
	rec.ResolvedArgv = []string{"go", "test"}
	rec.EnvNames = []string{"PATH"}
	rec.Stdout = StreamRecord{Present: true, SourceBytes: 4, RedactedBytes: 3,
		SHA256: strings.Repeat("3c", 32), Retained: []byte("abc")}
	c := cloneRecord(&rec)
	rec.ResolvedArgv[0] = "MUTATED"
	rec.EnvNames[0] = "MUTATED"
	rec.Stdout.Retained[0] = 'X'
	if c.ResolvedArgv[0] != "go" || c.EnvNames[0] != "PATH" || string(c.Stdout.Retained) != "abc" {
		t.Fatalf("the record clone shares backing arrays with its source: %+v", c)
	}
}
