package project

import (
	"fmt"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/hint"
)

// Render writes the local Markdown plan document. It shows only closed enumerations,
// numbers and the local names of resolved identifiers: a recommendation carries no text,
// so nothing in the output comes from the server as an instruction. Unresolved IDs are
// shown as IDs.
func Render(im *Imported, cur *Current, source string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# HINT recommendation plan\n\n")
	w("Source: %s  \n", source)
	w("Document check: **%s**\n\n", im.Result.Document)
	w("This document is generated locally. HINT does not change code. Review every step before you act on it.\n")
	w("Forecasts are predictions, not measured effects.\n\n")
	if im.Result.Document != "accepted" {
		w("The document was rejected (`%s`). No plan is shown.\n", im.Result.Document)
		return b.String()
	}
	plans, _ := im.Doc["payload"].(map[string]any)["plans"].([]any)
	status := map[string]hint.PlanResult{}
	for _, r := range im.Result.Plans {
		status[r.PlanID] = r
	}
	name := func(id string) string {
		if n, ok := cur.Names[id]; ok {
			return fmt.Sprintf("%s (`%s`)", n, id)
		}
		return fmt.Sprintf("unresolved `%s`", id)
	}
	renderSummary(w, im.Doc["payload"].(map[string]any), len(plans))
	renderFindings(w, im.Doc["payload"].(map[string]any), name)
	for i, p := range plans {
		m := p.(map[string]any)
		id := str(m["plan_id"])
		r := status[id]
		w("## Plan %d: %s\n\n", i+1, title(m))
		w("| Item | Value |\n|---|---|\n")
		w("| Plan ID | `%s` |\n", id)
		w("| Import status | **%s**%s |\n", r.Status, codes(r.Codes))
		w("| Decision | %s |\n", str(m["decision"]))
		w("| Intervention | %s |\n", str(m["intervention_class"]))
		if t := str(m["transformation"]); t != "" {
			w("| Transformation | %s |\n", t)
		}
		w("| Valid | %s to %s |\n", str(m["created_at"]), str(m["expires_at"]))
		if ab := str(m["authorization_basis"]); ab != "" {
			w("| Authorization basis | %s |\n", ab)
		}
		w("| Required approvals | %s |\n", joinAny(m["required_approvals"]))
		w("\n")
		switch r.Status {
		case "reject":
			w("This plan is rejected. Do not apply it. Reasons: %s.\n\n", strings.Join(r.Codes, ", "))
		case "render_only":
			w("This plan is shown for information. It has no applicable steps.\n\n")
		case "partial":
			w("Only the `supported` steps can be applied. The other steps are shown, not applicable.\n\n")
		}
		if t := list(m["targets"]); len(t) > 0 {
			w("### Targets\n\n")
			for _, x := range t {
				w("- %s\n", name(str(x)))
			}
			w("\n")
		}
		if pcs := list(m["preconditions"]); len(pcs) > 0 {
			w("### Preconditions\n\n")
			for _, x := range pcs {
				pc := x.(map[string]any)
				w("- %s", str(pc["check"]))
				if ts := list(pc["targets"]); len(ts) > 0 {
					var ns []string
					for _, t := range ts {
						ns = append(ns, name(str(t)))
					}
					w(": %s", strings.Join(ns, "; "))
				}
				w("\n")
			}
			w("\n")
		}
		if steps := list(m["steps"]); len(steps) > 0 {
			w("### Steps\n\n")
			for _, x := range steps {
				st := x.(map[string]any)
				w("%v. **%s** (%s", st["step_id"], str(st["action"]), str(st["support"]))
				if st["requires_approval"] == true {
					w(", needs approval")
				}
				if d := list(st["depends_on"]); len(d) > 0 {
					w(", after step %s", joinAny(d))
				}
				w(")\n")
				for _, t := range list(st["targets"]) {
					w("   - target: %s\n", name(str(t)))
				}
				if d := str(st["destination"]); d != "" {
					w("   - destination: %s\n", name(d))
				}
			}
			w("\n")
		}
		if rs := list(m["reasons"]); len(rs) > 0 {
			w("### Reasons\n\n")
			for _, x := range rs {
				rr := x.(map[string]any)
				w("- %s (%d observations, %d metrics as evidence)\n", str(rr["code"]), len(list(rr["observation_ids"])), len(list(rr["metric_ids"])))
			}
			w("\n")
		}
		if ps := list(m["predictions"]); len(ps) > 0 {
			w("### Forecasts (predictions, not measured effects)\n\n")
			if sim, ok := m["simulation"].(map[string]any); ok {
				w("Simulation level %s, calibration %s", str(sim["level"]), orUnknown(str(sim["calibration_status"])))
				if sim["out_of_distribution"] == true {
					w(", **out of distribution**")
				}
				w(".\n\n")
			} else {
				w("No simulation details are given.\n\n")
			}
			w("| Metric | Unit | Per | Interval | Level | Point | P(improvement) |\n|---|---|---|---|---|---|---|\n")
			for _, x := range ps {
				pr := x.(map[string]any)
				iv, _ := pr["interval"].(map[string]any)
				w("| %s | %s | %s | %v to %v | %v | %s | %s |\n", str(pr["metric"]), str(pr["unit"]), str(pr["per"]),
					iv["lower"], iv["upper"], iv["level"], numOr(pr["point"]), numOr(pr["probability_improvement"]))
			}
			w("\n")
		}
		if fb, ok := m["forecast_basis"].(map[string]any); ok {
			renderForecastBasis(w, fb)
		}
		if c, ok := m["refactoring_cost_estimate"].(map[string]any); ok {
			iv, _ := c["interval"].(map[string]any)
			w("Estimated refactoring cost: %v to %v %s (level %v).\n\n", iv["lower"], iv["upper"], str(c["unit"]), iv["level"])
		}
		if h, ok := m["break_even_horizon"].(map[string]any); ok {
			if h["unbounded"] == true {
				w("Break-even horizon: unbounded (no break-even is expected).\n\n")
			} else if iv, ok := h["interval"].(map[string]any); ok {
				w("Break-even horizon: %v to %v tasks (level %v).\n\n", iv["lower"], iv["upper"], iv["level"])
			}
		}
		if rc := list(m["risk_constraints"]); len(rc) > 0 {
			w("### Constraints\n\n")
			for _, x := range rc {
				c := x.(map[string]any)
				w("- %s: %s (deterministic check: %v)\n", str(c["kind"]), str(c["requirement"]), c["deterministic"])
			}
			w("\n")
		}
	}
	return b.String()
}

// renderSummary writes the short answer first: the problems, the proposed action and next
// step, the evaluation status, and what kind of value each part of the report is.
func renderSummary(w func(string, ...any), payload map[string]any, plans int) {
	fs, abst := list(payload["findings"]), list(payload["abstentions"])
	cmp, fb := list(payload["project_comparisons"]), list(payload["context_feedback"])
	if len(fs) == 0 && len(abst) == 0 && len(cmp) == 0 && len(fb) == 0 {
		return
	}
	w("## Summary\n\n")
	supported := 0
	for _, x := range fs {
		if x.(map[string]any)["evidence_state"] == "supported" {
			supported++
		}
	}
	w("- Problems found: %d (%d with enough evidence, %d limited).", len(fs), supported, len(fs)-supported)
	if len(abst) > 0 {
		var ps []string
		for _, x := range abst {
			ps = append(ps, str(x.(map[string]any)["problem"]))
		}
		w(" Not diagnosed: %s.", strings.Join(ps, ", "))
	}
	w("\n")
	if len(fs) > 0 {
		f := fs[0].(map[string]any)
		action := joinAny(f["interventions"])
		if d, ok := f["decision"].(map[string]any); ok {
			action = str(d["chosen"])
		}
		ns, _ := f["next_step"].(map[string]any)
		w("- First problem: %s (%s). Proposed action: %s. Next step: %s.\n", str(f["problem"]), str(f["evidence_state"]), action, str(ns["check"]))
	}
	if len(cmp) > 0 {
		w("- Evaluation: %d eval comparisons of your project (observed; not bound to a finding, so they decide nothing alone).\n", len(cmp))
	} else {
		w("- Evaluation: no eval comparison yet. Run the variants with `git hint eval run` and send a report to measure a change.\n")
	}
	for _, x := range fb {
		f := x.(map[string]any)
		w("- Context policy `%s`: %s.\n", str(f["policy_ref"]), str(f["proposal"]))
	}
	w("- Plans: %d. Rejected and refused plans stay listed. HINT does not record whether you applied a plan; after a change, collect again and compare (`git hint changes`, `git hint eval compare`).\n\n", plans)
	w("Kinds of values: **observed** = counted in your activity or eval runs; **estimated** = a labeled estimate (for example tokens from bytes); **predicted** = a plan forecast, not measured; **experimentally supported** = an effect from paired runs, shown only in an effect report. A cause is a hypothesis unless it is `supported`.\n\n")
}

// MaxShownFindings is the number of findings that the ordinary report shows.
const MaxShownFindings = 5

// renderFindings writes the effort findings of a v0.2 recommendation set (at most
// MaxShownFindings, in the server's heuristic order) and the abstentions. Every value is an
// enumeration, a number or a resolved local name.
func renderFindings(w func(string, ...any), payload map[string]any, name func(string) string) {
	fs, abst := list(payload["findings"]), list(payload["abstentions"])
	defer renderFeedback(w, list(payload["context_feedback"]))
	defer renderComparisons(w, list(payload["project_comparisons"]))
	if len(fs) == 0 && len(abst) == 0 {
		return
	}
	w("## Findings\n\n")
	w("The order is a heuristic priority, not a calibrated ranking. A cause is a hypothesis unless its state is `supported`.\n\n")
	for i, x := range fs {
		if i == MaxShownFindings {
			w("%d more findings are not shown.\n\n", len(fs)-MaxShownFindings)
			break
		}
		f := x.(map[string]any)
		sc, _ := f["scope"].(map[string]any)
		w("### Finding %d: %s (%s)\n\n", i+1, str(f["problem"]), str(f["evidence_state"]))
		w("Scope: %v tasks, %d observations as evidence.\n\n", sc["task_count"], len(list(f["observation_ids"])))
		w("| Quantity | Unit | Value | Coverage |\n|---|---|---|---|\n")
		for _, y := range list(f["quantities"]) {
			q := y.(map[string]any)
			cov := list(q["coverage"])
			v := "unknown"
			if q["state"] == "observed" {
				v = fmt.Sprint(q["value"])
			}
			w("| %s | %s | %s | %v of %v |\n", str(q["metric"]), str(q["unit"]), v, cov[0], cov[1])
		}
		w("\nCauses:\n\n")
		for _, y := range list(f["causes"]) {
			c := y.(map[string]any)
			w("- %s: **%s**", str(c["cause"]), str(c["state"]))
			if b := list(c["basis"]); len(b) > 0 {
				w(" (%s)", joinAny(b))
			}
			w("\n")
		}
		w("\nCandidate interventions: %s.\n\n", joinAny(f["interventions"]))
		if t := list(f["targets"]); len(t) > 0 {
			w("Targets:\n\n")
			for _, y := range t {
				w("- %s\n", name(str(y)))
			}
			w("\n")
		}
		if d, ok := f["decision"].(map[string]any); ok {
			renderDecision(w, d)
		}
		ns, _ := f["next_step"].(map[string]any)
		w("Next step: **%s**", str(ns["check"]))
		var tn []string
		for _, y := range list(ns["targets"]) {
			tn = append(tn, name(str(y)))
		}
		if len(tn) > 0 {
			w(" on %s", strings.Join(tn, "; "))
		}
		w(".\n\n")
	}
	if len(abst) > 0 {
		w("Not diagnosed:\n\n")
		for _, y := range abst {
			a := y.(map[string]any)
			w("- %s: %s\n", str(a["problem"]), str(a["reason"]))
		}
		w("\n")
	}
}

// renderForecastBasis writes how to read a plan's forecast: the objective and unit, the
// horizon, the model-conditional probability and net value with their threshold and level, the
// basis of the one-time cost, the evidence and every stated condition. Unknown stays unknown.
func renderForecastBasis(w func(string, ...any), fb map[string]any) {
	obj, _ := fb["objective"].(map[string]any)
	w("Forecast basis (a model-conditional forecast, not a measurement): objective %s in %s over %v future tasks.\n\n", str(obj["metric"]), str(obj["unit"]), fb["horizon_tasks"])
	basis := map[string]string{"heuristic_default": "heuristic default", "configured_policy": "configured policy"}[str(fb["threshold_basis"])]
	p := "unknown"
	if v, ok := fb["p_net_gain_positive"].(float64); ok {
		p = fmt.Sprint(v)
	}
	w("- P(net value over the horizon > 0) under the model: %s; decision threshold %v (%s).\n", p, fb["threshold"], basis)
	if iv, ok := fb["net_gain_interval"].(map[string]any); ok {
		w("- Net value over %v tasks: %v to %v %s (%v interval).\n", fb["horizon_tasks"], iv["lower"], iv["upper"], str(obj["unit"]), iv["level"])
	} else {
		w("- Net value over the horizon: unknown.\n")
	}
	cost := map[string]string{"observed_comparable_work": "an estimate from observed comparable refactoring work, not a measured cost of this change",
		"assumption_scenario": "a stated scenario, not an estimate from this project"}[str(fb["cost_basis"])]
	if cost == "" {
		cost = "unknown (so no net value or break-even)"
	}
	w("- One-time cost: %s.\n", cost)
	ev := map[string]string{"synthetic_fixture": "a synthetic fixture (not project validation)", "project_history": "this project's history (baseline evidence; it does not validate the effect)"}[str(fb["evidence"])]
	w("- Evidence: %s.\n", ev)
	if eb, ok := fb["effect_basis"].(map[string]any); ok {
		renderEffectBasis(w, eb, fb["evidence"] == "synthetic_fixture")
	}
	if c := list(fb["conditional_on"]); len(c) > 0 {
		w("- Conditional on: %s.\n", joinAny(c))
	}
	w("\n")
}

// renderEffectBasis writes which effect distribution supplies a forecast's economics (draft
// v0.2). A synthetic example says so before any "observed" wording; an observed comparison is
// the uncertainty of a mean, never a prediction for one task or a validated production effect.
// The labels confer no evidence: the document checks validate their form only.
func renderEffectBasis(w func(string, ...any), eb map[string]any, synthetic bool) {
	switch eb["source"] {
	case "model_projection":
		w("- Effect basis: model projection: the model's mean per-task delta over the sampled task mix, with a model-conditional interval (not a prediction for one task).\n")
	case "observed_objective_clustered":
		label := "- Effect basis: "
		if synthetic {
			label = "- Effect basis (synthetic example, not project evidence): "
		}
		e, _ := eb["effect"].(map[string]any)
		iv, _ := e["interval"].(map[string]any)
		w("%sobserved controlled comparison over %v objectives: %v %s per task (%v to %v, %v interval); the uncertainty of a mean, "+
			"not a prediction for one task and not a validated effect of this change in production (policy %s). The model prediction is shown separately.\n",
			label, eb["objectives"], e["point"], str(e["unit"]), iv["lower"], iv["upper"], iv["level"], str(eb["evidence_policy"]))
	default:
		w("- Effect basis: unknown.\n")
	}
}

// renderComparisons writes the project-level eval variant comparisons: observations that no
// finding is bound to, so they decide nothing by themselves.
func renderComparisons(w func(string, ...any), cs []any) {
	if len(cs) == 0 {
		return
	}
	w("## Project eval comparisons\n\n")
	w("Observed on the project's eval tasks; not bound to a finding, so they do not decide an intervention.\n\n")
	w("| Pair | Arm | Partition | Objectives | Tokens per attempt (variant - baseline) | Success difference |\n|---|---|---|---|---|---|\n")
	for _, x := range cs {
		c := x.(map[string]any)
		w("| `%s` | %s | %s | %v | %s | %s |\n", str(c["pair_id"]), str(c["arm"]), str(c["partition"]), c["paired_objectives"], effortDelta(c["effort_delta"]), delta(c["success_delta"]))
	}
	w("\n")
}

// delta writes an observed difference with its interval and coverage.
func delta(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "unknown"
	}
	cov := list(m["coverage"])
	iv, ok := m["interval"].(map[string]any)
	if !ok {
		return fmt.Sprintf("%s (no interval; %v of %v)", numOr(m["observed"]), cov[0], cov[1])
	}
	return fmt.Sprintf("%s (%v to %v; %v of %v)", numOr(m["observed"]), iv["lower"], iv["upper"], cov[0], cov[1])
}

// effortDelta writes a token difference with the provenance of its counters (semantics and
// scope together): only whole-task counters (hint-task-tokens-v1, full_task) are task effort;
// root-session counters, provider-reported totals and unknown counters are not.
func effortDelta(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "unknown"
	}
	label := map[[2]string]string{
		{"hint-task-tokens-v1", "full_task"}:                        "whole-task tokens",
		{"harbor-0.23.0+opencode:n-tokens-root-v1", "root_session"}: "root-session raw counters (Harbor 0.23.0 + OpenCode), not task effort",
		{"openai-compat-provider-totals-v1", "full_task"}:           "provider-reported totals of the observed requests; cache and reasoning semantics unknown; not task effort",
	}[[2]string{str(m["counter_semantics"]), str(m["scope"])}]
	if label == "" {
		label = "counter scope unknown, not task effort"
	}
	return delta(v) + " [" + label + "]"
}

// renderFeedback writes the proposals on the local context policy versions. Nothing changes
// by itself: the user changes or restores the policy with git hint init --context-limits.
func renderFeedback(w func(string, ...any), fb []any) {
	if len(fb) == 0 {
		return
	}
	w("## Context policy feedback\n\n")
	w("Proposals only (heuristic). Change or restore the policy with `git hint init --context-limits`.\n\n")
	for _, x := range fb {
		f := x.(map[string]any)
		w("- policy `%s`: **%s** (%s)\n", str(f["policy_ref"]), str(f["proposal"]), joinAny(f["basis"]))
		for _, y := range list(f["quantities"]) {
			q := y.(map[string]any)
			v := "unknown"
			if q["state"] == "observed" {
				v = fmt.Sprint(q["value"])
			}
			w("  - %s: %s %s\n", str(q["metric"]), v, str(q["unit"]))
		}
	}
	w("\n")
}

// renderDecision writes the intervention comparison of a finding: observed differences of
// the project's eval pairs, never a net value or a break-even (the cost model is unavailable).
func renderDecision(w func(string, ...any), d map[string]any) {
	if hint.DecisionEffortProblem(d) {
		// a stored decision that the current rules refuse (for example a choice from root-session
		// counters): historical and unverified, never an active recommendation
		w("Intervention comparison: **historical, unverified** (stored choice `%s` is not supported under the current rules: its token differences do not cover the whole task). Not an active recommendation.\n\n", str(d["chosen"]))
	} else {
		w("Intervention comparison (heuristic; cost model unavailable, so no net value or break-even): **%s**.\n\n", str(d["chosen"]))
	}
	w("| Intervention | State | Tokens per attempt (variant - baseline) | Success difference | Pairs |\n|---|---|---|---|---|\n")
	for _, x := range list(d["alternatives"]) {
		a := x.(map[string]any)
		scope := "none"
		if sc, ok := a["scope"].(map[string]any); ok {
			scope = fmt.Sprintf("`%s`: %s, %s, %v objectives, %v families", str(sc["pair_id"]), str(sc["arm"]), str(sc["partition"]), sc["paired_objectives"], sc["task_families"])
		}
		w("| %s | %s | %s | %s | %s |\n", str(a["intervention"]), str(a["state"]), effortDelta(a["effort_delta"]), delta(a["success_delta"]), scope)
	}
	if d["evidence"] == "unbound" {
		w("\nNo eval comparison is bound to this finding's objectives, so the choice is to collect more evidence (see the project eval comparisons).\n\n")
	} else {
		w("\nObserved differences on the eval tasks bound to this finding, not a forecast for future work.\n\n")
	}
}

func title(m map[string]any) string {
	t := str(m["intervention_class"])
	if tr := str(m["transformation"]); tr != "" {
		t += " / " + tr
	}
	return t
}

func codes(c []string) string {
	if len(c) == 0 {
		return ""
	}
	cc := append([]string{}, c...)
	sort.Strings(cc)
	return " (" + strings.Join(cc, ", ") + ")"
}

func str(v any) string { s, _ := v.(string); return s }

func list(v any) []any { l, _ := v.([]any); return l }

func joinAny(v any) string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func numOr(v any) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprint(v)
}

func orUnknown(s string) string {
	if s == "" {
		return "not given"
	}
	return s
}
