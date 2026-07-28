package testgate

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// truncatedExcerpt builds the head+tail representation with its explicit marker.
func truncatedExcerpt(head, tail string) []byte {
	out := append([]byte(head), ElisionMarker...)
	return append(out, tail...)
}

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
		SHA256: strings.Repeat("3c", 32), Retained: truncatedExcerpt("head", "tail"), Truncated: true,
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
		// The one fact in this record a verifier can RECOMPUTE, so it must be recomputed.
		{"an untruncated excerpt disagreeing with its own digest", StreamRecord{
			Present: true, SourceBytes: 3, RedactedBytes: 3,
			SHA256: strings.Repeat("3c", 32), Retained: []byte("abc"),
		}, "hashes to"},
		{"a truncated excerpt with no elision marker", StreamRecord{
			Present: true, SourceBytes: 99, RedactedBytes: 99,
			SHA256: strings.Repeat("3c", 32), Retained: []byte("head-tail"), Truncated: true,
		}, "no elision marker"},
		{"a truncated excerpt with two markers", StreamRecord{
			Present: true, SourceBytes: 99, RedactedBytes: 99, SHA256: strings.Repeat("3c", 32),
			Retained:  append(truncatedExcerpt("a", "b"), ElisionMarker...),
			Truncated: true,
		}, "more than one elision marker"},
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
	// An untruncated excerpt IS the whole redacted stream, so its digest is recomputable and must match -
	// a fixture with a made-up digest would have been asserting that the record may disagree with itself.
	whole := StreamRecord{Present: true, SourceBytes: 5, RedactedBytes: 3,
		SHA256: sha256Hex([]byte("abc")), Retained: []byte("abc")}
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
		SHA256: sha256Hex(original), Retained: original,
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
		SHA256: sha256Hex([]byte("abc")), Retained: []byte("abc")}
	c := cloneRecord(&rec)
	rec.ResolvedArgv[0] = "MUTATED"
	rec.EnvNames[0] = "MUTATED"
	rec.Stdout.Retained[0] = 'X'
	if c.ResolvedArgv[0] != "go" || c.EnvNames[0] != "PATH" || string(c.Stdout.Retained) != "abc" {
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
		SHA256: sha256Hex([]byte{0xff, 0xfe, 0x00}), Retained: []byte{0xff, 0xfe, 0x00}}

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
	if !bytes.Equal(back.Stdout.Retained, []byte{0xff, 0xfe, 0x00}) {
		t.Fatalf("non-UTF-8 stream bytes did not survive the round trip: %v", back.Stdout.Retained)
	}
	if !reflect.DeepEqual(back, rec) {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", back, rec)
	}
	again, digest2, err := back.Encode()
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(again, raw) || digest2 != digest {
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
	rec.SchemaVersion = ResultRecordVersion
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
			b, _ := json.Marshal(r)
			return b
		}(), "schema version"},
		{"a document that decodes but contradicts itself", func() []byte {
			r := validRecord()
			r.SchemaVersion = ResultRecordVersion
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
	rec.SchemaVersion = ResultRecordVersion
	rec.TerminalReason = ""
	if _, _, err := rec.Encode(); err == nil || !strings.Contains(err.Error(), "blank") {
		t.Fatalf("err = %v, want the encoder to refuse it", err)
	}
	rec = validRecord() // version left at zero
	if _, _, err := rec.Encode(); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("err = %v, want a version refusal", err)
	}
}
