package testgate

import (
	"strings"
	"testing"
)

// TestTheExecutionWireHasExactlyOneSpellingPerFact.
//
// Go writes a []byte as base64, which is the grammar the design pins, but it writes a NIL one as `null` -
// and `null` is not base64. That gave every empty value two admissible spellings with the same meaning
// and different digests, which is the one thing a digest-identified record cannot have.
func TestTheExecutionWireHasExactlyOneSpellingPerFact(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"a null executable", `{"executable_b64":null,"argv_b64":["Z28="],"cwd_b64":"L3c=","env_names_b64":[],"env_digest":"` + strings.Repeat("5e", 32) + `"}`},
		{"a null argv list", `{"executable_b64":"L2dv","argv_b64":null,"cwd_b64":"L3c=","env_names_b64":[],"env_digest":"` + strings.Repeat("5e", 32) + `"}`},
		{"a null argv element", `{"executable_b64":"L2dv","argv_b64":[null],"cwd_b64":"L3c=","env_names_b64":[],"env_digest":"` + strings.Repeat("5e", 32) + `"}`},
		{"a null environment list", `{"executable_b64":"L2dv","argv_b64":["Z28="],"cwd_b64":"L3c=","env_names_b64":null,"env_digest":"` + strings.Repeat("5e", 32) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v ExecutionView
			if err := v.UnmarshalJSONForTest([]byte(tc.doc)); err == nil {
				t.Fatal("null was accepted, so one empty value has two admissible spellings")
			}
		})
	}

	// And the empty forms round-trip as themselves rather than becoming null.
	v := ExecutionView{Executable: Bytes("/go"), Argv: ByteList{Bytes("")}, Cwd: Bytes("/w"),
		EnvNames: nil, EnvDigest: strings.Repeat("5e", 32)}
	raw, err := v.MarshalJSONForTest()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("an empty value was written as null: %s", raw)
	}
}
