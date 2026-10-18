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

// MaxShownFindings is the number of findings that the ordinary report shows.
const MaxShownFindings = 5

// renderFindings writes the effort findings of a v0.2 recommendation set (at most
// MaxShownFindings, in the server's heuristic order) and the abstentions. Every value is an
// enumeration, a number or a resolved local name.
func renderFindings(w func(string, ...any), payload map[string]any, name func(string) string) {
	fs, abst := list(payload["findings"]), list(payload["abstentions"])
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

// renderDecision writes the intervention comparison of a finding. Unknown inputs stay
// unknown; a break-even horizon appears only when the server gives one.
func renderDecision(w func(string, ...any), d map[string]any) {
	w("Intervention comparison (heuristic, uncalibrated; %v future tasks, unit %s): **%s**.\n\n", d["horizon_tasks"], str(d["unit"]), str(d["chosen"]))
	w("| Intervention | State | One-time | Per task | Success difference | Net over the horizon |\n|---|---|---|---|---|---|\n")
	iv := func(v any) string {
		m, ok := v.(map[string]any)
		if !ok {
			return "unknown"
		}
		return fmt.Sprintf("%v to %v", m["lower"], m["upper"])
	}
	for _, x := range list(d["alternatives"]) {
		a := x.(map[string]any)
		w("| %s | %s | %s (%s) | %s | %s | %s |\n", str(a["intervention"]), str(a["state"]), iv(a["one_time"]), str(a["one_time_source"]),
			iv(a["per_task"]), iv(a["success"]), iv(a["net"]))
	}
	if be, ok := d["break_even_tasks"].(map[string]any); ok {
		w("\nBreak-even after %v to %v tasks (observed inputs, uncalibrated).\n", be["lower"], be["upper"])
	} else {
		w("\nNo break-even horizon: an input is unknown or not observed.\n")
	}
	w("\n")
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
