package report

// These checks establish receipt consistency only. No writer or runner authority is
// enabled by accepting versions 0.3.0 or 0.3.1. Referenced history is self-contained in the receipt.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
)

func object(x any) map[string]any { m, _ := x.(map[string]any); return m }
func array(x any) []any           { a, _ := x.([]any); return a }
func sameValue(a, b any) bool     { return reflect.DeepEqual(a, b) }

func sealed(m map[string]any, key string) bool {
	rest := map[string]any{}
	for k, v := range m {
		if k != key {
			rest[k] = v
		}
	}
	d, e := hint.Digest(rest)
	return e == nil && d == str(m[key])
}

// projectHistoryConsistent is the existing receipt chain rule shared with intent-only 0.3.1.
// It validates assertions, not external latest history or real pre-work ordering.
func projectHistoryConsistent(rs []map[string]any) bool {
	if len(rs) == 0 {
		return false
	}
	start := rs[0]
	for i, r := range rs {
		if r["revision"].(float64) != float64(i+1) || str(r["origin"]) != "project" || str(r["scope_digest"]) != str(start["scope_digest"]) {
			return false
		}
		if i > 0 && str(r["predecessor"]) != str(rs[i-1]["revision_digest"]) {
			return false
		}
		if i > 0 { // Append-only attempt order and member tasks; terminal attempts stay immutable.
			if str(rs[i-1]["state"]) == "closed" {
				return false
			}
			oldAttempts := array(rs[i-1]["attempts"])
			newAttempts := array(r["attempts"])
			if len(newAttempts) < len(oldAttempts) {
				return false
			}
			for j, x := range oldAttempts {
				a, b := object(x), object(newAttempts[j])
				if str(a["attempt_ref"]) != str(b["attempt_ref"]) {
					return false
				}
				for _, id := range array(a["task_ids"]) {
					found := false
					for _, v := range array(b["task_ids"]) {
						if id == v {
							found = true
						}
					}
					if !found {
						return false
					}
				}
				for _, id := range array(a["late_tasks"]) {
					found := false
					for _, v := range array(b["late_tasks"]) {
						if id == v {
							found = true
						}
					}
					if !found {
						return false
					}
				}
			}
			current := map[string]map[string]any{}
			for _, a := range array(r["attempts"]) {
				am := object(a)
				if current[str(am["attempt_ref"])] != nil {
					return false
				}
				current[str(am["attempt_ref"])] = am
			}
			for _, a := range array(rs[i-1]["attempts"]) {
				am := object(a)
				now := current[str(am["attempt_ref"])]
				if now == nil || (str(am["outcome"]) != "open" && !sameValue(am, now)) {
					return false
				}
			}
		}
	}

	return true
}

func projectOriginalIntent(rs []map[string]any) bool {
	if len(rs) == 0 {
		return false
	}
	start := rs[0]
	return start["revision"].(float64) == 1 && start["predecessor"] == nil &&
		str(start["origin"]) == "project" && str(start["state"]) == "open" &&
		len(array(start["attempts"])) == 0 && object(object(start["scope"])["verification_intent"]) != nil
}

func prospectiveStartBinding(scope map[string]any) bool {
	intent := object(scope["verification_intent"])
	binding := object(intent["start_binding"])
	if object(scope["plan"]) == nil || binding == nil {
		return false
	}
	for _, id := range array(scope["base_snapshots"]) {
		if id == binding["base_snapshot_id"] {
			return true
		}
	}
	return false
}

func projectProblem(m map[string]any) error {
	if str(m["report_version"]) != "0.3.0" && str(m["report_version"]) != "0.3.1" {
		return nil
	}
	invalid := func() error { return fmt.Errorf("%w: project verification binding", ErrInvalid) }
	units := map[string][]map[string]any{}
	seenRevisions := map[string]map[float64]bool{}
	executions := map[string]bool{}
	for _, x := range array(m["trials"]) {
		executions[str(object(x)["trial_ref"])] = true
	}
	for _, x := range array(m["work_units"]) {
		u := object(x)
		ref := str(u["unit_ref"])
		n := u["revision"].(float64)
		if seenRevisions[ref] == nil {
			seenRevisions[ref] = map[float64]bool{}
		}
		if seenRevisions[ref][n] || (n == 1) != (u["predecessor"] == nil) {
			return invalid()
		}
		seenRevisions[ref][n] = true
		scope := object(u["scope"])
		d, e := hint.Digest(scope)
		if e != nil || d != str(u["scope_digest"]) || !sealed(u, "revision_digest") {
			return invalid()
		}
		units[str(u["unit_ref"])] = append(units[str(u["unit_ref"])], u)
		executions[str(scope["run_ref"])] = true
		executions[str(u["unit_ref"])] = true
		for _, x := range array(u["attempts"]) {
			a := object(x)
			executions[str(a["attempt_ref"])] = true
			executions[str(a["trial_ref"])] = true
			for _, id := range array(a["task_ids"]) {
				executions[str(id)] = true
			}
		}
	}
	// Unlike legacy assertions, 0.3.1 also validates prospective intent-only history.
	// Detect intent anywhere, so adding it after an original intent-free start refuses.
	if str(m["report_version"]) == "0.3.1" {
		for _, rs := range units {
			hasIntent := false
			for _, r := range rs {
				_, present := object(r["scope"])["verification_intent"]
				hasIntent = hasIntent || present
			}
			if !hasIntent {
				continue
			}
			sort.Slice(rs, func(i, j int) bool { return rs[i]["revision"].(float64) < rs[j]["revision"].(float64) })
			if !projectOriginalIntent(rs) || !projectHistoryConsistent(rs) || !prospectiveStartBinding(object(rs[0]["scope"])) {
				return invalid()
			}
		}
	}
	records := map[string]map[string]any{}
	verifyExecutions := map[string]bool{}
	for _, x := range array(m["project_verifications"]) {
		p := object(x)
		ref := str(p["verification_ref"])
		if old := records[ref]; old != nil {
			if !sameValue(old, p) {
				return invalid()
			}
			continue
		}
		if !sealed(p, "content_digest") {
			return invalid()
		}
		records[ref] = p
		if verifyExecutions[str(p["verification_execution_ref"])] {
			return invalid()
		}
		verifyExecutions[str(p["verification_execution_ref"])] = true
		w, v := object(p["work"]), object(p["verifier"])
		rs := units[str(w["unit_ref"])]
		if len(rs) == 0 || executions[str(p["verification_execution_ref"])] {
			return invalid()
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i]["revision"].(float64) < rs[j]["revision"].(float64) })
		start := rs[0]
		scope := object(start["scope"])
		intent := object(scope["verification_intent"])
		if !projectOriginalIntent(rs) {
			return invalid()
		}
		if str(w["start_scope_digest"]) != str(start["scope_digest"]) {
			return invalid()
		}
		d, _ := hint.Digest(intent)
		if d != str(w["verification_intent_digest"]) {
			return invalid()
		}
		for k, val := range intent {
			if !sameValue(val, v[k]) {
				return invalid()
			}
		}
		plan := object(scope["plan"])
		actualPlan := object(p["plan"])
		if plan == nil || !sameValue(plan["recommendation_set_id"], actualPlan["recommendation_set_id"]) || !sameValue(plan["plan_id"], actualPlan["plan_id"]) {
			return invalid()
		}
		if str(m["report_version"]) == "0.3.1" && !sameValue(object(intent["start_binding"])["recommendation_set_digest"], actualPlan["recommendation_set_digest"]) {
			return invalid()
		}
		var frozen map[string]any
		frozenIndex := -1
		if !projectHistoryConsistent(rs) {
			return invalid()
		}
		for i, r := range rs {
			if str(r["revision_digest"]) == str(w["frozen_open_revision_digest"]) {
				frozen = r
				frozenIndex = i
			}
		}
		if frozen == nil || str(frozen["state"]) != "open" || frozenIndex < len(rs)-2 {
			return invalid()
		}
		var attempt map[string]any
		for _, x := range array(frozen["attempts"]) {
			a := object(x)
			if str(a["attempt_ref"]) == str(w["attempt_ref"]) {
				attempt = a
			}
		}
		if attempt == nil || str(attempt["outcome"]) != "open" || len(array(attempt["task_ids"])) == 0 || len(array(attempt["late_tasks"])) != 0 || str(attempt["trial_ref"]) != "" {
			return invalid()
		}
		if frozenIndex == len(rs)-2 {
			closed := rs[len(rs)-1]
			if str(closed["state"]) != "closed" || len(array(closed["attempts"])) != len(array(frozen["attempts"])) {
				return invalid()
			}
			for _, x := range array(closed["attempts"]) {
				a := object(x)
				if str(a["attempt_ref"]) == str(w["attempt_ref"]) {
					for k, val := range attempt {
						if k != "outcome" && k != "closure" && k != "application_change_set" && !sameValue(val, a[k]) {
							return invalid()
						}
					}
					if str(object(a["closure"])["project_verification_ref"]) != ref {
						return invalid()
					}
				} else {
					found := false
					for _, y := range array(frozen["attempts"]) {
						if sameValue(x, y) {
							found = true
						}
					}
					if !found {
						return invalid()
					}
				}
			}
		}
		ids := array(v["mandatory_checks"])
		sorted := append([]any(nil), ids...)
		sort.Slice(sorted, func(i, j int) bool { return str(sorted[i]) < str(sorted[j]) })
		if !sameValue(ids, sorted) {
			return invalid()
		}
		d, _ = hint.Digest(ids)
		if d != str(v["mandatory_checks_digest"]) {
			return invalid()
		}
		verdict := object(p["verdict"])
		checks := array(verdict["checks"])
		if len(ids) != len(checks) {
			return invalid()
		}
		trusts := []string{str(v["qualification_result_trust"])}
		passed := true
		failed := false
		for i, x := range checks {
			c := object(x)
			if str(c["check_id"]) != str(ids[i]) {
				return invalid()
			}
			trusts = append(trusts, str(c["result_trust"]))
			passed = passed && str(c["status"]) == "pass"
			failed = failed || str(c["status"]) == "fail"
		}
		if str(v["qualification_state"]) != "validated" {
			trusts = append(trusts, "unknown")
		}
		if str(verdict["result_trust"]) != runner.MinTrust(trusts...) {
			return invalid()
		}
		outcome := "unknown"
		if failed {
			outcome = "failed"
		} else if passed && str(v["qualification_state"]) == "validated" && str(verdict["result_trust"]) == "process_separated" {
			outcome = "passed"
		}
		if str(verdict["outcome"]) != outcome {
			return invalid()
		}
	}
	for _, rs := range units {
		for _, u := range rs {
			for _, x := range array(u["attempts"]) {
				a := object(x)
				c := object(a["closure"])
				ref := str(c["project_verification_ref"])
				if ref == "" {
					if str(u["origin"]) == "project" && str(c["result_trust"]) != "unknown" {
						return invalid()
					}
					continue
				}
				p := records[ref]
				if p == nil || str(u["origin"]) != "project" {
					return invalid()
				}
				w := object(p["work"])
				verdict := object(p["verdict"])
				if str(u["predecessor"]) != str(w["frozen_open_revision_digest"]) || str(u["state"]) != "closed" || str(a["outcome"]) == "open" || c["validation_ref"] != nil || c["validation_task_id"] != nil {
					return invalid()
				}
				if str(w["unit_ref"]) != str(u["unit_ref"]) || str(w["attempt_ref"]) != str(a["attempt_ref"]) || str(c["result_trust"]) != str(verdict["result_trust"]) {
					return invalid()
				}
				if str(a["outcome"]) == "completed" && (str(verdict["outcome"]) != "passed" || str(c["state"]) != "validated") {
					return invalid()
				}
			}
		}
	}
	return nil
}

// uniqueJSONMembers preserves Python strict-decode parity for the new contract. The
// existing legacy readers remain unchanged. Schema validation precedes this walk.
func uniqueJSONMembers(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var visit func() bool
	visit = func() bool {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		delimiter, ok := tok.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return false
				}
				k, ok := key.(string)
				if !ok || keys[k] {
					return false
				}
				keys[k] = true
				if !visit() {
					return false
				}
			}
			end, err := dec.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for dec.More() {
				if !visit() {
					return false
				}
			}
			end, err := dec.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !visit() {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}
