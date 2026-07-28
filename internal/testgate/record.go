package testgate

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/state"
)

// ResultRecord is the ONE immutable record an attempt produces, in the shape design section 3 and
// section 5 pin.
//
// It exists as a type because two places have to build one and neither may guess at its shape: the
// lifecycle publishes it when a command ends, and recovery publishes it when settling an attempt whose
// runner died. An earlier version of the recovery decision carried a handful of the fields the ledger
// happens to bind and called that "the result", which meant the record could not actually be written
// from it - the resolved command, the environment identity and the retained stream evidence were simply
// missing, and whoever executed the decision would have had to find them again by ambient re-read or
// invent them.
type ResultRecord struct {
	AttemptID    string
	TestedCommit string
	TestedTree   string

	// ResolvedExecutable and ResolvedArgv are the command that actually ran, resolved at attempt start
	// rather than re-derived. A verifier is expected to be able to see WHICH argv ran.
	ResolvedExecutable string
	ResolvedArgv       []string
	// EnvNames are the sorted frozen variable names; EnvDigest is over the canonical ACTUAL name/value
	// list, not a redacted one - redacted pairs collapse distinct secrets onto one marker, so a
	// redaction-based digest cannot prove equality.
	EnvNames  []string
	EnvDigest string

	// Execution and Identity are the two independent halves of the outcome.
	Execution state.TestExecution
	Identity  state.TestIdentity
	// TerminalReason is the account of how the command ended, and TerminalAuthor says who wrote it.
	// Recovery authors the account for an interrupted attempt because no runner survived to report one.
	TerminalReason string
	TerminalAuthor state.TerminalAuthor
	// HasExitCode and ExitCode carry the optional exit fact by value, for the same reason the runner's
	// terminal does: a pointer is a validated fact somebody else can still rewrite.
	HasExitCode bool
	ExitCode    int

	Stdout StreamRecord
	Stderr StreamRecord
}

// StreamRecord is one stream's evidence, with every quantity named apart.
//
// The design is explicit that "raw byte counts before encoding" was not a definition: redaction CHANGES
// LENGTH, so a producer and a verifier could agree on a schema while attesting different quantities.
// Three counts, not one.
type StreamRecord struct {
	// Present says whether this stream has evidence at all. A crash can leave one stream readable and
	// the other absent, so the two are modelled independently rather than behind one flag.
	Present bool
	// SourceBytes is what was read FROM THE PROCESS, before redaction.
	SourceBytes uint64
	// RedactedBytes is the length of the full redacted stream - the stream the digest covers. It differs
	// from SourceBytes whenever a replacement differs in length from the token it replaced.
	RedactedBytes uint64
	// SHA256 is over the ENTIRE redacted stream, computed streaming. Runner-attested: nothing else saw
	// the whole stream, so it is trusted at the same level as the exit code and is NOT recomputable
	// from this record.
	SHA256 string
	// Retained is the bounded excerpt actually kept, as exact bytes. It is base64 on the wire so the
	// record is self-contained and lossless for output that is not valid UTF-8.
	Retained []byte
	// Truncated says whether the excerpt is short of the full redacted stream.
	Truncated bool
}

// RetainedBytes is the count of the retained excerpt, derived rather than stored.
//
// Storing it beside the slice would be a second representation of one fact, and the two could disagree.
func (s StreamRecord) RetainedBytes() uint64 { return uint64(len(s.Retained)) }

// validate refuses a record that contradicts itself, before anything can publish it.
func (r ResultRecord) validate() error {
	if r.AttemptID == "" {
		return fmt.Errorf("%w: the result names no attempt", ErrLifecycle)
	}
	if !state.IsGitOID(r.TestedCommit) || !state.IsGitOID(r.TestedTree) {
		return fmt.Errorf("%w: the result is about %q/%q, which are not git object ids",
			ErrLifecycle, r.TestedCommit, r.TestedTree)
	}
	if r.ResolvedExecutable == "" || len(r.ResolvedArgv) == 0 {
		return fmt.Errorf("%w: the result records no command, so a verifier cannot see which argv ran", ErrLifecycle)
	}
	if !state.IsSHA256Hex(r.EnvDigest) {
		return fmt.Errorf("%w: the environment digest %q is not a sha256", ErrLifecycle, r.EnvDigest)
	}
	if !state.KnownTestExecution(r.Execution) {
		return fmt.Errorf("%w: the result records the unknown execution %q", ErrLifecycle, r.Execution)
	}
	if !state.KnownTestIdentity(r.Identity) {
		return fmt.Errorf("%w: the result records the unknown identity %q", ErrLifecycle, r.Identity)
	}
	if !state.KnownTerminalAuthor(r.TerminalAuthor) {
		return fmt.Errorf("%w: the terminal account names no known authority", ErrLifecycle)
	}
	// Only an interrupted attempt can carry a recovery-authored account, because it is the only row with
	// no runner left to report one. The same rule the ledger enforces, applied where the record is built
	// rather than only where it is stored.
	if r.TerminalAuthor == state.TerminalByRecovery && r.Execution != state.TestExecutionInterrupted {
		return fmt.Errorf("%w: a %q result carries a recovery-authored terminal account", ErrLifecycle, r.Execution)
	}
	if r.TerminalReason != state.CanonicalTerminalReason(r.TerminalReason) {
		return fmt.Errorf("%w: the terminal account is not canonical", ErrLifecycle)
	}
	if len(r.TerminalReason) > state.MaxTerminalReasonBytes {
		return fmt.Errorf("%w: the terminal account is %d bytes, limit %d",
			ErrLifecycle, len(r.TerminalReason), state.MaxTerminalReasonBytes)
	}
	if !r.HasExitCode && r.ExitCode != 0 {
		return fmt.Errorf("%w: no exit code is claimed but %d is carried", ErrLifecycle, r.ExitCode)
	}
	for _, st := range []struct {
		what string
		s    StreamRecord
	}{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
		if err := st.s.validate(st.what); err != nil {
			return err
		}
	}
	return nil
}

func (s StreamRecord) validate(what string) error {
	if !s.Present {
		// Absent has ONE shape. Otherwise "no evidence" and "evidence nobody looked at" become
		// indistinguishable, and whether the extra values are trusted depends on each reader checking
		// the flag first.
		if s.SourceBytes != 0 || s.RedactedBytes != 0 || s.SHA256 != "" || len(s.Retained) != 0 || s.Truncated {
			return fmt.Errorf("%w: %s is absent but still carries evidence", ErrLifecycle, what)
		}
		return nil
	}
	if !state.IsSHA256Hex(s.SHA256) {
		return fmt.Errorf("%w: the %s digest %q is not a sha256", ErrLifecycle, what, s.SHA256)
	}
	// The excerpt is drawn FROM the redacted stream, so it cannot be longer than it.
	if s.RetainedBytes() > s.RedactedBytes {
		return fmt.Errorf("%w: %s retains %d bytes of a %d-byte redacted stream",
			ErrLifecycle, what, s.RetainedBytes(), s.RedactedBytes)
	}
	// Truncation is a FACT about the excerpt, not a free-standing flag: it is true exactly when the
	// excerpt is short of the stream. Left independent, a producer could keep everything and still
	// claim truncation, or drop bytes and deny it.
	if s.Truncated != (s.RetainedBytes() < s.RedactedBytes) {
		return fmt.Errorf("%w: %s claims truncated=%t while retaining %d of %d redacted bytes",
			ErrLifecycle, what, s.Truncated, s.RetainedBytes(), s.RedactedBytes)
	}
	return nil
}

// cloneStream and cloneRecord hand out owned copies, including the retained bytes.
//
// The retained excerpt is a byte slice, so a shallow copy shares its backing array - the same aliasing
// class slice 3b had to close at nine seams, and the one place here where the evidence is bytes rather
// than scalars.
func cloneStream(s StreamRecord) StreamRecord {
	c := s
	if s.Retained != nil {
		c.Retained = append([]byte(nil), s.Retained...)
	}
	return c
}

func cloneRecord(r *ResultRecord) *ResultRecord {
	if r == nil {
		return nil
	}
	c := *r
	if r.ResolvedArgv != nil {
		c.ResolvedArgv = append([]string(nil), r.ResolvedArgv...)
	}
	if r.EnvNames != nil {
		c.EnvNames = append([]string(nil), r.EnvNames...)
	}
	c.Stdout = cloneStream(r.Stdout)
	c.Stderr = cloneStream(r.Stderr)
	return &c
}
