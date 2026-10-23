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
	// the canonical effective configuration of the plugin source and the declared task family
	// of its task (HINT_TASK_FAMILY): opaque, project-scoped references only
	src := d["sources"].([]any)[0].(map[string]any)
	src["configuration"] = M{
		"agent":          M{"name": "opencode", "version": M{"state": "known", "value": "2.0.26"}},
		"model":          M{"state": "known", "ref": id("eval-report:model", "local-model"), "provenance": "launch_parameter"},
		"harness":        M{"kind": "direct", "version": M{"state": "not_applicable"}},
		"context_policy": M{"state": "enabled", "ref": ctxPolicyRef, "version": 1},
		"config_refs":    M{"state": "known", "refs": A{}},
		"budget":         M{"max_requests": M{"state": "unknown"}, "max_seconds": M{"state": "unknown"}},
		"declared":       M{"model": M{"state": "known", "ref": id("eval-report:model", "local-model")}},
	}
	src["capabilities"] = append(src["capabilities"].([]any), "correlation.task_family_id")
	for _, o := range obj(d, "payload")["observations"].([]any) {
		if corr, ok := o.(map[string]any)["correlation"].(map[string]any); ok && corr["task_id"] != nil {
			corr["task_family_id"] = id("eval-report:family", "billing-refactor")
		}
	}
	return reseal(d)
}

// contextPolicy is the v0.2 export policy: the v0.1 fields plus the context measurement and the
// forecast basis of a plan.
func contextPolicy(pol M) M {
	p := clone(pol)
	p["hint_version"] = "0.2.0"
	f := obj(p, "payload", "fields")
	f["Observation"] = append(f["Observation"].([]any), "context", "collection", "request_kind", "model_ledger")
	if plan, ok := f["Plan"].([]any); ok {
		f["Plan"] = append(plan, "forecast_basis")
	}
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
	// how to read the split plan's forecast: tokens over 120 future tasks, the cost estimated
	// from observed comparable refactoring work, the behavior transport stated
	obj(d, "payload", "plans", 0)["forecast_basis"] = M{"objective": M{"metric": "model_tokens", "unit": "tokens"}, "horizon_tasks": 120,
		"threshold": 0.9, "threshold_basis": "heuristic_default", "p_net_gain_positive": 0.93,
		"net_gain_interval": M{"lower": 50000, "upper": 200000, "level": 0.9}, "cost_basis": "observed_comparable_work",
		"evidence": "project_history", "conditional_on": A{"behavior_transport"}}
	// the refused context plan: no one-time cost, so no net value
	obj(d, "payload", "plans", 1)["forecast_basis"] = M{"objective": M{"metric": "model_tokens", "unit": "tokens"}, "horizon_tasks": 50,
		"threshold": 0.8, "threshold_basis": "configured_policy", "p_net_gain_positive": nil, "net_gain_interval": nil, "cost_basis": nil,
		"evidence": "project_history", "conditional_on": A{"behavior_transport"}}
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
		"effort_delta": M{"observed": -120.5, "interval": M{"lower": -400, "upper": 300}, "unit": "tokens_per_attempt", "coverage": A{6, 6},
			"counter_semantics": "harbor-0.23.0+opencode:n-tokens-root-v1", "scope": "root_session"},
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

func writeV02(root string, act, pol, rec, changes M) (ctx, findings, declared M) {
	ex := func(n string) string { return filepath.Join(root, "examples", "v0.2", n) }
	ctx = contextDoc(act)
	findings = findingsDoc(rec, act, ctx)
	declared = declaredChangeSet(changes, findings)
	write(ex("activity-context.json"), ctx)
	write(ex("export-policy.json"), contextPolicy(pol))
	write(ex("recommendation-findings.json"), findings)
	write(ex("change-set-declared.json"), declared)
	write(ex("activity-model-call.json"), modelCallDoc(act))
	write(ex("activity-model-ledger.json"), ledgerDoc(act))
	write(ex("activity-model-ledger-entries.json"), ledgerEntriesDoc(act))
	write(ex("activity-model-ledger-agent-run.json"), ledgerAgentRunDoc(act))
	write(filepath.Join(root, "tests", "conformance", "valid", "model-ledger-agent-run-aggregate.json"), ledgerAgentRunAggregateDoc(act))
	write(filepath.Join(root, "tests", "conformance", "valid", "model-ledger-entries-revision.json"), ledgerEntriesRevisionDoc(act))
	write(filepath.Join(root, "tests", "conformance", "vectors", "ledger-selection.json"), M{"vectors": ledgerSelectionVectors(ledgerDoc(act))})
	// a positive control for the effort rule: an explicitly synthetic complete whole-task choice
	control := clone(findings)
	wholeTaskChoice(control)
	write(filepath.Join(root, "tests", "conformance", "valid", "decision-whole-task-synthetic.json"), reseal(control))
	// effect bases: a model projection that keeps its legitimate conditions, and two clearly
	// synthetic observed comparisons, one with a scenario cost and one with an unknown cost
	model := clone(findings)
	modelProjection(model)
	write(filepath.Join(root, "tests", "conformance", "valid", "effect-basis-model.json"), reseal(model))
	scenario := clone(findings)
	observedScenario(scenario)
	write(filepath.Join(root, "tests", "conformance", "valid", "effect-basis-observed-synthetic.json"), reseal(scenario))
	unknown := clone(findings)
	observedOn(unknown, 1)
	write(filepath.Join(root, "tests", "conformance", "valid", "effect-basis-observed-unknown-cost.json"), reseal(unknown))
	// descriptive provider totals of isolated runs: a valid comparison, never a decision
	totals := clone(findings)
	providerTotals(obj(totals, "payload", "project_comparisons", 0, "effort_delta"))
	write(filepath.Join(root, "tests", "conformance", "valid", "effort-provider-totals-descriptive.json"), reseal(totals))
	return ctx, findings, declared
}

// modelProjection labels the refused plan's forecast as a model projection that stays
// conditional on unmodeled coordination (out of distribution, never recommended).
func modelProjection(d M) M {
	p := obj(d, "payload", "plans", 1)
	p["simulation"].(map[string]any)["out_of_distribution"] = true
	fb := p["forecast_basis"].(map[string]any)
	fb["conditional_on"] = A{"behavior_transport", "coordination_unmodeled"}
	fb["effect_basis"] = M{"source": "model_projection", "estimand": "model_task_mix_mean_delta", "uncertainty": "model_conditional_interval"}
	return fb
}

// observedOn gives plan i's forecast an explicitly synthetic observed controlled comparison:
// the mean delta over three objective clusters with the uncertainty of that mean, kept apart
// from the plan's model_tokens prediction.
func observedOn(d M, i int) M {
	fb := obj(d, "payload", "plans", i, "forecast_basis")
	fb["evidence"] = "synthetic_fixture"
	fb["effect_basis"] = M{"source": "observed_objective_clustered", "estimand": "controlled_task_token_delta", "uncertainty": "uncertainty_of_mean_delta",
		"effect":     M{"point": -1200.0, "interval": M{"lower": -1800, "upper": -600, "level": 0.9}, "unit": "tokens", "per": "task"},
		"objectives": 3, "record_refs": A{id("record", "controlled-comparison")}, "population_ref": id("population", "controlled-comparison"),
		"evidence_policy": "controlled-effect-v1"}
	return fb
}

// retarget moves plan i's forecast with an observed basis consistently to another objective:
// the objective, the observed summary unit and the matching per-task model prediction.
func retarget(d M, i int, metric, unit string) {
	fb := observedOn(d, i)
	fb["objective"] = M{"metric": metric, "unit": unit}
	fb["effect_basis"].(M)["effect"].(M)["unit"] = unit
	for _, p := range obj(d, "payload", "plans", i)["predictions"].([]any) {
		pm := p.(map[string]any)
		if pm["metric"] == "model_tokens" {
			pm["metric"], pm["unit"] = metric, unit
		}
	}
}

// observedScenario is the split plan with a synthetic observed comparison and an assumed
// (scenario) one-time cost: numeric economics, refused, never recommended.
func observedScenario(d M) M {
	p := obj(d, "payload", "plans", 0)
	p["decision"], p["steps"] = "refuse_uncertain", A{} // a refused plan has no steps
	fb := observedOn(d, 0)
	fb["cost_basis"] = "assumption_scenario"
	return fb
}

// declaredChangeSet is the change set of the split, declared by the client as the application
// of the first plan of the findings set (an association only, not a confirmed realization).
// It is a new document: the plain change set keeps its own ID and digest.
func declaredChangeSet(changes, findings M) M {
	d := clone(changes)
	d["hint_version"] = "0.2.0"
	d["document_id"] = id("document", "change-set", "declared")
	obj(d, "payload")["plan_application"] = M{
		"recommendation_set": M{"document_id": findings["document_id"], "content_digest": findings["content_digest"]},
		"plan_id":            obj(findings, "payload", "plans", 0)["plan_id"],
		"declaration":        "client_declared_association",
		"work":               M{"unit_ref": id("eval-report:work-unit", "split-ledger"), "revision": 3, "attempt_ref": id("eval-report:attempt", "split-ledger", "2")},
	}
	return reseal(d)
}

// wholeTaskChoice turns finding 0's decision into an explicitly synthetic, complete whole-task
// CONTEXT_ONLY choice (canonical labels, numeric observed and interval, full coverage of the
// paired objectives, success evidence); the negative cases then remove one piece of evidence.
// providerTotals relabels an effort delta as the provider-reported totals of isolated runs.
func providerTotals(e M) M {
	e["counter_semantics"], e["scope"] = "openai-compat-provider-totals-v1", "full_task"
	return e
}

func wholeTaskChoice(d M) M {
	dec := obj(d, "payload", "findings", 0, "decision")
	dec["chosen"], dec["evidence"] = "CONTEXT_ONLY", "bound_eval_pairs"
	a := obj(d, "payload", "findings", 0, "decision", "alternatives", 0)
	a["state"] = "chosen"
	a["scope"] = M{"pair_id": id("pair", "whole-task"), "arm": "context_only", "partition": "held_out", "paired_objectives": 3, "task_families": 3}
	a["effort_delta"] = M{"observed": -50.0, "interval": M{"lower": -80, "upper": -20}, "unit": "tokens_per_attempt", "coverage": A{3, 3},
		"counter_semantics": "hint-task-tokens-v1", "scope": "full_task"}
	a["success_delta"] = M{"observed": 0.1, "interval": M{"lower": 0, "upper": 0.2}, "unit": "success_fraction", "coverage": A{3, 3}}
	return a
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
	cfg := func(d M) map[string]any {
		return d["sources"].([]any)[0].(map[string]any)["configuration"].(map[string]any)
	}
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
		{"effort-labels-mismatched", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			obj(d, "payload", "project_comparisons", 0, "effort_delta")["scope"] = "full_task"
		}, nil},
		{"effort-unknown-semantics-full-task", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			e := obj(d, "payload", "project_comparisons", 0, "effort_delta")
			delete(e, "counter_semantics")
			e["scope"] = "full_task"
		}, nil},
		{"decision-chosen-root-scope", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			dec := obj(d, "payload", "findings", 0, "decision")
			dec["chosen"], dec["evidence"] = "CONTEXT_ONLY", "bound_eval_pairs"
			a := obj(d, "payload", "findings", 0, "decision", "alternatives", 0)
			a["state"] = "chosen"
			a["effort_delta"] = obj(d, "payload", "project_comparisons", 0, "effort_delta")
		}, nil},
		{"decision-chosen-without-effort", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			obj(d, "payload", "findings", 0, "decision")["chosen"] = "CONTEXT_ONLY"
			obj(d, "payload", "findings", 0, "decision", "alternatives", 0)["state"] = "chosen"
		}, nil},
		{"decision-known-unlabeled-effort", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			a := obj(d, "payload", "findings", 0, "decision", "alternatives", 0)
			a["state"] = "known"
			a["effort_delta"] = M{"observed": -50.0, "interval": M{"lower": -80, "upper": -20}, "unit": "tokens_per_attempt", "coverage": A{3, 3}}
		}, nil},
		{"decision-chosen-state-conflict", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			a := obj(d, "payload", "findings", 0, "decision", "alternatives", 0)
			a["state"] = "chosen"
			a["effort_delta"] = M{"observed": -50.0, "interval": M{"lower": -80, "upper": -20}, "unit": "tokens_per_attempt", "coverage": A{3, 3},
				"counter_semantics": "hint-task-tokens-v1", "scope": "full_task"}
		}, nil},
		{"decision-no-change-partial", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			obj(d, "payload", "findings", 0, "decision")["chosen"] = "NO_CHANGE"
			obj(d, "payload", "findings", 0, "decision", "alternatives", 0)["state"] = "partial_coverage"
		}, nil},
		{"decision-whole-task-null-observed", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			e := wholeTaskChoice(d)["effort_delta"].(M)
			e["observed"], e["interval"], e["coverage"] = nil, nil, A{0, 3}
		}, nil},
		{"decision-whole-task-zero-coverage", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			wholeTaskChoice(d)["effort_delta"].(M)["coverage"] = A{0, 0}
		}, nil},
		{"decision-whole-task-partial-coverage", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			wholeTaskChoice(d)["effort_delta"].(M)["coverage"] = A{1, 3}
		}, nil},
		{"decision-whole-task-coverage-mismatch", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			wholeTaskChoice(d)["effort_delta"].(M)["coverage"] = A{2, 2} // the pair has 3 objectives
		}, nil},
		{"decision-whole-task-no-success", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			wholeTaskChoice(d)["success_delta"] = nil
		}, nil},
		{"effort-provider-totals-root-scope", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			providerTotals(obj(d, "payload", "project_comparisons", 0, "effort_delta"))["scope"] = "root_session"
		}, nil},
		{"decision-whole-task-provider-totals", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			providerTotals(wholeTaskChoice(d)["effort_delta"].(M)) // the same complete numbers, provider totals: never a decision
		}, nil},
		{"decision-known-provider-totals", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			a := wholeTaskChoice(d)
			a["state"] = "known"
			obj(d, "payload", "findings", 0, "decision")["chosen"] = "NO_CHANGE"
			providerTotals(a["effort_delta"].(M))
		}, nil},
		{"decision-cost-unknown-provider-totals", "recommendation-findings.json", "semantic", "effort_scope_inconsistent", func(d M) {
			a := wholeTaskChoice(d)
			a["state"] = "cost_unknown"
			obj(d, "payload", "findings", 0, "decision")["chosen"] = "COLLECT_MORE_EVIDENCE"
			providerTotals(a["effort_delta"].(M))
		}, nil},
		{"effort-scope-free-text", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "project_comparisons", 0, "effort_delta")["scope"] = "whole repo"
		}, nil},
		{"feedback-automatic-change", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "context_feedback", 0)["proposal"] = "apply_revision"
		}, nil},
		{"feedback-free-text", "recommendation-findings.json", "schema", "content", func(d M) {
			obj(d, "payload", "context_feedback", 0)["note"] = forbidden["prompt"]
		}, nil},
		{"forecast-without-horizon", "recommendation-findings.json", "schema", "finding", func(d M) {
			delete(obj(d, "payload", "plans", 0, "forecast_basis"), "horizon_tasks")
		}, nil},
		{"forecast-probability-out-of-range", "recommendation-findings.json", "schema", "finding", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["p_net_gain_positive"] = 1.5
		}, nil},
		{"forecast-net-without-level", "recommendation-findings.json", "schema", "finding", func(d M) {
			delete(obj(d, "payload", "plans", 0, "forecast_basis", "net_gain_interval"), "level")
		}, nil},
		{"finding-in-v01-document", "recommendation-findings.json", "schema", "version", func(d M) {
			d["hint_version"] = "0.1.0"
		}, nil},
		{"forecast-net-interval-order", "recommendation-findings.json", "semantic", "interval_order", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["net_gain_interval"] = M{"lower": 200000, "upper": 50000, "level": 0.9}
		}, nil},
		{"forecast-mixed-units", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis", "objective")["unit"] = "seconds"
		}, nil},
		{"forecast-unknown-cost-numeric-net", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["cost_basis"] = nil
		}, nil},
		{"forecast-scenario-recommended", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["cost_basis"] = "assumption_scenario"
		}, nil},
		{"forecast-synthetic-recommended", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["evidence"] = "synthetic_fixture"
		}, nil},
		{"forecast-inconsistent-ood", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["conditional_on"] = A{"behavior_transport", "unseen_task_mix"}
		}, nil},
		{"forecast-custom-threshold-as-default", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["threshold"] = 0.8
		}, nil},
		{"forecast-hidden-behavior-transport", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["conditional_on"] = A{}
		}, nil},
		{"forecast-recommend-out-of-distribution", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "simulation")["out_of_distribution"] = true
		}, nil},
		{"forecast-recommend-uncalibrated", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "simulation")["calibration_status"] = "uncalibrated"
		}, nil},
		{"forecast-recommend-calibration-omitted", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			delete(obj(d, "payload", "plans", 0, "simulation"), "calibration_status")
		}, nil},
		{"forecast-recommend-ood-omitted", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			delete(obj(d, "payload", "plans", 0, "simulation"), "out_of_distribution")
		}, nil},
		{"forecast-recommend-cost-unknown", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			p := obj(d, "payload", "plans", 0)
			delete(p, "refactoring_cost_estimate")
			delete(p, "break_even_horizon")
			fb := p["forecast_basis"].(map[string]any)
			fb["cost_basis"], fb["p_net_gain_positive"], fb["net_gain_interval"] = nil, nil, nil
		}, nil},
		{"forecast-recommend-below-threshold", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "forecast_basis")["p_net_gain_positive"] = 0.01
		}, nil},
		{"forecast-recommend-l2", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			obj(d, "payload", "plans", 0, "simulation")["level"] = "L2"
		}, nil},
		{"effect-basis-tuple-mismatch", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["estimand"] = "model_task_mix_mean_delta"
		}, nil},
		{"effect-basis-model-with-observed-summary", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			e := observedOn(d, 1)["effect_basis"].(M)
			e["source"], e["estimand"], e["uncertainty"] = "model_projection", "model_task_mix_mean_delta", "model_conditional_interval"
		}, nil},
		{"effect-basis-observed-without-summary", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			delete(observedOn(d, 1)["effect_basis"].(M), "effect")
		}, nil},
		{"effect-basis-observed-without-records", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			delete(observedOn(d, 1)["effect_basis"].(M), "record_refs")
		}, nil},
		{"effect-basis-observed-without-population", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			delete(observedOn(d, 1)["effect_basis"].(M), "population_ref")
		}, nil},
		{"effect-basis-observed-without-policy", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			delete(observedOn(d, 1)["effect_basis"].(M), "evidence_policy")
		}, nil},
		{"effect-basis-observed-without-objectives", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			delete(observedOn(d, 1)["effect_basis"].(M), "objectives")
		}, nil},
		{"effect-basis-unit-mismatch", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["effect"].(M)["unit"] = "seconds"
		}, nil},
		{"effect-basis-seconds-objective", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			// the whole forecast moved to seconds: objective, observed summary and model prediction
			retarget(d, 1, "task_elapsed_seconds", "seconds")
		}, nil},
		{"effect-basis-input-only-objective", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			retarget(d, 1, "input_tokens", "tokens") // input-only usage is not the model_tokens estimand
		}, nil},
		{"effect-basis-model-seconds-objective", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			modelProjection(d)
			fb := obj(d, "payload", "plans", 1, "forecast_basis")
			fb["objective"] = M{"metric": "task_elapsed_seconds", "unit": "seconds"}
			obj(d, "payload", "plans", 1, "predictions", 0)["metric"], obj(d, "payload", "plans", 1, "predictions", 0)["unit"] = "task_elapsed_seconds", "seconds"
		}, nil},
		{"effect-basis-point-outside-interval", "recommendation-findings.json", "semantic", "effect_basis_inconsistent", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["effect"].(M)["point"] = -2500.0
		}, nil},
		{"effect-basis-interval-order", "recommendation-findings.json", "semantic", "interval_order", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["effect"].(M)["interval"] = M{"lower": -600, "upper": -1800, "level": 0.9}
		}, nil},
		{"effect-basis-unknown-cost-numeric-net", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			observedOn(d, 1)["p_net_gain_positive"] = 0.5
		}, nil},
		{"effect-basis-synthetic-recommended", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			observedOn(d, 0) // the split plan stays recommend
		}, nil},
		{"effect-basis-scenario-recommended", "recommendation-findings.json", "semantic", "forecast_inconsistent", func(d M) {
			observedScenario(d)
			obj(d, "payload", "plans", 0)["decision"] = "recommend"
		}, nil},
		{"effect-basis-zero-objectives", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["objectives"] = 0
		}, nil},
		{"effect-basis-duplicate-records", "recommendation-findings.json", "schema", "finding", func(d M) {
			e := observedOn(d, 1)["effect_basis"].(M)
			e["record_refs"] = A{e["record_refs"].(A)[0], e["record_refs"].(A)[0]}
		}, nil},
		{"effect-basis-raw-record-name", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["record_refs"] = A{"controlled comparison of billing"}
		}, nil},
		{"effect-basis-private-policy-version", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["evidence_policy"] = "effect-0.1.0+selection-0.1.0"
		}, nil},
		{"effect-basis-per-attempt", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["effect"].(M)["per"] = "attempt"
		}, nil},
		{"effect-basis-single-task-interval", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["uncertainty"] = "predictive_interval"
		}, nil},
		{"effect-basis-unknown-field", "recommendation-findings.json", "schema", "finding", func(d M) {
			observedOn(d, 1)["effect_basis"].(M)["eligible"] = true
		}, nil},
		{"configuration-model-known-without-ref", "activity-context.json", "schema", "source", func(d M) {
			delete(cfg(d)["model"].(map[string]any), "ref")
		}, nil},
		{"configuration-policy-enabled-without-ref", "activity-context.json", "schema", "source", func(d M) {
			delete(cfg(d)["context_policy"].(map[string]any), "ref")
		}, nil},
		{"configuration-policy-disabled-with-ref", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["context_policy"].(map[string]any)["state"] = "disabled"
		}, nil},
		{"configuration-unknown-refs-with-list", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["config_refs"] = M{"state": "unknown", "refs": A{}}
		}, nil},
		{"configuration-unknown-version-with-value", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["harness"] = M{"kind": "direct", "version": M{"state": "unknown", "value": "1.0.0"}}
		}, nil},
		{"configuration-raw-model-text", "activity-context.json", "schema", "content", func(d M) {
			cfg(d)["model"] = M{"state": "known", "ref": forbidden["identifier"]}
		}, nil},
		{"configuration-model-without-provenance", "activity-context.json", "schema", "source", func(d M) {
			delete(cfg(d)["model"].(map[string]any), "provenance")
		}, nil},
		{"configuration-declared-as-effective", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["model"].(map[string]any)["provenance"] = "declared"
		}, nil},
		{"configuration-without-budget", "activity-context.json", "schema", "source", func(d M) {
			delete(cfg(d), "budget")
		}, nil},
		{"configuration-null-limit", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["budget"] = M{"max_requests": nil, "max_seconds": M{"state": "unknown"}}
		}, nil},
		{"configuration-known-limit-without-value", "activity-context.json", "schema", "source", func(d M) {
			cfg(d)["budget"] = M{"max_requests": M{"state": "known"}, "max_seconds": M{"state": "unknown"}}
		}, nil},
		{"task-family-without-task", "activity-context.json", "schema", "observation", func(d M) {
			corr := o0(d)["correlation"].(map[string]any)
			delete(corr, "task_id")
		}, nil},
		{"task-family-conflict", "activity-context.json", "semantic", "task_family_conflict", func(d M) {
			o0(d)["correlation"].(map[string]any)["task_family_id"] = id("eval-report:family", "other")
		}, nil},
		{"plan-application-work-without-attempt", "change-set-declared.json", "schema", "change", func(d M) {
			delete(obj(d, "payload", "plan_application", "work"), "attempt_ref")
		}, nil},
		{"plan-application-in-v01-document", "change-set-declared.json", "schema", "version", func(d M) {
			d["hint_version"] = "0.1.0"
		}, nil},
		{"plan-application-confirmed-claim", "change-set-declared.json", "schema", "change", func(d M) {
			obj(d, "payload", "plan_application")["declaration"] = "confirmed_realization"
		}, nil},
		{"plan-application-free-text", "change-set-declared.json", "schema", "content", func(d M) {
			obj(d, "payload", "plan_application")["note"] = forbidden["prompt"]
		}, nil},
		{"plan-application-without-digest", "change-set-declared.json", "schema", "change", func(d M) {
			delete(obj(d, "payload", "plan_application", "recommendation_set"), "content_digest")
		}, nil},
		{"plan-application-group-of-one", "change-set-declared.json", "schema", "change", func(d M) {
			obj(d, "payload", "plan_application")["group"] = M{"group_id": id("group", "g1"), "repo_ids": A{obj(d, "payload")["repo_id"]}}
		}, nil},
		{"plan-application-group-without-repo", "change-set-declared.json", "semantic", "plan_application_inconsistent", func(d M) {
			obj(d, "payload", "plan_application")["group"] = M{"group_id": id("group", "g1"), "repo_ids": A{id("repo", "x"), id("repo", "y")}}
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
