package main

import "path/filepath"

// v0.2 = v0.1 plus the optional context measurement of an observation. The example holds
// one deliver call measured at two stages (raw tool output and delivered context), an
// expansion, a retrieval of the original that repeats the first artifact, a stale
// expansion that was refused, and a delivery with a disabled policy and unknown bytes.

var (
	ctxPolicyRef = id("context-policy", "default", "1", "sha256:policy")
	ctxSnapshot  = id("snapshot", "shopkit", "c1")
	ctxArtifact  = id("context-artifact", "shopkit", "c1", "ledger")
	ctxRef       = id("context-ref", "shopkit", "c1", "ledger", "callers")
	ctxCall1     = id("call", "ctx1")
	ctxCall2     = id("call", "ctx2")
	ctxCall3     = id("call", "ctx3")
	ctxCall4     = id("call", "ctx4")
	ctxCall5     = id("call", "ctx5")
)

func contextDoc(act M) M {
	d := clone(act)
	d["hint_version"] = "0.2.0"
	obs := obj(d, "payload")["observations"].([]any)
	base := plain(obs[2].(map[string]any)) // a READ with a tool call
	mk := func(n int, op, call string, ctx M) M {
		o := plain(base)
		o["observation_id"] = id("event", "context", string(rune('a'+n)))
		o["op"] = op
		corr := o["correlation"].(map[string]any)
		corr["tool_call_id"] = call
		o["context"] = ctx
		delete(o, "size")
		return o
	}
	deliver := func(stage string, bytes int) M {
		return M{"action": "deliver", "stage": stage, "artifact_id": ctxArtifact, "snapshot_id": ctxSnapshot, "policy_state": "enabled",
			"policy_ref": ctxPolicyRef, "freshness": "current", "result": "delivered", "bytes": bytes, "omitted": 3,
			"tokens": M{"value": bytes / 4, "estimator": "bytes-div-4", "estimator_version": "1", "label": "estimate"}}
	}
	extra := A{
		mk(0, "DISCOVER", ctxCall1, deliver("raw_tool_output", 5400)),
		mk(1, "DISCOVER", ctxCall1, deliver("delivered_context", 1800)),
		mk(2, "DISCOVER", ctxCall2, M{"action": "expand", "stage": "delivered_context", "ref_id": ctxRef, "artifact_id": id("context-artifact", "shopkit", "c1", "callers"),
			"snapshot_id": ctxSnapshot, "policy_state": "enabled", "policy_ref": ctxPolicyRef, "freshness": "current", "result": "delivered", "bytes": 900, "omitted": 0}),
		mk(3, "READ", ctxCall3, M{"action": "retrieve_original", "stage": "delivered_context", "artifact_id": ctxArtifact, "snapshot_id": ctxSnapshot,
			"policy_state": "enabled", "policy_ref": ctxPolicyRef, "freshness": "current", "result": "delivered", "bytes": 5400, "repeat_of": ctxArtifact}),
		mk(4, "DISCOVER", ctxCall4, M{"action": "expand", "stage": "delivered_context", "ref_id": ctxRef, "policy_state": "enabled", "policy_ref": ctxPolicyRef,
			"freshness": "stale", "result": "refused_stale", "bytes": 0}),
		mk(5, "READ", ctxCall5, M{"action": "deliver", "stage": "delivered_context", "artifact_id": ctxArtifact, "snapshot_id": ctxSnapshot,
			"policy_state": "disabled", "result": "delivered", "bytes": nil, "freshness": "unknown"}),
	}
	obj(d, "payload")["observations"] = append(obs, extra...)
	return reseal(d)
}

// contextPolicy is the v0.2 export policy: the v0.1 fields plus the context measurement.
func contextPolicy(pol M) M {
	p := clone(pol)
	p["hint_version"] = "0.2.0"
	f := obj(p, "payload", "fields")
	f["Observation"] = append(f["Observation"].([]any), "context")
	return reseal(p)
}

// findingsDoc is a v0.2 recommendation set with effort findings: a supported repeated
// navigation whose stale-index cause is supported by a stale context measurement, a limited
// large-read finding, and one abstention.
func findingsDoc(rec, act, ctx M) M {
	d := clone(rec)
	d["hint_version"] = "0.2.0"
	for _, p := range obj(d, "payload")["plans"].([]any) {
		p.(map[string]any)["plan_schema"] = "0.2.0" // a plan names the version of its document
	}
	plan := obj(d, "payload", "plans", 0)
	snap := plan["snapshots"].([]any)[0].(map[string]any)["snapshot_id"]
	targets := plan["targets"].([]any)
	obs := obj(act, "payload")["observations"].([]any)
	cobs := obj(ctx, "payload")["observations"].([]any)
	oid := func(o any) any { return o.(map[string]any)["observation_id"] }
	task := id("task", "shopkit", "1")
	q := func(metric, unit string, v any, known, total int) M {
		st := "observed"
		if v == nil {
			st = "unknown"
		}
		return M{"metric": metric, "unit": unit, "value": v, "state": st, "coverage": A{known, total}}
	}
	nav := M{"finding_id": id("finding", "repeated-navigation"), "problem": "repeated_navigation", "evidence_state": "supported",
		"quantities": A{q("repeats_without_observed_edit", "operations", 4, 6, 6), q("repeats_after_edit", "operations", 1, 6, 6), q("tasks", "tasks", 3, 3, 3)},
		"scope":      M{"task_ids": A{task, id("task", "shopkit", "2"), id("task", "shopkit", "3")}, "task_count": 3, "snapshot_ids": A{snap}},
		"targets":    A{targets[0]}, "observation_ids": A{oid(obs[2]), oid(cobs[len(cobs)-2])},
		"causes": A{
			M{"cause": "stale_index", "state": "supported", "basis": A{"stale_context_measurement", "repeat_without_observed_edit"}},
			M{"cause": "architectural_coupling", "state": "not_supported", "basis": A{"single_component"}},
			M{"cause": "intentional_repeat", "state": "not_excluded", "basis": A{"repeat_after_own_edit"}}},
		"interventions": A{"CONTEXT_ONLY", "COLLECT_MORE_EVIDENCE"},
		"next_step":     M{"check": "refresh_index_and_recheck", "targets": A{targets[0]}}, "priority": "heuristic",
		"decision": M{"chosen": "COLLECT_MORE_EVIDENCE", "cost_model": "unavailable", "basis": "heuristic", "evidence": "unbound",
			"alternatives": A{
				M{"intervention": "CONTEXT_ONLY", "state": "unbound", "one_time_source": "configuration", "effort_delta": nil, "success_delta": nil, "scope": nil},
				M{"intervention": "REFACTOR_CODE", "state": "unbound", "one_time_source": "unknown", "effort_delta": nil, "success_delta": nil, "scope": nil}}}}
	read := M{"finding_id": id("finding", "large-read"), "problem": "large_low_selectivity_read", "evidence_state": "limited",
		"quantities": A{q("large_reads", "operations", 2, 2, 5), q("large_read_bytes", "bytes", nil, 2, 5)},
		"scope":      M{"task_ids": A{task}, "task_count": 1, "snapshot_ids": A{snap}},
		"targets":    A{}, "observation_ids": A{oid(obs[2])},
		"causes": A{M{"cause": "insufficient_context", "state": "not_excluded", "basis": A{"large_read_without_edit", "low_coverage"}},
			M{"cause": "necessary_exploration", "state": "not_excluded", "basis": A{}}},
		"interventions": A{"CONTEXT_ONLY", "COLLECT_MORE_EVIDENCE"},
		"next_step":     M{"check": "collect_more_tasks", "targets": A{}}, "priority": "heuristic"}
	p := obj(d, "payload")
	p["findings"] = A{nav, read}
	p["abstentions"] = A{M{"problem": "coordination_wait", "reason": "insufficient_coverage"}}
	p["project_comparisons"] = A{M{"pair_id": id("pair", "context-only"), "arm": "context_only", "partition": "held_out", "paired_objectives": 6, "task_families": 3,
		"effort_delta":   M{"observed": -120.5, "interval": M{"lower": -400, "upper": 300}, "unit": "tokens_per_attempt", "coverage": A{6, 6}},
		"success_delta":  M{"observed": 0.1, "interval": M{"lower": 0, "upper": 0.2}, "unit": "success_fraction", "coverage": A{6, 6}},
		"objective_refs": A{id("objective", "1")}, "intervention_refs": A{id("eval", "variant-1")}, "baseline_refs": A{id("eval", "baseline-1")},
		"label": "observed_unbound"}}
	p["context_feedback"] = A{M{"feedback_id": id("feedback", "policy-1"), "policy_ref": ctxPolicyRef, "proposal": "keep", "basis_label": "heuristic",
		"basis": A{"expansion_is_normal", "less_total_navigation", "no_worse_outcome_with_delivery"},
		"quantities": A{q("tasks_with_delivery", "tasks", 12, 12, 12), q("comparable_tasks", "tasks", 15, 15, 15), q("matched_groups", "groups", 3, 3, 3),
			q("expansions", "operations", 9, 20, 20), q("navigation_after_delivery_per_task", "operations_per_task", 2.5, 12, 12),
			q("total_navigation_per_task", "operations_per_task", 4.5, 12, 12), q("total_navigation_per_comparable_task", "operations_per_task", 6.0, 15, 15),
			q("pass_rate_with_delivery", "fraction", 0.92, 12, 12), q("pass_rate_comparable", "fraction", 0.87, 15, 15)},
		"task_ids": A{task}, "observation_ids": A{oid(cobs[len(cobs)-6])}}}
	return reseal(d)
}

func writeV02(root string, act, pol, rec M) (ctx, findings M) {
	ex := func(n string) string { return filepath.Join(root, "examples", "v0.2", n) }
	ctx = contextDoc(act)
	findings = findingsDoc(rec, act, ctx)
	write(ex("activity-context.json"), ctx)
	write(ex("export-policy.json"), contextPolicy(pol))
	write(ex("recommendation-findings.json"), findings)
	return ctx, findings
}

// v02Invalid are the negative cases of the context measurement and of the findings.
func v02Invalid() []invalidCase {
	first := func(d M) int { // index of the first context measurement
		for i, o := range obj(d, "payload")["observations"].([]any) {
			if _, ok := o.(map[string]any)["context"]; ok {
				return i
			}
		}
		panic("no context measurement")
	}
	ctx := func(d M, i int) M { return obj(d, "payload", "observations", first(d)+i, "context") }
	o0 := func(d M) M { return obj(d, "payload", "observations", first(d)) }
	return []invalidCase{
		{"context-on-edit-op", "activity-context.json", "schema", "observation", func(d M) {
			o0(d)["op"] = "EDIT_INTENT"
		}, nil},
		{"context-without-tool-call", "activity-context.json", "schema", "observation", func(d M) {
			delete(o0(d)["correlation"].(map[string]any), "tool_call_id")
		}, nil},
		{"context-enabled-without-policy-ref", "activity-context.json", "schema", "observation", func(d M) {
			delete(ctx(d, 0), "policy_ref")
		}, nil},
		{"context-disabled-with-policy-ref", "activity-context.json", "schema", "observation", func(d M) {
			ctx(d, 5)["policy_ref"] = ctxPolicyRef
		}, nil},
		{"context-tokens-without-estimator", "activity-context.json", "schema", "observation", func(d M) {
			delete(ctx(d, 0)["tokens"].(map[string]any), "estimator")
		}, nil},
		{"context-tokens-observed-label", "activity-context.json", "schema", "observation", func(d M) {
			ctx(d, 0)["tokens"].(map[string]any)["label"] = "observed"
		}, nil},
		{"context-stale-refusal-marked-current", "activity-context.json", "schema", "observation", func(d M) {
			ctx(d, 4)["freshness"] = "current"
		}, nil},
		{"context-delivered-without-artifact", "activity-context.json", "schema", "observation", func(d M) {
			delete(ctx(d, 2), "artifact_id")
		}, nil},
		{"context-raw-text-field", "activity-context.json", "schema", "content", func(d M) {
			ctx(d, 1)["text"] = forbidden["prompt"]
		}, nil},
		{"context-in-v01-document", "activity-context.json", "schema", "version", func(d M) {
			d["hint_version"] = "0.1.0"
		}, nil},
		{"finding-free-text", "recommendation-findings.json", "schema", "content", func(d M) {
			obj(d, "payload", "findings", 0)["summary"] = forbidden["prompt"]
		}, nil},
		{"finding-cause-free-text", "recommendation-findings.json", "schema", "content", func(d M) {
			obj(d, "payload", "findings", 0, "causes", 0)["cause"] = forbidden["identifier"]
		}, nil},
		{"finding-unknown-with-value", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "findings", 1, "quantities", 1)["value"] = 70000
		}, nil},
		{"finding-calibrated-priority", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "findings", 0)["priority"] = "calibrated"
		}, nil},
		{"finding-without-cause", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "findings", 0)["causes"] = A{}
		}, nil},
		{"decision-with-net-value", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "findings", 0, "decision")["net"] = M{"lower": 1000, "upper": 5000}
		}, nil},
		{"decision-calibrated-basis", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "findings", 0, "decision")["basis"] = "calibrated"
		}, nil},
		{"feedback-automatic-change", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "context_feedback", 0)["proposal"] = "apply_revision"
		}, nil},
		{"feedback-free-text", "recommendation-findings.json", "schema", "content", func(d M) {
			obj(d, "payload", "context_feedback", 0)["note"] = forbidden["prompt"]
		}, nil},
		{"finding-in-v01-document", "recommendation-findings.json", "schema", "version", func(d M) {
			d["hint_version"] = "0.1.0"
		}, nil},
		// a second measurement of the same call, stage and action (two stages are fine)
		{"context-duplicate-stage", "activity-context.json", "semantic", "duplicate_measurement", func(d M) {
			o := plain(o0(d))
			o["observation_id"] = id("event", "context", "dup")
			obs := obj(d, "payload")["observations"].([]any)
			obj(d, "payload")["observations"] = append(obs, o)
		}, nil},
	}
}
