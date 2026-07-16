package protocol

import "testing"

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
		"checkpoint_review": `{
			"protocol_version":1,"message_type":"checkpoint_review","turn_id":"t","state_revision":3,
			"human_context":"looks good","requires_human_decision":false,"decision_question":null,
			"verdict":"AGREE","findings":[{"severity":"minor","file":null,"line":null,"problem":"p","evidence":"e","suggested_fix":"f"}],
			"tests_adequate":true,"tests_critique":"ok"
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
			"commits":[{"sha":"abc123","message":"do it"}],"files_changed":["a.go"],
			"tests_command":"go test ./...","tests_passed":true,"test_output_summary":"ok","deviations_from_plan":[],"notes":"n"
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

// A wrong-type submit is caught by the same validator.
func TestArtifactSchemaRejectsWrongShape(t *testing.T) {
	// plan with a non-string step title.
	body := `{"protocol_version":1,"message_type":"plan","turn_id":"t","state_revision":2,
		"human_context":null,"requires_human_decision":false,"decision_question":null,
		"plan_markdown":"x","steps":[{"title":1,"description":"d","files":[],"tests":[]}],"risks":[],"open_questions":[]}`
	if _, err := Validate("plan", []byte(body)); err == nil {
		t.Fatalf("a non-string step title should be rejected")
	}
}
