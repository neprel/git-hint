package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

// D03: the local report shows at most five findings in the server's order, with local names,
// unknown values as unknown and the heuristic label; a v0.1 set shows no findings section.
func TestRenderFindings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", "recommendation-findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	payload := doc["payload"].(map[string]any)
	fs := payload["findings"].([]any)
	target := fs[0].(map[string]any)["targets"].([]any)[0].(string)
	cur := &Current{Names: map[string]string{target: "billing/ledger.go"}}
	im := &Imported{Result: hint.ImportResult{Document: "accepted"}, Doc: doc}
	md := Render(im, cur, "x.json")
	for _, want := range []string{"## Summary", "- Problems found: 2 (1 with enough evidence, 1 limited). Not diagnosed: coordination_wait.",
		"- First problem: repeated_navigation (supported). Proposed action: COLLECT_MORE_EVIDENCE. Next step: refresh_index_and_recheck.",
		"- Evaluation: 1 eval comparisons of your project", "Kinds of values: **observed**", "## Findings", "heuristic priority, not a calibrated ranking", "### Finding 1: repeated_navigation (supported)",
		"- stale_index: **supported** (stale_context_measurement, repeat_without_observed_edit)", "| large_read_bytes | bytes | unknown | 2 of 5 |",
		"Next step: **refresh_index_and_recheck** on billing/ledger.go", "- coordination_wait: insufficient_coverage", "### Finding 2: large_low_selectivity_read (limited)",
		"Intervention comparison (heuristic; cost model unavailable, so no net value or break-even): **COLLECT_MORE_EVIDENCE**.",
		"| CONTEXT_ONLY | unbound | unknown | unknown | none |", "No eval comparison is bound to this finding's objectives",
		"## Project eval comparisons", "| context_only | held_out | 6 | -120.5 (-400 to 300; 6 of 6) [root-session raw counters (Harbor 0.23.0 + OpenCode), not task effort] | 0.1 (0 to 0.2; 6 of 6) |",
		"Forecast basis (a model-conditional forecast, not a measurement): objective model_tokens in tokens over 120 future tasks.",
		"- P(net value over the horizon > 0) under the model: 0.93; decision threshold 0.9 (heuristic default).",
		"- Net value over 120 tasks: 50000 to 200000 tokens (0.9 interval).",
		"- One-time cost: an estimate from observed comparable refactoring work, not a measured cost of this change.",
		"- Conditional on: behavior_transport.", "- Net value over the horizon: unknown.", "decision threshold 0.8 (configured policy)",
		"## Context policy feedback", "**keep** (expansion_is_normal, less_total_navigation, no_worse_outcome_with_delivery)",
		"  - total_navigation_per_task: 4.5 operations_per_task"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Index(md, "## Summary") > strings.Index(md, "## Findings") || strings.Index(md, "## Findings") > strings.Index(md, "## Plan 1") {
		t.Error("findings come after the plans")
	}
	// seven findings: five are shown
	for len(fs) < 7 {
		fs = append(fs, fs[1])
	}
	payload["findings"] = fs
	md = Render(im, cur, "x.json")
	if strings.Count(md, "### Finding ") != 5 || !strings.Contains(md, "2 more findings are not shown.") {
		t.Errorf("limit: %d findings shown", strings.Count(md, "### Finding "))
	}
	delete(payload, "findings")
	delete(payload, "abstentions")
	delete(payload, "context_feedback")
	delete(payload, "project_comparisons")
	if md := Render(im, cur, "x.json"); strings.Contains(md, "## Findings") || strings.Contains(md, "## Summary") {
		t.Error("a set without findings shows a findings section")
	}
}

// BenchmarkRender measures the local report of the v0.2 example set (3 plans, 2 findings,
// 1 comparison, 1 feedback entry).
// Run: go test ./internal/project -run '^$' -bench Render -benchmem
func BenchmarkRender(b *testing.B) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", "recommendation-findings.json"))
	if err != nil {
		b.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	im := &Imported{Result: hint.ImportResult{Document: "accepted"}, Doc: doc}
	cur := &Current{Names: map[string]string{}}
	var n int
	for i := 0; i < b.N; i++ {
		n = len(Render(im, cur, "x.json"))
	}
	b.ReportMetric(float64(n), "report_bytes")
}

// C7BE-23: a stored decision that the current rules refuse (a choice from root-session or
// unlabeled counters) is rendered as historical and unverified, never as an active choice; an
// unlabeled delta says its counter scope is unknown.
func TestRenderRefusesUnsupportedStoredDecisions(t *testing.T) {
	var md strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&md, f, a...) }
	legacy := map[string]any{"chosen": "CONTEXT_ONLY", "evidence": "bound_eval_pairs", "alternatives": []any{
		map[string]any{"intervention": "CONTEXT_ONLY", "state": "chosen",
			"effort_delta": map[string]any{"observed": -990.0, "interval": map[string]any{"lower": -990.0, "upper": -990.0}, "unit": "tokens_per_attempt", "coverage": []any{3.0, 3.0}}}}}
	renderDecision(w, legacy)
	out := md.String()
	if !strings.Contains(out, "**historical, unverified**") || strings.Contains(out, "heuristic; cost model unavailable, so no net value or break-even): **CONTEXT_ONLY**") ||
		!strings.Contains(out, "[counter scope unknown, not task effort]") {
		t.Fatalf("legacy decision:\n%s", out)
	}
	md.Reset()
	full := map[string]any{"chosen": "CONTEXT_ONLY", "evidence": "bound_eval_pairs", "alternatives": []any{
		map[string]any{"intervention": "CONTEXT_ONLY", "state": "chosen",
			"effort_delta": map[string]any{"observed": -50.0, "interval": map[string]any{"lower": -80.0, "upper": -20.0}, "unit": "tokens_per_attempt", "coverage": []any{3.0, 3.0},
				"counter_semantics": "hint-task-tokens-v1", "scope": "full_task"},
			"success_delta": map[string]any{"observed": 0.1, "interval": map[string]any{"lower": 0.0, "upper": 0.2}, "unit": "success_fraction", "coverage": []any{3.0, 3.0}},
			"scope":         map[string]any{"pair_id": "p", "paired_objectives": 3.0}}}} // a synthetic complete whole-task fixture
	renderDecision(w, full)
	if out := md.String(); !strings.Contains(out, "**CONTEXT_ONLY**") || !strings.Contains(out, "[whole-task tokens]") {
		t.Fatalf("whole-task decision:\n%s", out)
	}
	// the same complete numbers labeled as provider totals: never a decision, never task effort
	md.Reset()
	prov := map[string]any{"chosen": "CONTEXT_ONLY", "evidence": "bound_eval_pairs", "alternatives": []any{map[string]any{}}}
	for k, v := range full["alternatives"].([]any)[0].(map[string]any) {
		prov["alternatives"].([]any)[0].(map[string]any)[k] = v
	}
	pe := map[string]any{}
	for k, v := range full["alternatives"].([]any)[0].(map[string]any)["effort_delta"].(map[string]any) {
		pe[k] = v
	}
	pe["counter_semantics"] = "openai-compat-provider-totals-v1"
	prov["alternatives"].([]any)[0].(map[string]any)["effort_delta"] = pe
	renderDecision(w, prov)
	if out := md.String(); !strings.Contains(out, "**historical, unverified**") || strings.Contains(out, "[whole-task tokens]") ||
		!strings.Contains(out, "[provider-reported totals of the observed requests; cache and reasoning semantics unknown; not task effort]") {
		t.Fatalf("provider totals decision:\n%s", out)
	}
	// whole-task scope without whole-task semantics is not task effort
	if l := effortDelta(map[string]any{"observed": 1.0, "coverage": []any{1.0, 1.0}, "scope": "full_task"}); !strings.Contains(l, "[counter scope unknown, not task effort]") {
		t.Fatal(l)
	}
	// whole-task labels without data (null observed and interval, zero or partial coverage) are
	// not evidence: such a stored decision is historical and unverified
	for _, eff := range []map[string]any{
		{"observed": nil, "interval": nil, "unit": "tokens_per_attempt", "coverage": []any{0.0, 3.0}, "counter_semantics": "hint-task-tokens-v1", "scope": "full_task"},
		{"observed": -990.0, "interval": map[string]any{"lower": -990.0, "upper": -990.0}, "unit": "tokens_per_attempt", "coverage": []any{1.0, 3.0},
			"counter_semantics": "hint-task-tokens-v1", "scope": "full_task"},
	} {
		md.Reset()
		alt := full["alternatives"].([]any)[0].(map[string]any)
		forged := map[string]any{"chosen": "CONTEXT_ONLY", "evidence": "bound_eval_pairs", "alternatives": []any{
			map[string]any{"intervention": "CONTEXT_ONLY", "state": "chosen", "effort_delta": eff, "success_delta": alt["success_delta"], "scope": alt["scope"]}}}
		renderDecision(w, forged)
		if out := md.String(); !strings.Contains(out, "**historical, unverified**") {
			t.Fatalf("label-only decision rendered as active:\n%s", out)
		}
	}
}

// The effect basis of a forecast (draft v0.2): a model projection keeps its conditions, a
// synthetic observed comparison says "synthetic" before "observed", an observed comparison with
// an unknown cost keeps unknown economics, and a set without the field renders as before.
func TestRenderEffectBasis(t *testing.T) {
	render := func(path string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return Render(&Imported{Result: hint.ImportResult{Document: "accepted"}, Doc: doc}, &Current{Names: map[string]string{}}, "x.json")
	}
	if md := render("examples/v0.2/recommendation-findings.json"); strings.Contains(md, "Effect basis") {
		t.Error("a forecast without an effect basis shows one")
	}
	md := render("tests/conformance/valid/effect-basis-model.json")
	for _, want := range []string{"- Effect basis: model projection: the model's mean per-task delta over the sampled task mix, with a model-conditional interval (not a prediction for one task).",
		"- Conditional on: behavior_transport, coordination_unmodeled."} {
		if !strings.Contains(md, want) {
			t.Errorf("model basis: missing %q in\n%s", want, md)
		}
	}
	observed := "- Effect basis (synthetic example, not project evidence): observed controlled comparison over 3 objectives: -1200 tokens per task (-1800 to -600, 0.9 interval); " +
		"the uncertainty of a mean, not a prediction for one task and not a validated effect of this change in production (policy controlled-effect-v1). The model prediction is shown separately."
	md = render("tests/conformance/valid/effect-basis-observed-synthetic.json")
	if !strings.Contains(md, observed) || !strings.Contains(md, "One-time cost: a stated scenario") {
		t.Errorf("synthetic observed basis with a scenario cost:\n%s", md)
	}
	md = render("tests/conformance/valid/effect-basis-observed-unknown-cost.json")
	if !strings.Contains(md, observed) || !strings.Contains(md, "- Net value over the horizon: unknown.") || !strings.Contains(md, "One-time cost: unknown (so no net value or break-even).") {
		t.Errorf("observed basis with an unknown cost:\n%s", md)
	}
}
