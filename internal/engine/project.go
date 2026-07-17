package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// Project derives a typed Event from a schema-valid canonical artifact for the
// current phase and the materialized facts the loader supplied. It is PURE: it
// rechecks the facts' digests/keys against cur and returns a semantic error for any
// state-inconsistency, but performs no I/O. A semantic error rejects the submit; it
// never becomes a human gate.
func Project(cur state.RunState, canonical []byte, facts ProjectionFacts) (Event, error) {
	turnID, rhd, err := envelope(canonical)
	if err != nil {
		return Event{}, err
	}
	src := state.EventRef{Digest: hashHex(canonical), TurnID: turnID}
	switch cur.Phase {
	case state.PhasePlanDraft:
		return projectPlan(canonical, src, rhd)
	case state.PhasePlanCritique:
		return projectCritique(cur, canonical, src, rhd, facts)
	case state.PhasePlanRevise:
		return projectRevision(cur, canonical, src, rhd, facts)
	case state.PhaseImplementStep:
		return Event{Kind: EvStepImplemented, Source: src, Decision: rhd}, nil
	case state.PhaseCheckpoint:
		return projectCheckpoint(canonical, src, rhd)
	case state.PhaseFix:
		return Event{Kind: EvFixImplemented, Source: src, Decision: rhd}, nil
	}
	return Event{}, semanticf("no submit is projected for phase %s", cur.Phase)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func envelope(canonical []byte) (turnID string, requiresDecision bool, err error) {
	var env struct {
		TurnID                string `json:"turn_id"`
		RequiresHumanDecision bool   `json:"requires_human_decision"`
	}
	if err := json.Unmarshal(canonical, &env); err != nil {
		return "", false, semanticf("undecodable artifact envelope")
	}
	return env.TurnID, env.RequiresHumanDecision, nil
}

type jsonStep struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
	Tests       []string `json:"tests"`
}

func toPlanSteps(js []jsonStep) []PlanStep {
	out := make([]PlanStep, 0, len(js))
	for _, s := range js {
		out = append(out, PlanStep{Title: s.Title, Description: s.Description, Files: s.Files, Tests: s.Tests})
	}
	return out
}

// materializePlan decodes a plan artifact into its CanonicalPlan and materialized
// document digest. It is the single plan decoder the live draft projector and the
// history materializer share (no second parser to drift).
func materializePlan(canonical []byte) (CanonicalPlan, string, error) {
	var a struct {
		Markdown      string     `json:"plan_markdown"`
		Steps         []jsonStep `json:"steps"`
		Risks         []string   `json:"risks"`
		OpenQuestions []string   `json:"open_questions"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return CanonicalPlan{}, "", semanticf("undecodable plan artifact")
	}
	steps := toPlanSteps(a.Steps)
	if err := protocol.ValidateStepTitles(stepTitles(steps)); err != nil {
		return CanonicalPlan{}, "", semanticf("plan step titles: %v", err)
	}
	p := CanonicalPlan{Markdown: a.Markdown, Steps: steps, Risks: a.Risks, OpenQuestions: a.OpenQuestions}
	d, err := planDocDigest(p.Markdown, p.Steps, p.Risks, p.OpenQuestions)
	return p, d, err
}

func projectPlan(canonical []byte, src state.EventRef, rhd bool) (Event, error) {
	p, digest, err := materializePlan(canonical)
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: EvPlanDrafted, Source: src, Materialized: digest, StepCount: len(p.Steps), Decision: rhd}, nil
}

type jsonCheckOp struct {
	Key         string  `json:"key"`
	Description string  `json:"description"`
	Evidence    string  `json:"evidence"`
	Action      string  `json:"action"`
	TargetStep  *string `json:"target_step"`
}

// critiqueProjection is the full materialized result of projecting a critique
// artifact against the current check set and plan steps: the resulting materialized
// checks (both the concrete list and the CheckSetRef), the sorted actionable finding
// keys, and the verdict/missing/target-validity classification inputs. Both the live
// critique projector and the history materializer derive their Event/classification
// from this one helper — no second parser and no silent reparse of the findings.
type critiqueProjection struct {
	Verdict        string
	ActionableKeys []string // sorted blocking/major finding keys
	Missing        bool
	Resulting      []MaterializedCheck // ops applied to the current set
	Checks         state.CheckSetRef   // materialized resulting set
	TargetsValid   bool
}

func projectCritiqueParts(canonical []byte, currentChecks []MaterializedCheck, planSteps []PlanStep) (critiqueProjection, error) {
	var a struct {
		Verdict  string `json:"verdict"`
		Findings []struct {
			Key      string `json:"key"`
			Severity string `json:"severity"`
		} `json:"findings"`
		ImplementationChecks []jsonCheckOp `json:"implementation_checks"`
		MissingEvidence      []string      `json:"missing_evidence"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return critiqueProjection{}, semanticf("undecodable critique artifact")
	}
	// All finding keys and all check-op keys must be canonical and unique.
	findingKeys := make([]string, 0, len(a.Findings))
	for _, f := range a.Findings {
		findingKeys = append(findingKeys, f.Key)
	}
	if err := protocol.ValidateKeySet("findings", findingKeys); err != nil {
		return critiqueProjection{}, semanticf("%v", err)
	}
	opKeys := make([]string, 0, len(a.ImplementationChecks))
	for _, op := range a.ImplementationChecks {
		opKeys = append(opKeys, op.Key)
	}
	if err := protocol.ValidateKeySet("implementation_checks", opKeys); err != nil {
		return critiqueProjection{}, semanticf("%v", err)
	}
	var actionableKeys []string
	for _, f := range a.Findings {
		if f.Severity == "blocking" || f.Severity == "major" {
			actionableKeys = append(actionableKeys, f.Key)
		}
	}
	sort.Strings(actionableKeys)
	resulting, err := applyCheckOps(currentChecks, a.ImplementationChecks)
	if err != nil {
		return critiqueProjection{}, err
	}
	checks, err := materializeChecks(resulting)
	if err != nil {
		return critiqueProjection{}, err
	}
	return critiqueProjection{
		Verdict:        a.Verdict,
		ActionableKeys: actionableKeys,
		Missing:        len(a.MissingEvidence) > 0,
		Resulting:      resulting,
		Checks:         checks,
		TargetsValid:   targetsValidFor(resulting, stepTitles(planSteps)),
	}, nil
}

func projectCritique(cur state.RunState, canonical []byte, src state.EventRef, rhd bool, facts ProjectionFacts) (Event, error) {
	if err := verifyCandidateFacts(cur, facts); err != nil {
		return Event{}, err
	}
	p, err := projectCritiqueParts(canonical, facts.CandidateChecks, facts.CandidatePlan.Steps)
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:            EvPlanCritiqued,
		Source:          src,
		Verdict:         p.Verdict,
		Actionable:      len(p.ActionableKeys) > 0,
		MissingEvidence: p.Missing,
		Decision:        rhd,
		ResultingChecks: p.Checks,
		TargetsValid:    p.TargetsValid,
		Findings:        p.ActionableKeys,
	}, nil
}

func projectCheckpoint(canonical []byte, src state.EventRef, rhd bool) (Event, error) {
	var a struct {
		Verdict  string `json:"verdict"`
		Findings []struct {
			Severity string `json:"severity"`
		} `json:"findings"`
		MissingEvidence []string `json:"missing_evidence"`
		TestsAdequate   bool     `json:"tests_adequate"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return Event{}, semanticf("undecodable checkpoint artifact")
	}
	blockingMajor := false
	for _, f := range a.Findings {
		if f.Severity == "blocking" || f.Severity == "major" {
			blockingMajor = true
		}
	}
	return Event{
		Kind:            EvStepCheckpointed,
		Source:          src,
		Verdict:         a.Verdict,
		Actionable:      blockingMajor || !a.TestsAdequate,
		MissingEvidence: len(a.MissingEvidence) > 0,
		TestsAdequate:   a.TestsAdequate,
		Decision:        rhd,
	}, nil
}

func projectRevision(cur state.RunState, canonical []byte, src state.EventRef, rhd bool, facts ProjectionFacts) (Event, error) {
	if err := verifyCandidateFacts(cur, facts); err != nil {
		return Event{}, err
	}
	if cur.PendingFindings == nil {
		return Event{}, semanticf("a revision arrived with no outstanding findings")
	}
	res, err := applyRevision(canonical, facts.CandidatePlan, cur.CandidatePlan.Digest, cur.PendingFindings.Keys)
	if err != nil {
		return Event{}, err
	}
	digest, err := planDocDigest(res.Markdown, res.Steps, res.Risks, res.OpenQuestions)
	if err != nil {
		return Event{}, err
	}
	return Event{Kind: EvPlanRevised, Source: src, Materialized: digest, StepCount: len(res.Steps), Decision: rhd}, nil
}

// applyRevision validates a plan_revision against the base plan digest and the exact
// outstanding finding keys, then materializes the resulting plan (a null section
// preserves the base). It is the single revision-materialization the live projector
// and the history materializer share.
func applyRevision(canonical []byte, base CanonicalPlan, baseDigest string, pending []string) (CanonicalPlan, error) {
	var a struct {
		BasePlanSHA256 string     `json:"base_plan_sha256"`
		Markdown       *string    `json:"plan_markdown"`
		Steps          []jsonStep `json:"steps"`
		Risks          []string   `json:"risks"`
		OpenQuestions  []string   `json:"open_questions"`
		Responses      []struct {
			FindingKey string `json:"finding_key"`
		} `json:"responses"`
	}
	// A null steps/risks/open_questions preserves the base; distinguish null from an
	// empty replacement with a presence probe.
	var probe struct {
		Steps         *json.RawMessage `json:"steps"`
		Risks         *json.RawMessage `json:"risks"`
		OpenQuestions *json.RawMessage `json:"open_questions"`
		Markdown      *json.RawMessage `json:"plan_markdown"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return CanonicalPlan{}, semanticf("undecodable revision artifact")
	}
	if err := json.Unmarshal(canonical, &probe); err != nil {
		return CanonicalPlan{}, semanticf("undecodable revision artifact")
	}
	if a.BasePlanSHA256 != baseDigest {
		return CanonicalPlan{}, semanticf("revision base does not match the current candidate plan")
	}
	// Responses must answer exactly the outstanding obligations — no missing, no extra.
	respKeys := make([]string, 0, len(a.Responses))
	for _, r := range a.Responses {
		respKeys = append(respKeys, r.FindingKey)
	}
	if err := protocol.ValidateKeySet("responses", respKeys); err != nil {
		return CanonicalPlan{}, semanticf("%v", err)
	}
	sort.Strings(respKeys)
	want := append([]string(nil), pending...)
	sort.Strings(want)
	if !reflect.DeepEqual(respKeys, want) {
		return CanonicalPlan{}, semanticf("revision responses are not an exact match for the outstanding findings")
	}

	res := CanonicalPlan{Markdown: base.Markdown, Steps: base.Steps, Risks: base.Risks, OpenQuestions: base.OpenQuestions}
	if isPresent(probe.Markdown) && a.Markdown != nil {
		res.Markdown = *a.Markdown
	}
	if isPresent(probe.Steps) {
		res.Steps = toPlanSteps(a.Steps)
	}
	if isPresent(probe.Risks) {
		res.Risks = a.Risks
	}
	if isPresent(probe.OpenQuestions) {
		res.OpenQuestions = a.OpenQuestions
	}
	if err := protocol.ValidateStepTitles(stepTitles(res.Steps)); err != nil {
		return CanonicalPlan{}, semanticf("revised plan step titles are invalid: %v", err)
	}
	if res.Markdown == "" {
		return CanonicalPlan{}, semanticf("the materialized plan markdown is empty")
	}
	return res, nil
}

// isPresent reports whether a nullable section was supplied as a non-null value.
func isPresent(raw *json.RawMessage) bool {
	return raw != nil && string(*raw) != "null"
}

// verifyCandidateFacts rechecks that the loader-supplied materialized plan and
// checks digest/key match the current durable candidate.
func verifyCandidateFacts(cur state.RunState, facts ProjectionFacts) error {
	if cur.CandidatePlan == nil || cur.CandidateChecks == nil {
		return semanticf("no candidate plan in state for this phase")
	}
	if err := protocol.ValidateStepTitles(stepTitles(facts.CandidatePlan.Steps)); err != nil {
		return semanticf("candidate step titles: %v", err)
	}
	if len(facts.CandidatePlan.Steps) != cur.CandidatePlan.StepCount {
		return semanticf("the supplied candidate step count does not match the durable count")
	}
	digest, err := planDocDigest(facts.CandidatePlan.Markdown, facts.CandidatePlan.Steps, facts.CandidatePlan.Risks, facts.CandidatePlan.OpenQuestions)
	if err != nil {
		return err
	}
	if digest != cur.CandidatePlan.Digest {
		return semanticf("the supplied candidate plan does not match the durable digest")
	}
	if facts.CandidateSource != cur.CandidatePlan.Source {
		return semanticf("the supplied candidate source does not match the durable source")
	}
	checks, err := materializeChecks(facts.CandidateChecks)
	if err != nil {
		return err
	}
	if checks.Digest != cur.CandidateChecks.Digest || !reflect.DeepEqual(checks.Keys, cur.CandidateChecks.Keys) {
		return semanticf("the supplied candidate checks do not match the durable set")
	}
	return nil
}

// applyCheckOps applies a critique's add/remove ops to the current materialized set
// (add is an upsert; remove of a missing key is a semantic error).
func applyCheckOps(current []MaterializedCheck, ops []jsonCheckOp) ([]MaterializedCheck, error) {
	m := make(map[string]MaterializedCheck, len(current))
	order := make([]string, 0, len(current))
	for _, c := range current {
		if _, ok := m[c.Key]; !ok {
			order = append(order, c.Key)
		}
		m[c.Key] = c
	}
	for _, op := range ops {
		switch op.Action {
		case "add":
			if _, ok := m[op.Key]; !ok {
				order = append(order, op.Key)
			}
			m[op.Key] = MaterializedCheck{Key: op.Key, Description: op.Description, Evidence: op.Evidence, TargetStep: op.TargetStep}
		case "remove":
			if _, ok := m[op.Key]; !ok {
				return nil, semanticf("remove of a check key that is not present")
			}
			delete(m, op.Key)
		default:
			return nil, semanticf("unknown check action %q", op.Action)
		}
	}
	out := make([]MaterializedCheck, 0, len(m))
	for _, k := range order {
		if c, ok := m[k]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// targetsValidFor reports whether every non-null target names a current step title.
func targetsValidFor(checks []MaterializedCheck, titles []string) bool {
	set := make(map[string]bool, len(titles))
	for _, t := range titles {
		set[t] = true
	}
	for _, c := range checks {
		if c.TargetStep != nil && !set[*c.TargetStep] {
			return false
		}
	}
	return true
}
