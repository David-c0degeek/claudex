package protocol

import "testing"

const hex64 = "0123456789012345678901234567890123456789012345678901234567890123"

// The embedded artifact schemas must accept a well-formed instance, not merely
// compile — a positive check that the strict profile did not over-constrain the
// harvested contracts.
func TestArtifactSchemasAcceptValidInstances(t *testing.T) {
	cases := map[string]string{
		"plan": `{
			"protocol_version":1,"message_type":"plan","turn_id":"t","state_revision":2,
			"human_context":null,"requires_human_decision":false,"decision_question":null,
			"plan_markdown":"# Plan","steps":[{"title":"s1","description":"do it","files":["a.go"],"tests":["a_test.go"]}],
			"risks":["r1"],"open_questions":[]
		}`,
		"plan_critique": `{
			"protocol_version":1,"message_type":"plan_critique","turn_id":"t","state_revision":2,
			"human_context":null,"requires_human_decision":false,"decision_question":null,
			"verdict":"AGREE",
			"findings":[{"key":"k1","kind":"new","category":"scope","severity":"major","file":null,"line":null,"problem":"p","evidence":"e","suggested_fix":"f"}],
			"implementation_checks":[{"key":"c1","description":"d","evidence":"e","action":"add","target_step":null}],
			"missing_evidence":[],"simpler_alternative":null,"notes":"n"
		}`,
		"plan_revision": `{
			"protocol_version":1,"message_type":"plan_revision","turn_id":"t","state_revision":3,
			"human_context":null,"requires_human_decision":false,"decision_question":null,
			"base_plan_sha256":"` + hex64 + `","plan_markdown":null,"steps":null,"risks":null,"open_questions":null,
			"responses":[{"finding_key":"k1","finding":"f","action":"accepted","rationale":"r"}]
		}`,
		"checkpoint_review": `{
			"protocol_version":1,"message_type":"checkpoint_review","turn_id":"t","state_revision":3,
			"human_context":"the human asked to keep the API stable","requires_human_decision":false,"decision_question":null,
			"verdict":"AGREE","findings":[{"severity":"minor","file":null,"line":null,"problem":"p","evidence":"e","suggested_fix":"f"}],
			"missing_evidence":[],"tests_adequate":true,"tests_critique":"ok"
		}`,
		"verification": `{
			"protocol_version":1,"message_type":"verification","turn_id":"t","state_revision":4,
			"human_context":null,"requires_human_decision":true,"decision_question":"which base?",
			"criteria":[{"criterion":"builds","met":true,"evidence":"go build ok"}],
			"scope_expansion":[],"tests_meaningful":true,"unsupported_claims":[],"verdict":"pass","notes":"n"
		}`,
		"implementation_report": `{
			"protocol_version":1,"message_type":"implementation_report","turn_id":"t","state_revision":5,
			"human_context":null,"requires_human_decision":false,"decision_question":null,
			"files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"
		}`,
	}
	for typ, body := range cases {
		t.Run(typ, func(t *testing.T) {
			if _, err := Validate(typ, []byte(body)); err != nil {
				t.Fatalf("valid %s instance rejected: %v", typ, err)
			}
		})
	}
}

// The implementation report must NOT accept agent-authored commit SHAs (the
// coordinator owns commit creation).
func TestImplementationReportRejectsCommits(t *testing.T) {
	body := `{"protocol_version":1,"message_type":"implementation_report","turn_id":"t","state_revision":5,
		"human_context":null,"requires_human_decision":false,"decision_question":null,
		"files_changed":["a.go"],"deviations_from_plan":[],"notes":"n","commits":[{"sha":"abc","message":"m"}]}`
	if _, err := Validate("implementation_report", []byte(body)); err == nil {
		t.Fatalf("implementation_report must reject an agent-authored commits field")
	}
}

// The checkpoint review must require missing_evidence (inconclusive-review path).
func TestCheckpointReviewRequiresMissingEvidence(t *testing.T) {
	body := `{"protocol_version":1,"message_type":"checkpoint_review","turn_id":"t","state_revision":3,
		"human_context":null,"requires_human_decision":false,"decision_question":null,
		"verdict":"AGREE","findings":[],"tests_adequate":true,"tests_critique":"ok"}`
	if _, err := Validate("checkpoint_review", []byte(body)); err == nil {
		t.Fatalf("checkpoint_review must require missing_evidence")
	}
}

// A wrong-type submit is caught by the same validator.
func TestArtifactSchemaRejectsWrongShape(t *testing.T) {
	body := `{"protocol_version":1,"message_type":"plan","turn_id":"t","state_revision":2,
		"human_context":null,"requires_human_decision":false,"decision_question":null,
		"plan_markdown":"x","steps":[{"title":1,"description":"d","files":[],"tests":[]}],"risks":[],"open_questions":[]}`
	if _, err := Validate("plan", []byte(body)); err == nil {
		t.Fatalf("a non-string step title should be rejected")
	}
}
