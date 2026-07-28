package testgate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

func validRecord() ResultRecord {
	return ResultRecord{
		AttemptID: theAttemptID, TestedCommit: theCommit, TestedTree: theTree,
		SchemaVersion: ResultRecordVersion,
		SpecDigest:    strings.Repeat("7a", 32),
		View: ExecutionView{
			Executable: Bytes("/usr/bin/go"),
			Argv:       ByteList{Bytes("go"), Bytes("test"), Bytes("./...")},
			Cwd:        Bytes("/work"),
			EnvNames:   ByteList{Bytes("PATH")},
			EnvDigest:  strings.Repeat("5e", 32),
		},
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
		{"no executable", func(r *ResultRecord) { r.View.Executable = nil }, "names no executable"},
		{"no argv", func(r *ResultRecord) { r.View.Argv = nil }, "has no argv"},
		{"no working directory", func(r *ResultRecord) { r.View.Cwd = nil }, "names no working directory"},
		{"an environment digest that is not a digest", func(r *ResultRecord) { r.View.EnvDigest = "nope" }, "is not a sha256"},
		// The view carries the environment IDENTITY; the spec carries its ordered VALUES, so neither
		// digest stands in for the other and both must be bound.
		{"no execution spec digest", func(r *ResultRecord) { r.SpecDigest = "" }, "execution spec digest"},
		{"no schema version", func(r *ResultRecord) { r.SchemaVersion = 0 }, "schema version"},

		{"an unknown execution", func(r *ResultRecord) { r.Execution = "probably-fine" }, "unknown execution"},
		{"an unknown identity", func(r *ResultRecord) { r.Identity = "probably-fine" }, "unknown identity"},
		{"no terminal authority", func(r *ResultRecord) { r.TerminalAuthor = "" }, "no known authority"},
		{"an invented terminal authority", func(r *ResultRecord) { r.TerminalAuthor = "the operator" }, "no known authority"},
		// Only an interrupted attempt has no runner left to report an ending, so only it may carry a
		// recovery-authored account. Anything else would present an inference as an observation.
		{"recovery authoring an ok account", func(r *ResultRecord) { r.TerminalAuthor = state.TerminalByRecovery },
			"cannot have observed it"},
		{"a non-canonical account", func(r *ResultRecord) { r.TerminalReason = "token=" + strings.Repeat("a", 40) },
			"not canonical"},
		{"an oversized account", func(r *ResultRecord) { r.TerminalReason = strings.Repeat("x", state.MaxTerminalReasonBytes+1) },
			"limit"},
		// A row that legitimately has no exit code, carrying one anyway - the shape check rather than the
		// agreement check.
		{"an absent exit code carrying a value", func(r *ResultRecord) {
			r.Execution, r.Identity = state.TestExecutionTimeout, state.TestIdentityUnobserved
			r.HasExitCode, r.ExitCode = false, 17
		}, "no exit code is claimed"},
		{"a timeout carrying an exit code", func(r *ResultRecord) {
			r.Execution, r.Identity = state.TestExecutionTimeout, state.TestIdentityUnobserved
			r.HasExitCode, r.ExitCode = true, 0
		}, "cannot carry an exit code"},
		{"ok with a non-zero code", func(r *ResultRecord) { r.ExitCode = 17 }, "requires exit code 0"},
		{"a blank account", func(r *ResultRecord) { r.TerminalReason = "   " }, "blank"},
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
		SHA256: strings.Repeat("3c", 32), Head: []byte("head"), Tail: []byte("tail"), Truncated: true,
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
		{"absent carrying bytes", StreamRecord{Head: []byte("x")}, "absent but still carries evidence"},
		{"absent claiming truncation", StreamRecord{Truncated: true}, "absent but still carries evidence"},
		{"a digest that is not a digest", func() StreamRecord { s := valid; s.SHA256 = "nope"; return s }(), "is not a sha256"},
		// The one fact in this record a verifier can RECOMPUTE, so it must be recomputed.
		{"an untruncated excerpt disagreeing with its own digest", StreamRecord{
			Present: true, SourceBytes: 3, RedactedBytes: 3,
			SHA256: strings.Repeat("3c", 32), Head: []byte("abc"),
		}, "hashes to"},
		// An untruncated excerpt is the WHOLE stream, so there is nothing for a tail to be the other
		// side of.
		{"an untruncated excerpt split into halves", StreamRecord{
			Present: true, SourceBytes: 3, RedactedBytes: 3,
			SHA256: sha256Hex([]byte("abc")), Head: []byte("ab"), Tail: []byte("c"),
		}, "split into two halves"},
		// The excerpt is drawn FROM the redacted stream, so it cannot be longer than it.
		{"an excerpt longer than the stream", func() StreamRecord {
			s := valid
			s.RedactedBytes, s.Head, s.Tail, s.Truncated = 2, []byte("abcd"), nil, false
			return s
		}(), "retains 4 bytes of a 2-byte redacted stream"},
		// Truncation is a FACT about the excerpt, not a free-standing flag. Left independent, a producer
		// could keep everything and still claim truncation, or drop bytes and deny it.
		{"claiming truncation while keeping everything", func() StreamRecord {
			s := valid
			s.RedactedBytes, s.Head, s.Tail = 3, []byte("abc"), nil
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
	// An untruncated excerpt IS the whole redacted stream, so its digest is recomputable and must match -
	// a fixture with a made-up digest would have been asserting that the record may disagree with itself.
	whole := StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 3,
		SHA256: sha256Hex([]byte("abc")), Head: []byte("abc")}
	if err := whole.validate("stderr"); err != nil {
		t.Fatalf("an untruncated stream was refused: %v", err)
	}
	if whole.RetainedBytes() != 3 || string(whole.Retained()) != "abc" {
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
		SHA256: sha256Hex(original), Head: original,
	}
	clone := cloneStream(src)
	for i := range original {
		original[i] = 'X'
	}
	if string(clone.Head) != "keep-me" {
		t.Fatalf("the clone shares the caller's backing array: %q", clone.Head)
	}

	rec := validRecord()
	rec.View.Argv = ByteList{Bytes("go"), Bytes("test")}
	rec.View.EnvNames = ByteList{Bytes("PATH")}
	rec.Stdout = StreamRecord{Present: true, SourceBytes: 4, RedactedBytes: 3,
		SHA256: sha256Hex([]byte("abc")), Head: []byte("abc")}
	c := cloneRecord(&rec)
	rec.View.Argv[0][0] = 'X'
	rec.View.EnvNames[0][0] = 'X'
	rec.Stdout.Head[0] = 'X'
	if string(c.View.Argv[0]) != "go" || string(c.View.EnvNames[0]) != "PATH" || string(c.Stdout.Head) != "abc" {
		t.Fatalf("the record clone shares backing arrays with its source: %+v", c)
	}
}

// TestARecordRoundTripsThroughItsCanonicalBoundary.
//
// A record type with no encoder was not a canonical record at all - it was an in-memory struct claiming
// to be one, and every "the digest identifies these bytes" statement around it was unbacked.
func TestARecordRoundTripsThroughItsCanonicalBoundary(t *testing.T) {
	rec := validRecord()
	rec.SchemaVersion = ResultRecordVersion
	rec.Stdout = StreamRecord{Present: true, SourceBytes: 9, RedactedBytes: 3,
		SHA256: sha256Hex([]byte{0xff, 0xfe, 0x00}), Head: []byte{0xff, 0xfe, 0x00}}
	// An argv element that is not valid UTF-8, which is the case a JSON string field would silently
	// normalize - binding bytes the child never received.
	rec.View.Argv = ByteList{Bytes("go"), Bytes{0xff, 0xfe, 'x'}}

	raw, digest, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if digest != sha256Hex(raw) {
		t.Fatalf("the returned digest is not over the returned bytes")
	}
	back, err := DecodeResultRecord(raw)
	if err != nil {
		t.Fatalf("DecodeResultRecord: %v", err)
	}
	// Arbitrary bytes must survive, which is why the excerpt is base64 on the wire: a stream that is
	// not valid UTF-8 is exactly the case the design harvested vectors for.
	if !bytes.Equal(back.Stdout.Head, []byte{0xff, 0xfe, 0x00}) {
		t.Fatalf("non-UTF-8 stream bytes did not survive the round trip: %v", back.Stdout.Head)
	}
	if !bytes.Equal(back.View.Argv[1], []byte{0xff, 0xfe, 'x'}) {
		t.Fatalf("a non-UTF-8 argv element did not survive the round trip: %v", back.View.Argv[1])
	}
	// The claim is BYTE identity, not struct-shape identity. Comparing structs would fuss over nil
	// against empty, which the wire deliberately no longer distinguishes - and byte identity is the only
	// form of "unchanged" a digest-identified record actually needs.
	againRaw, _, err := back.Encode()
	if err != nil {
		t.Fatalf("re-encoding what was decoded: %v", err)
	}
	if !bytes.Equal(againRaw, raw) {
		t.Fatal("a decoded record does not re-encode to the bytes it came from")
	}
	_, digest2, err := back.Encode()
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if digest2 != digest {
		t.Fatal("encoding is not deterministic, so the digest does not identify the record")
	}
}

// TestTheRecordBoundaryRefusesDocumentsItDidNotWrite.
//
// Strict, because an unknown field, a duplicate key or a trailing object is a document written by
// something that disagrees with this schema, and guessing which parts to honour is how two readers end
// up with two different records.
func TestTheRecordBoundaryRefusesDocumentsItDidNotWrite(t *testing.T) {
	rec := validRecord()
	raw, _, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, tc := range []struct {
		name string
		doc  []byte
		want string
	}{
		{"not JSON at all", []byte("not a result"), "not JSON"},
		// A COPY. Appending onto a reslice of raw rewrites raw's own backing array, which silently
		// corrupted the trailing-content case below - the same aliasing class this package spent nine
		// seams closing, reappearing in the test that checks the boundary.
		{"an unknown field", append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"extra":1}`)...), "decoding"},
		{"trailing content", append(append([]byte(nil), raw...), []byte("{}")...), "trailing content"},
		{"a version this code does not speak", func() []byte {
			r := validRecord()
			r.SchemaVersion = ResultRecordVersion + 1
			b, _ := json.Marshal(r)
			return b
		}(), "schema version"},
		{"no version at all", func() []byte {
			r := validRecord()
			r.SchemaVersion = 0
			b, _ := json.Marshal(r)
			return b
		}(), "schema version"},
		{"a document that decodes but contradicts itself", func() []byte {
			r := validRecord()
			r.ExitCode = 17
			b, _ := json.Marshal(r)
			return b
		}(), "requires exit code 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeResultRecord(tc.doc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestAnUnpublishableRecordCannotAcquireADigest.
//
// Validation happens BEFORE encoding, so nothing that state would refuse can ever be named by a digest -
// which matters because the digest is what the ledger binds, and a digest for a record that cannot be
// stored is a reference to something that will never exist.
func TestAnUnpublishableRecordCannotAcquireADigest(t *testing.T) {
	rec := validRecord()
	rec.TerminalReason = ""
	if _, _, err := rec.Encode(); err == nil || !strings.Contains(err.Error(), "blank") {
		t.Fatalf("err = %v, want the encoder to refuse it", err)
	}
	rec = validRecord()
	rec.SchemaVersion = 0
	if _, _, err := rec.Encode(); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("err = %v, want a version refusal", err)
	}
}

// TestTheExcerptSplitIsDeterministic.
//
// "Head and tail within the ceiling" is not a specification until the split is named: two producers
// obeying the same bound could keep different bytes and both call themselves correct, and a verifier
// could not tell which. Storing the halves separately is also what makes marker collision impossible -
// process output is arbitrary bytes, so any in-band delimiter is a byte sequence real output can contain.
func TestTheExcerptSplitIsDeterministic(t *testing.T) {
	stream := []byte("0123456789")
	head, tail, truncated := SplitExcerpt(stream, 5)
	if !truncated || string(head) != "012" || string(tail) != "89" {
		t.Fatalf("split = %q / %q truncated=%t, want a deterministic head-heavy split", head, tail, truncated)
	}
	// Repeating it gives the same answer, which is the whole point.
	h2, t2, _ := SplitExcerpt(stream, 5)
	if string(h2) != string(head) || string(t2) != string(tail) {
		t.Fatal("the split is not deterministic")
	}
	// Within budget: the whole stream, no tail, not truncated.
	head, tail, truncated = SplitExcerpt(stream, 100)
	if truncated || tail != nil || string(head) != "0123456789" {
		t.Fatalf("an in-budget stream was split: %q / %q truncated=%t", head, tail, truncated)
	}
	// The excerpt never exceeds the budget, and the halves are owned copies.
	head, tail, _ = SplitExcerpt(stream, 4)
	if uint64(len(head)+len(tail)) != 4 {
		t.Fatalf("the split kept %d bytes of a 4-byte budget", len(head)+len(tail))
	}
	stream[0] = 'X'
	if head[0] != '0' {
		t.Fatal("the split shares the caller's backing array")
	}
	// A byte sequence identical to the display marker is ordinary output, and must survive.
	withMarker := append(append([]byte("a"), ElisionMarker...), 'b')
	h3, t3, tr := SplitExcerpt(withMarker, uint64(len(withMarker)))
	if tr || !bytes.Equal(h3, withMarker) || t3 != nil {
		t.Fatal("output containing the display marker was treated as already truncated")
	}
	rec := StreamRecord{Present: true, SourceBytes: uint64(len(withMarker)), RedactedBytes: uint64(len(withMarker)),
		SHA256: sha256Hex(withMarker), Head: h3}
	if err := rec.validate("stdout"); err != nil {
		t.Fatalf("output containing the display marker was refused: %v", err)
	}
}

// TestTheCombinedCeilingIsCombined.
//
// Per-stream checking admits a record twice the size the operator allowed.
func TestTheCombinedCeilingIsCombined(t *testing.T) {
	five := StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 5,
		SHA256: sha256Hex([]byte("12345")), Head: []byte("12345")}
	if FitsCombinedOutputCeiling(five, five, 9) {
		t.Fatal("two five-byte excerpts fit a nine-byte ceiling")
	}
	if !FitsCombinedOutputCeiling(five, five, 10) {
		t.Fatal("two five-byte excerpts did not fit a ten-byte ceiling")
	}
	if !FitsCombinedOutputCeiling(five, StreamRecord{}, 5) {
		t.Fatal("an absent second stream was counted")
	}
}

// TestTheDecoderRefusesNonCanonicalDocuments.
//
// DisallowUnknownFields alone was not the strict canonical boundary the comment claimed: Go matches keys
// case-insensitively, takes the last of a duplicated pair, ignores order and tolerates whitespace. Each
// of those decodes to the same record while hashing differently, so one record would have had several
// durable digests - and a digest that does not uniquely name a record is not an identity.
func TestTheDecoderRefusesNonCanonicalDocuments(t *testing.T) {
	rec := validRecord()
	raw, _, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, tc := range []struct {
		name string
		doc  []byte
	}{
		{"a duplicated key", bytes.Replace(append([]byte(nil), raw...), []byte(`{"attempt_id"`),
			[]byte(`{"attempt_id":"somebody-else","attempt_id"`), 1)},
		{"whitespace", append(append([]byte(" "), raw...), ' ')},
		{"a case-aliased key", bytes.Replace(append([]byte(nil), raw...), []byte(`"attempt_id"`), []byte(`"Attempt_Id"`), 1)},
		{"reordered keys", func() []byte {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Re-emit with a key deliberately hoisted to the front.
			out := []byte(`{"terminal_reason":`)
			out = append(out, m["terminal_reason"]...)
			for k, v := range m {
				if k == "terminal_reason" {
					continue
				}
				out = append(out, ',')
				out = append(out, []byte(`"`+k+`":`)...)
				out = append(out, v...)
			}
			return append(out, '}')
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeResultRecord(tc.doc); err == nil {
				t.Fatal("a non-canonical document was accepted, so one record has several durable digests")
			}
		})
	}
}

// TestTheSplitRuleIsEnforcedNotJustDefined.
//
// SplitExcerpt on its own was a helper nothing consulted: for the same retained length, an all-head
// excerpt and the documented split both validated, so "head+tail" was a shape rather than a rule and two
// producers could keep different bytes while each called itself correct. And a single combined budget
// does not say how much of it each stream gets, so 100/0 and 50/50 both fit the ceiling.
func TestTheSplitRuleIsEnforcedNotJustDefined(t *testing.T) {
	head, tail, _ := SplitExcerpt([]byte("0123456789"), 8)
	good := StreamRecord{Present: true, SourceBytes: 10, RedactedBytes: 10,
		SHA256: strings.Repeat("3c", 32), Head: head, Tail: tail, Truncated: true}
	if err := good.CheckSplit("stdout", 8); err != nil {
		t.Fatalf("the documented split was refused: %v", err)
	}
	skewed := good
	skewed.Head, skewed.Tail = Bytes("01234567"), nil
	if err := skewed.CheckSplit("stdout", 8); err == nil {
		t.Fatal("an all-head excerpt of the right length passed the split rule")
	}
	// An untruncated stream has nothing to allocate, so the rule does not apply to it.
	whole := StreamRecord{Present: true, RedactedBytes: 3, SHA256: sha256Hex([]byte("abc")), Head: Bytes("abc")}
	if err := whole.CheckSplit("stdout", 8); err != nil {
		t.Fatalf("an untruncated stream was held to the split rule: %v", err)
	}

	// The combined budget is DIVIDED, so both allocations are named rather than left to a producer.
	out, errB := AllocateOutputBudget(9)
	if out != 5 || errB != 4 {
		t.Fatalf("nine bytes divided %d/%d", out, errB)
	}
}

// TestEncodingSettlesNilAgainstEmptyBeforeHashing.
//
// A record built with a nil half and the same record read back from disk describe the same thing, and
// they must hash the same or one fact has two durable identities.
//
// It is the WIRE TYPES that settle this, not a normalization pass: an earlier version normalized the
// struct before encoding, and the mutation sweep showed removing that step changed nothing, because the
// marshaller already emits the empty form. Unprovable code removed rather than kept for comfort.
func TestEncodingSettlesNilAgainstEmptyBeforeHashing(t *testing.T) {
	withNil := validRecord()
	withNil.Stdout = StreamRecord{Present: true, SourceBytes: 3, RedactedBytes: 3,
		SHA256: sha256Hex([]byte("abc")), Head: Bytes("abc"), Tail: nil}

	withEmpty := validRecord()
	withEmpty.Stdout = StreamRecord{Present: true, SourceBytes: 3, RedactedBytes: 3,
		SHA256: sha256Hex([]byte("abc")), Head: Bytes("abc"), Tail: Bytes{}}

	rawA, digestA, err := withNil.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	rawB, digestB, err := withEmpty.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if digestA != digestB || !bytes.Equal(rawA, rawB) {
		t.Fatalf("nil and empty produced two identities for one record:\n %s\n %s", rawA, rawB)
	}
	if bytes.Contains(rawA, []byte("null")) {
		t.Fatalf("an empty value was written as null: %s", rawA)
	}
}

// TestTheAllocationDoesNotDependOnWhatSurvivedTheCrash.
//
// Both capture streams exist from the moment the attempt starts, whatever is readable afterwards. An
// earlier version gave a lone present stream the whole ceiling, which made the division a function of
// which staging file happened to survive: a runner capturing both pipes keeps half, and a recovery that
// finds only the stdout staging recomputes the whole ceiling and rejects the excerpt the runner
// legitimately wrote. Availability after a crash cannot redefine history.
func TestTheAllocationDoesNotDependOnWhatSurvivedTheCrash(t *testing.T) {
	const ceiling = 100
	outBudget, _ := AllocateOutputBudget(ceiling)

	stream := bytes.Repeat([]byte{'o'}, 500)
	head, tail, truncated := SplitExcerpt(stream, outBudget)
	if !truncated {
		t.Fatal("the fixture was not truncated")
	}
	// Written while BOTH pipes were being captured.
	rec := StreamRecord{Present: true, SourceBytes: 500, RedactedBytes: 500,
		SHA256: strings.Repeat("3c", 32), Head: head, Tail: tail, Truncated: true}

	// Read back when only stdout staging survived. The allocation must be the same one.
	againOut, _ := AllocateOutputBudget(ceiling)
	if againOut != outBudget {
		t.Fatalf("the allocation changed from %d to %d because stderr staging was missing", outBudget, againOut)
	}
	if err := rec.CheckSplit("stdout", againOut); err != nil {
		t.Fatalf("a legitimately written excerpt was rejected after a crash: %v", err)
	}
}

// TestTheExcerptBudgetComesFromTheRecordsACTUALFixedSize.
//
// The two policy bounds measure different things and neither implies the other: raw retained bytes
// against canonical bytes, where the excerpt is base64 and the argv, environment names and terminal
// account are variable-length. The design says outright that this metadata can make even a ZERO-OUTPUT
// record nearly exhaust the record ceiling, so an excerpt sized against the raw bound alone can produce
// a record nobody can store.
func TestTheExcerptBudgetComesFromTheRecordsACTUALFixedSize(t *testing.T) {
	// EVERY field final except the excerpt bytes, which is the contract: the counts and the digest are
	// per-stream metadata that only exists once a stream is present, so measuring a record with absent
	// streams would understate the fixed size by exactly the amount that matters.
	base := func(bothStreams bool) ResultRecord {
		rec := validRecord()
		rec.Stdout = StreamRecord{Present: true, SourceBytes: 99999, RedactedBytes: 99999,
			SHA256: strings.Repeat("3c", 32), Head: Bytes{}, Tail: Bytes{}, Truncated: true}
		if bothStreams {
			rec.Stderr = rec.Stdout
		}
		return rec
	}

	// The assertion is a PROPERTY, not a number: the derived budget fits and one byte more does not.
	// An earlier version asserted the arithmetic's answer, which is how a formula that was wrong for
	// padded, split fields went unnoticed.
	for _, bothStreams := range []bool{false, true} {
		rec := base(bothStreams)
		bare, _, err := rec.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, spare := range []uint64{400, 401, 402, 403, 404, 1000} {
			ceiling := uint64(len(bare)) + spare
			budget, err := ExcerptBudget(rec, ceiling)
			if err != nil {
				t.Fatalf("both=%t spare=%d: %v", bothStreams, spare, err)
			}
			atBudget, _, err := sizedLike(rec, budget).Encode()
			if err != nil {
				t.Fatalf("encoding at the budget: %v", err)
			}
			if uint64(len(atBudget)) > ceiling {
				t.Fatalf("both=%t spare=%d: a %d-byte excerpt encodes to %d, over the %d ceiling",
					bothStreams, spare, budget, len(atBudget), ceiling)
			}
			over, _, err := sizedLike(rec, budget+1).Encode()
			if err != nil {
				t.Fatalf("encoding one over: %v", err)
			}
			if uint64(len(over)) <= ceiling {
				t.Fatalf("both=%t spare=%d: budget %d is not maximal; %d also fits",
					bothStreams, spare, budget, budget+1)
			}
		}
	}

	// A ceiling the metadata alone already exhausts is refused rather than answered with a nonsense
	// budget - the zero-output case the design calls out by name.
	if _, err := ExcerptBudget(base(false), 10); err == nil {
		t.Fatal("a ceiling smaller than the metadata produced a budget")
	}
}

// TestCountsBeyondTheEncodersRangeAreRefused.
//
// Canonical JSON restricts integers to the range every reader holds exactly, which is smaller than the
// uint64 the field type admits. Without this the record is accepted here and fails much later with a
// message about integer ranges - which is exactly how the first two worst-case records built in this
// package failed.
func TestCountsBeyondTheEncodersRangeAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		bend func(*StreamRecord)
	}{
		{"source bytes", func(s *StreamRecord) { s.SourceBytes = MaxRepresentableCount + 1 }},
		{"redacted bytes", func(s *StreamRecord) { s.RedactedBytes = MaxRepresentableCount + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := StreamRecord{Present: true, SourceBytes: 3, RedactedBytes: 3,
				SHA256: sha256Hex([]byte("abc")), Head: Bytes("abc")}
			tc.bend(&s)
			if err := s.validate("stdout"); err == nil || !strings.Contains(err.Error(), "cannot represent") {
				t.Fatalf("err = %v, want a representability refusal", err)
			}
		})
	}
}
