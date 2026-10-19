package main

import "github.com/neprel/git-hint/tests/conformance/hintconf"

// ledgerDoc is the model-call example plus one sealed model ledger of a controlled run (draft
// v0.2): a ledger source with the run's configuration and the descriptive provider usage
// normalization, and the run's USAGE record whose canonical usage is unknown.
func ledgerDoc(act M) M {
	d := modelCallDoc(act)
	d["document_id"] = id("document", "activity", "model-ledger")
	cfg := M{
		"agent":          M{"name": "opencode", "version": M{"state": "known", "value": "2.0.26"}},
		"model":          M{"state": "known", "ref": id("eval-report:model", "local-model"), "provenance": "launch_parameter"},
		"harness":        M{"kind": "harbor", "version": M{"state": "known", "value": "0.23.0"}},
		"context_policy": M{"state": "disabled"},
		"config_refs":    M{"state": "known", "refs": A{id("eval-report:observed-config", "setting_override", "launch")}},
		"budget":         M{"max_requests": M{"state": "known", "value": 16}, "max_seconds": M{"state": "known", "value": 300}},
	}
	cd, _ := hintconf.Digest(cfg)
	src := M{"source_id": id("source", "hint-model-ledger"), "agent": "opencode", "agent_version": "2.0.26",
		"adapter": M{"name": "hint-model-ledger", "version": "0.1.0"}, "configuration": cfg, "usage_normalization": "openai-compat-usage-v1",
		"capabilities": A{"time.start", "time.end", "correlation.task_id", "correlation.task_family_id", "correlation.run_ref", "usage", "model_ledger"},
		"unavailable":  A{"correlation.session_id", "correlation.turn_id", "correlation.tool_call_id", "targets"}}
	d["sources"] = append(d["sources"].([]any), src)
	run := id("eval-report:trial", "trial-1")
	ledger := id("ledger", run)
	task, family := id("task", "task-1"), id("eval-report:family", "refactor")
	unknown := M{"state": "unknown", "unit": "tokens", "reason": "not_supported"}
	at := "2026-10-11T11:00:05Z"
	rec := M{"observation_id": id("ledger-usage", ledger, "0"), "class": "observation", "op": "USAGE", "source": "transport", "source_id": src["source_id"],
		"precision": "exact", "time": M{"clock": "collector", "start": at, "end": at},
		"correlation": M{"task_id": task, "task_family_id": family, "run_ref": run}, "outcome": M{"status": "success"},
		"usage": M{"input_tokens": unknown, "output_tokens": unknown},
		"model_ledger": M{"ledger_ref": ledger, "revision": 0, "state": "complete", "reasons": A{},
			"counts": M{"admitted": 2, "late": 0, "rejected_limit": 0, "complete": 2, "usage_partial": 0, "missing_usage": 0, "usage_conflict": 0,
				"unreadable": 0, "truncated": 0, "error": 0, "cancelled": 0, "unknown": 0},
			"components":         M{"prompt": 6091, "completion": 597, "cached": 4800, "reasoning": nil},
			"component_coverage": M{"prompt": 2, "completion": 2, "cached": 2, "reasoning": 0},
			"model_check":        "requested_model_observed", "bypass": "enforced", "scope": "isolated_container", "sealed_at": at,
			"binding": M{"run_ref": run, "task_id": task, "task_family_id": family, "configuration_digest": cd,
				"unit_ref": id("eval-report:work-unit", "controlled", "run-1", "eval-1"), "attempt_ref": id("eval-report:attempt", "controlled", "run-1", "eval-1", "1")}},
		"missing": A{"correlation.session_id", "correlation.turn_id"},
		"provenance": M{"captured_at": at, "source_event_id": id("ledger-event", ledger, "0"),
			"transformations": A{M{"step": "normalize", "tool": "hint-cli", "version": "0.1.0"}}}}
	obj(d, "payload")["observations"] = append(obj(d, "payload")["observations"].([]any), rec)
	return reseal(d)
}

// ledgerInvalid are the negative cases of the model ledger record.
func ledgerInvalid() []invalidCase {
	const base = "activity-model-ledger.json"
	l := func(d M) M { return obj(d, "payload", "observations", 6, "model_ledger") }
	counts := func(d M) M { return l(d)["counts"].(map[string]any) }
	second := func(d M, change func(r M)) {
		obs := obj(d, "payload")["observations"].([]any)
		r := plain(obs[6].(map[string]any))
		change(r)
		obj(d, "payload")["observations"] = append(obs, r)
	}
	return []invalidCase{
		{"ledger-in-v01-document", base, "schema", "version", func(d M) { d["hint_version"] = "0.1.0" }, nil},
		{"ledger-on-read", base, "schema", "observation", func(d M) { obj(d, "payload", "observations", 6)["op"] = "READ" }, nil},
		{"ledger-without-run-ref", base, "schema", "observation", func(d M) {
			delete(obj(d, "payload", "observations", 6, "correlation"), "run_ref")
		}, nil},
		{"ledger-missing-count", base, "schema", "observation", func(d M) { delete(counts(d), "unknown") }, nil},
		{"ledger-fractional-count", base, "schema", "observation", func(d M) { counts(d)["late"] = 0.5 }, nil},
		{"ledger-boolean-count", base, "schema", "observation", func(d M) { counts(d)["error"] = false }, nil},
		{"ledger-negative-count", base, "schema", "observation", func(d M) { counts(d)["error"] = -1 }, nil},
		{"ledger-unknown-reason", base, "schema", "observation", func(d M) { l(d)["reasons"] = A{"slow"} }, nil},
		{"ledger-model-verified-wording", base, "schema", "observation", func(d M) { l(d)["model_check"] = "verified" }, nil},
		{"ledger-raw-run-name", base, "schema", "observation", func(d M) { l(d)["binding"].(map[string]any)["run_ref"] = "trial-1" }, nil},
		{"ledger-without-attempt", base, "schema", "observation", func(d M) { delete(l(d)["binding"].(map[string]any), "attempt_ref") }, nil},
		{"ledger-admitted-mismatch", base, "semantic", "ledger_inconsistent", func(d M) { counts(d)["admitted"] = 3 }, nil},
		{"ledger-complete-with-late", base, "semantic", "ledger_inconsistent", func(d M) { counts(d)["late"] = 1 }, nil},
		{"ledger-complete-partial-completion", base, "semantic", "ledger_inconsistent", func(d M) {
			l(d)["component_coverage"].(map[string]any)["completion"] = 1
		}, nil},
		{"ledger-complete-watched-egress", base, "semantic", "ledger_inconsistent", func(d M) { l(d)["bypass"] = "observed_no_bypass" }, nil},
		{"ledger-complete-without-requests", base, "semantic", "ledger_inconsistent", func(d M) {
			for _, k := range []string{"admitted", "complete"} {
				counts(d)[k] = 0
			}
			for _, k := range []string{"prompt", "completion", "cached"} {
				l(d)["component_coverage"].(map[string]any)[k] = 0
				l(d)["components"].(map[string]any)[k] = nil
			}
		}, nil},
		{"ledger-component-without-coverage", base, "semantic", "ledger_inconsistent", func(d M) { l(d)["components"].(map[string]any)["reasoning"] = 40 }, nil},
		{"ledger-incomplete-without-reason", base, "semantic", "ledger_inconsistent", func(d M) { l(d)["state"] = "incomplete" }, nil},
		{"ledger-canonical-usage-known", base, "semantic", "ledger_inconsistent", func(d M) {
			obj(d, "payload", "observations", 6, "usage")["input_tokens"] = known("tokens", 6091, "exact", "harness_usage")
		}, nil},
		{"ledger-binding-other-task", base, "semantic", "ledger_inconsistent", func(d M) {
			l(d)["binding"].(map[string]any)["task_id"] = id("task", "task-2")
		}, nil},
		{"ledger-plugin-source", base, "semantic", "ledger_inconsistent", func(d M) {
			obj(d, "payload", "observations", 6)["source_id"] = d["sources"].([]any)[0].(map[string]any)["source_id"]
		}, nil},
		{"ledger-second-body-same-revision", base, "semantic", "ledger_inconsistent", func(d M) {
			second(d, func(r M) {
				r["observation_id"] = id("ledger-usage", "duplicate")
				r["model_ledger"].(map[string]any)["components"].(map[string]any)["prompt"] = 6092
			})
		}, nil},
		{"ledger-revision-new-seal-time", base, "semantic", "ledger_inconsistent", func(d M) {
			second(d, func(r M) {
				r["observation_id"] = id("ledger-usage", "revision-1")
				m := r["model_ledger"].(map[string]any)
				m["revision"], m["state"], m["reasons"], m["sealed_at"] = 1, "incomplete", A{"late_requests"}, "2026-10-11T11:00:06Z"
				m["counts"].(map[string]any)["late"] = 1
			})
		}, nil},
	}
}
