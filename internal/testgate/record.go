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

// ElisionMarker is what a RENDERER puts between the two halves when showing a truncated excerpt to a
// person. It is deliberately NOT part of the stored representation.
//
// An earlier version stored head, marker and tail in one blob and validated by searching for exactly
// one marker. Process output is arbitrary bytes, so that is unsound in both directions: a legitimate
// excerpt containing this byte sequence was rejected, and any blob containing one copy of it was
// accepted as a well-formed head+tail. The split is structural now, which makes collision impossible
// rather than unlikely.
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
	// View is the ONE description of what ran. Its digest is what proves this result describes the
	// execution that was armed, rather than a similar-looking one.
	View ExecutionView `json:"execution_view"`

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
	// Head and Tail are the two halves of the bounded excerpt, stored SEPARATELY so that no byte
	// sequence in the output can be mistaken for a boundary. Both are base64 on the wire, so the record
	// is self-contained and lossless for output that is not valid UTF-8.
	Head Bytes `json:"head_b64"`
	Tail Bytes `json:"tail_b64"`
	// Truncated says whether the excerpt is short of the full redacted stream.
	Truncated bool `json:"truncated"`
}

// RetainedBytes is the count of the retained excerpt, derived rather than stored.
//
// Storing it beside the bytes would be a second representation of one fact, and the two could disagree.
func (s StreamRecord) RetainedBytes() uint64 { return uint64(len(s.Head) + len(s.Tail)) }

// Retained is the excerpt as one slice, for callers that only want to read it. It is a COPY: handing
// out the record's own arrays would let a reader edit the evidence.
func (s StreamRecord) Retained() []byte {
	out := make([]byte, 0, len(s.Head)+len(s.Tail))
	out = append(out, s.Head...)
	return append(out, s.Tail...)
}

// CheckSplit verifies the excerpt against the DETERMINISTIC allocation for the budget it was given.
//
// Validation without this accepted any head/tail division of the right total length, so "head+tail" was
// a shape rather than a rule: an all-head excerpt and the documented split both passed, and two
// producers could keep different bytes and each call itself correct. It takes the budget because the
// record does not carry policy, which is why it lives at the collaborator boundary rather than inside
// validate.
func (s StreamRecord) CheckSplit(what string, budget uint64) error {
	if !s.Present || !s.Truncated {
		return nil
	}
	wantHead := (budget + 1) / 2
	wantTail := budget - wantHead
	if uint64(len(s.Head)) != wantHead || uint64(len(s.Tail)) != wantTail {
		return fmt.Errorf("%w: %s keeps %d+%d bytes, but a %d-byte budget allocates %d+%d",
			ErrLifecycle, what, len(s.Head), len(s.Tail), budget, wantHead, wantTail)
	}
	return nil
}

// AllocateOutputBudget divides ONE combined ceiling between the two streams.
//
// The policy bounds the streams together, and until the division is named a 100/0 allocation and a 50/50
// allocation both respect the same ceiling - so a verifier could not say which bytes should have been
// kept. A present stream gets an equal share; when only one is present it gets all of it; an odd byte
// goes to stdout.
func AllocateOutputBudget(stdoutPresent, stderrPresent bool, combined uint64) (outBudget, errBudget uint64) {
	switch {
	case stdoutPresent && stderrPresent:
		outBudget = (combined + 1) / 2
		return outBudget, combined - outBudget
	case stdoutPresent:
		return combined, 0
	case stderrPresent:
		return 0, combined
	}
	return 0, 0
}

// SplitExcerpt allocates a bounded excerpt DETERMINISTICALLY: the head takes the larger half of an odd
// budget, and the tail the rest.
//
// It is defined here, once, because "head+tail within the ceiling" is not a specification until the
// split is named - two producers obeying the same ceiling could otherwise keep different bytes and both
// call themselves correct, and a verifier could not tell which.
func SplitExcerpt(stream []byte, budget uint64) (head, tail []byte, truncated bool) {
	if uint64(len(stream)) <= budget {
		return append([]byte(nil), stream...), nil, false
	}
	headLen := (budget + 1) / 2
	tailLen := budget - headLen
	return append([]byte(nil), stream[:headLen]...),
		append([]byte(nil), stream[uint64(len(stream))-tailLen:]...), true
}

// validate refuses a record that contradicts itself, before anything can publish it.
func (r ResultRecord) validate() error {
	// The version is checked HERE rather than only at the encoder, because a record built without one
	// was passing every check its builder ran and then being refused at publication - which is the
	// worst place to find out, since the attempt is already bound by then.
	if r.SchemaVersion != ResultRecordVersion {
		return fmt.Errorf("%w: result record schema version %d, want %d",
			ErrLifecycle, r.SchemaVersion, ResultRecordVersion)
	}
	if r.AttemptID == "" {
		return fmt.Errorf("%w: the result names no attempt", ErrLifecycle)
	}
	if !state.IsGitOID(r.TestedCommit) || !state.IsGitOID(r.TestedTree) {
		return fmt.Errorf("%w: the result is about %q/%q, which are not git object ids",
			ErrLifecycle, r.TestedCommit, r.TestedTree)
	}
	if err := r.View.validate(); err != nil {
		return err
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
		if s.SourceBytes != 0 || s.RedactedBytes != 0 || s.SHA256 != "" ||
			len(s.Head) != 0 || len(s.Tail) != 0 || s.Truncated {
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
		if len(s.Tail) != 0 {
			return fmt.Errorf("%w: %s is untruncated but split into two halves", ErrLifecycle, what)
		}
		if got := sha256Hex(s.Head); got != s.SHA256 {
			return fmt.Errorf("%w: %s retains the whole stream but hashes to %q, not the bound %q",
				ErrLifecycle, what, got, s.SHA256)
		}
	}
	return nil
}

// FitsCombinedOutputCeiling reports whether the two streams TOGETHER respect the frozen policy bound.
//
// Combined, because that is what the policy bounds - checking each stream separately admits a record
// twice the size the operator allowed. It takes the ceiling as an argument because the record does not
// carry policy, and it is named on the type so a caller holding the frozen bound has no excuse.
func FitsCombinedOutputCeiling(stdout, stderr StreamRecord, max uint64) bool {
	return stdout.RetainedBytes()+stderr.RetainedBytes() <= max
}

// cloneStream and cloneRecord hand out owned copies, including the retained bytes.
//
// The retained excerpt is a byte slice, so a shallow copy shares its backing array - the same aliasing
// class slice 3b had to close at nine seams, and the one place here where the evidence is bytes rather
// than scalars.
func cloneStream(s StreamRecord) StreamRecord {
	c := s
	if s.Head != nil {
		c.Head = append(Bytes(nil), s.Head...)
	}
	if s.Tail != nil {
		c.Tail = append(Bytes(nil), s.Tail...)
	}
	return c
}

func cloneRecord(r *ResultRecord) *ResultRecord {
	if r == nil {
		return nil
	}
	c := *r
	c.View = cloneView(r.View)
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
	// The document must be EXACTLY what this schema would have written.
	//
	// DisallowUnknownFields alone is not the strict canonical boundary the comment used to claim: Go's
	// decoder matches keys case-insensitively, takes the last of a duplicated pair, ignores key order
	// and tolerates whitespace and explicit nulls. Every one of those produces a document that decodes
	// to this record while hashing differently - so one record would have had several durable digests,
	// and a digest that does not uniquely name a record is not an identity.
	//
	// Re-encoding and comparing settles all of them at once, and it cannot drift from the encoder
	// because it IS the encoder.
	reencoded, _, err := rec.Encode()
	if err != nil {
		return ResultRecord{}, err
	}
	if !bytes.Equal(reencoded, raw) {
		return ResultRecord{}, fmt.Errorf("%w: the result record is not in canonical form", ErrLifecycle)
	}
	return rec, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// isSHA256 is the record boundary's view of the one digest grammar.
func isSHA256(s string) bool { return state.IsSHA256Hex(s) }
