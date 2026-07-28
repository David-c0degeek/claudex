package testgate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ResultRecordVersion is the on-disk schema version of a result record; an unknown version fails
// closed rather than being read as the current shape.
const ResultRecordVersion = 1

// ElisionMarker separates the head and tail of a truncated excerpt.
//
// The excerpt rule is head+tail with an EXPLICIT marker rather than a silent cut, so a reader can see
// that bytes are missing and where. Without the marker a truncated excerpt is indistinguishable from
// output that genuinely contained the two halves adjacent to each other.
var ElisionMarker = []byte("\n...[claudex: output elided]...\n")

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
	SchemaVersion int    `json:"schema_version"`
	AttemptID     string `json:"attempt_id"`
	TestedCommit  string `json:"tested_commit"`
	TestedTree    string `json:"tested_tree"`

	// ResolvedExecutable and ResolvedArgv are the command that actually ran, resolved at attempt start
	// rather than re-derived. A verifier is expected to be able to see WHICH argv ran.
	ResolvedExecutable string   `json:"resolved_executable"`
	ResolvedArgv       []string `json:"resolved_argv"`
	// EnvNames are the sorted frozen variable names; EnvDigest is over the canonical ACTUAL name/value
	// list, not a redacted one - redacted pairs collapse distinct secrets onto one marker, so a
	// redaction-based digest cannot prove equality.
	EnvNames  []string `json:"env_names"`
	EnvDigest string   `json:"env_digest"`

	// Execution and Identity are the two independent halves of the outcome.
	Execution state.TestExecution `json:"execution"`
	Identity  state.TestIdentity  `json:"identity"`
	// TerminalReason is the account of how the command ended, and TerminalAuthor says who wrote it.
	// Recovery authors the account for an interrupted attempt because no runner survived to report one.
	TerminalReason string               `json:"terminal_reason"`
	TerminalAuthor state.TerminalAuthor `json:"terminal_author"`
	// HasExitCode and ExitCode carry the optional exit fact by value, for the same reason the runner's
	// terminal does: a pointer is a validated fact somebody else can still rewrite.
	HasExitCode bool `json:"has_exit_code"`
	ExitCode    int  `json:"exit_code"`

	Stdout StreamRecord `json:"stdout"`
	Stderr StreamRecord `json:"stderr"`
}

// StreamRecord is one stream's evidence, with every quantity named apart.
//
// The design is explicit that "raw byte counts before encoding" was not a definition: redaction CHANGES
// LENGTH, so a producer and a verifier could agree on a schema while attesting different quantities.
// Three counts, not one.
type StreamRecord struct {
	// Present says whether this stream has evidence at all. A crash can leave one stream readable and
	// the other absent, so the two are modelled independently rather than behind one flag.
	Present bool `json:"present"`
	// SourceBytes is what was read FROM THE PROCESS, before redaction.
	SourceBytes uint64 `json:"source_bytes"`
	// RedactedBytes is the length of the full redacted stream - the stream the digest covers. It differs
	// from SourceBytes whenever a replacement differs in length from the token it replaced.
	RedactedBytes uint64 `json:"redacted_bytes"`
	// SHA256 is over the ENTIRE redacted stream, computed streaming. Runner-attested: nothing else saw
	// the whole stream, so it is trusted at the same level as the exit code and is NOT recomputable
	// from this record.
	SHA256 string `json:"sha256"`
	// Retained is the bounded excerpt actually kept, as exact bytes. It is base64 on the wire so the
	// record is self-contained and lossless for output that is not valid UTF-8.
	Retained []byte `json:"retained_b64"`
	// Truncated says whether the excerpt is short of the full redacted stream.
	Truncated bool `json:"truncated"`
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
	// The SAME truth table the acceptance boundary applies, not a second opinion about it. Checking only
	// that the enums are known accepted `ok` with no exit code, `ok` with 17, `nonzero` with 0, and a
	// timeout carrying a code - facts publication or state would later reject, or worse, that
	// state.Outcome routes as a pass.
	if err := checkTerminalFacts(r.Execution, r.Identity, r.TerminalReason, r.TerminalAuthor,
		r.HasExitCode, r.ExitCode); err != nil {
		return err
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

// checkTerminalFacts is the one place the terminal vocabulary, the exit fact, the authority and the
// account are checked against each other.
//
// It exists because the same rules were being spelled out in two places, and the two disagreed: the
// record boundary admitted contradictions the lifecycle had already learned to refuse. Two validators
// for one fact is one validator and one liability.
func checkTerminalFacts(e state.TestExecution, id state.TestIdentity, reason string,
	author state.TerminalAuthor, hasExit bool, exit int) error {
	if !state.KnownTestExecution(e) {
		return fmt.Errorf("%w: the unknown execution %q", ErrLifecycle, e)
	}
	if id != "" && !state.KnownTestIdentity(id) {
		return fmt.Errorf("%w: the unknown identity %q", ErrLifecycle, id)
	}
	// The exit fact must AGREE with the verdict, not merely be present.
	switch e {
	case state.TestExecutionOK:
		if !hasExit || exit != 0 {
			return fmt.Errorf("%w: %q requires exit code 0, got %s", ErrLifecycle, e, exitText(hasExit, exit))
		}
	case state.TestExecutionNonzero:
		if !hasExit || exit == 0 {
			return fmt.Errorf("%w: %q requires a non-zero exit code, got %s", ErrLifecycle, e, exitText(hasExit, exit))
		}
		if exit < 0 {
			return fmt.Errorf("%w: %q reported the impossible exit code %d", ErrLifecycle, e, exit)
		}
	default:
		// A timeout, a cancel, a spawn failure and an interruption all end without the command
		// reporting anything, so a code here would be invented.
		if hasExit {
			return fmt.Errorf("%w: %q cannot carry an exit code", ErrLifecycle, e)
		}
	}
	// Absent has ONE representation, so whether the carried value is trusted never depends on the
	// reader consulting the flag first.
	if !hasExit && exit != 0 {
		return fmt.Errorf("%w: no exit code is claimed but %d is carried", ErrLifecycle, exit)
	}
	if !state.KnownTerminalAuthor(author) {
		return fmt.Errorf("%w: the terminal account names no known authority", ErrLifecycle)
	}
	if !state.AuthorityAgreesWithExecution(author, e) {
		return fmt.Errorf("%w: a %q account authored by %q, which cannot have observed it", ErrLifecycle, e, author)
	}
	// The ledger requires a non-blank account, so accepting one here would build a record that cannot
	// be finalized - discovered only after it is durable.
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: the terminal account is blank", ErrLifecycle)
	}
	if reason != state.CanonicalTerminalReason(reason) {
		return fmt.Errorf("%w: the terminal account is not canonical", ErrLifecycle)
	}
	if len(reason) > state.MaxTerminalReasonBytes {
		return fmt.Errorf("%w: the terminal account is %d bytes after canonicalization, limit %d",
			ErrLifecycle, len(reason), state.MaxTerminalReasonBytes)
	}
	return nil
}

func exitText(has bool, code int) string {
	if !has {
		return "none"
	}
	return fmt.Sprintf("%d", code)
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
	if !s.Truncated {
		// The record is SELF-CONTAINED here: the retained bytes ARE the whole redacted stream, so the
		// digest is recomputable and must match. Checking only its grammar let a record disagree with
		// its own bound digest while every other rule passed - and the design's own boundary says this
		// is one of the facts a verifier recomputes rather than trusts.
		if got := sha256Hex(s.Retained); got != s.SHA256 {
			return fmt.Errorf("%w: %s retains the whole stream but hashes to %q, not the bound %q",
				ErrLifecycle, what, got, s.SHA256)
		}
		return nil
	}
	// A truncated excerpt is head+tail with an EXPLICIT marker, so a reader can see that bytes are
	// missing and where. A silent cut is indistinguishable from output that really did contain the two
	// halves adjacent to each other.
	if !bytes.Contains(s.Retained, ElisionMarker) {
		return fmt.Errorf("%w: %s is truncated but carries no elision marker", ErrLifecycle, what)
	}
	if idx := bytes.Index(s.Retained, ElisionMarker); idx != bytes.LastIndex(s.Retained, ElisionMarker) {
		return fmt.Errorf("%w: %s carries more than one elision marker, so the excerpt is not head+tail",
			ErrLifecycle, what)
	}
	return nil
}

// FitsOutputCeiling reports whether the excerpt respects the policy ceiling.
//
// It is separate from validate because the ceiling lives in the run policy and the record does not carry
// it. Keeping it out of validate would have been the easy thing; naming it here means a caller that has
// the policy has no excuse for not applying it.
func (s StreamRecord) FitsOutputCeiling(max uint64) bool { return s.RetainedBytes() <= max }

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

// Encode renders the record as canonical bytes and returns them with their digest.
//
// A record type with no encoder was not a canonical record at all - it was an in-memory struct that
// claimed to be one, and every "the digest identifies these bytes" statement around it was unbacked.
// The record is validated BEFORE it is encoded, so nothing unpublishable can acquire a digest.
func (r ResultRecord) Encode() ([]byte, string, error) {
	if r.SchemaVersion != ResultRecordVersion {
		return nil, "", fmt.Errorf("%w: result record schema version %d, want %d",
			ErrLifecycle, r.SchemaVersion, ResultRecordVersion)
	}
	if err := r.validate(); err != nil {
		return nil, "", err
	}
	raw, err := canonjson.CanonicalizeValue(r)
	if err != nil {
		return nil, "", fmt.Errorf("%w: canonicalizing the result record: %v", ErrLifecycle, err)
	}
	return raw, sha256Hex(raw), nil
}

// DecodeResultRecord strictly decodes canonical bytes into a validated record.
//
// Strict, because an unknown field, a duplicate key or a trailing object is a document written by
// something that disagrees with this schema, and guessing which parts to honour is how two readers end
// up with two different records. Validated, because a caller that receives an unvalidated struct has to
// remember to validate it, and the whole point of this boundary is that it does not depend on anyone
// remembering.
func DecodeResultRecord(raw []byte) (ResultRecord, error) {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	// The version is read BEFORE the strict decode, so a document from another version gets version
	// remediation rather than a confusing complaint about a field it was never supposed to have.
	//
	// Trailing content is named here rather than being reported as "not JSON", which is what a plain
	// Unmarshal calls it: the document parsed fine and then something else followed it, and telling an
	// operator their file is not JSON would send them looking for the wrong thing.
	probeDec := json.NewDecoder(bytes.NewReader(raw))
	if err := probeDec.Decode(&probe); err != nil {
		return ResultRecord{}, fmt.Errorf("%w: the result record is not JSON: %v", ErrLifecycle, err)
	}
	if probeDec.More() {
		return ResultRecord{}, fmt.Errorf("%w: the result record has trailing content", ErrLifecycle)
	}
	if probe.SchemaVersion != ResultRecordVersion {
		return ResultRecord{}, fmt.Errorf("%w: result record schema version %d, want %d",
			ErrLifecycle, probe.SchemaVersion, ResultRecordVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rec ResultRecord
	if err := dec.Decode(&rec); err != nil {
		return ResultRecord{}, fmt.Errorf("%w: decoding the result record: %v", ErrLifecycle, err)
	}
	if dec.More() {
		return ResultRecord{}, fmt.Errorf("%w: the result record has trailing content", ErrLifecycle)
	}
	if err := rec.validate(); err != nil {
		return ResultRecord{}, err
	}
	return rec, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
