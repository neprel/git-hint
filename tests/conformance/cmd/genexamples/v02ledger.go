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
		{"ledger-revision-heals-incompleteness", base, "semantic", "ledger_inconsistent", func(d M) {
			// revision 0 is incomplete with a late request; revision 1 claims complete with no late request
			m := l(d)
			m["state"], m["reasons"] = "incomplete", A{"late_requests"}
			counts(d)["late"] = 1
			second(d, func(r M) {
				r["observation_id"] = id("ledger-usage", "healed")
				x := r["model_ledger"].(map[string]any)
				x["revision"], x["state"], x["reasons"] = 1, "complete", A{}
				x["counts"].(map[string]any)["late"] = 0
			})
		}, nil},
		{"ledger-revision-rewrites-components", base, "semantic", "ledger_inconsistent", func(d M) {
			second(d, func(r M) {
				r["observation_id"] = id("ledger-usage", "rewritten")
				x := r["model_ledger"].(map[string]any)
				x["revision"], x["state"], x["reasons"] = 1, "incomplete", A{"late_requests"}
				x["counts"].(map[string]any)["late"] = 1
				x["components"].(map[string]any)["prompt"] = 106091
			})
		}, nil},
		{"ledger-complete-with-teardown-reason", base, "semantic", "ledger_inconsistent", func(d M) {
			l(d)["reasons"] = A{"teardown_incomplete"}
		}, nil},
		{"ledger-revision-drops-teardown-reason", base, "semantic", "ledger_inconsistent", func(d M) {
			m := l(d)
			m["state"], m["reasons"] = "incomplete", A{"teardown_incomplete"}
			second(d, func(r M) {
				r["observation_id"] = id("ledger-usage", "teardown-dropped")
				x := r["model_ledger"].(map[string]any)
				x["revision"], x["state"], x["reasons"] = 1, "incomplete", A{"late_requests"}
				x["counts"].(map[string]any)["late"] = 1
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

// ledgerSelectionVectors are the shared usage-selection cases of one task with a model ledger
// (07B-G §13): the sources and observations of the task and the declared coverage reason. The
// client (activity.UsageCoverage) and the server (metrics.usage_selection, with the bound work
// attempt received) must both give the reason. Without a reviewed provider mapping a bound,
// consistent, complete ledger still gives no canonical usage.
func ledgerSelectionVectors(base M) []any {
	d := clone(base)
	srcs := d["sources"].([]any)
	plugin := srcs[0].(map[string]any)
	ledgerSrc := srcs[len(srcs)-1].(map[string]any)
	plugin["configuration"] = clone(ledgerSrc["configuration"].(map[string]any))
	obs := d["payload"].(map[string]any)["observations"].([]any)
	led := obs[len(obs)-1].(map[string]any)
	corr := led["correlation"].(map[string]any)
	var agents []any
	for _, o := range obs[:len(obs)-1] {
		m := clone(o.(map[string]any))
		c := m["correlation"].(map[string]any)
		c["task_id"], c["task_family_id"], c["run_ref"] = corr["task_id"], corr["task_family_id"], corr["run_ref"]
		agents = append(agents, m)
	}
	rev := func(r M, n int, late int, sealed string) M {
		x := clone(r)
		x["observation_id"] = id("ledger-usage", "vector", sealed, string(rune('0'+n)))
		m := x["model_ledger"].(map[string]any)
		m["revision"], m["sealed_at"] = n, sealed
		if late > 0 {
			m["state"], m["reasons"] = "incomplete", A{"late_requests"}
			m["counts"].(map[string]any)["late"] = late
		}
		return x
	}
	sealed := led["model_ledger"].(map[string]any)["sealed_at"].(string)
	// provider_totals (07B-J §25 A): the descriptive totals only when every gate passed; the
	// prompt total keeps its cached part (no non-cached input is derived)
	vector := func(name, reason string, sources A, records ...any) M {
		var totals any
		if reason == "usage_normalization_unmapped" {
			c := led["model_ledger"].(map[string]any)["components"].(map[string]any)
			totals = M{"counter_semantics": "openai-compat-provider-totals-v1", "scope": "full_task", "input_tokens": c["prompt"], "output_tokens": c["completion"]}
		}
		return M{"name": name, "reason": reason, "provider_totals": totals, "sources": sources, "observations": A(records)}
	}
	both := A{plugin, ledgerSrc}
	withAgents := func(extra ...any) A { return append(append(A{}, agents...), extra...) }
	otherRun := clone(agents[0].(map[string]any))
	otherRun["correlation"].(map[string]any)["run_ref"] = id("eval-report:trial", "trial-2")
	noRun := clone(agents[0].(map[string]any))
	delete(noRun["correlation"].(map[string]any), "run_ref")
	otherFamily := clone(agents[0].(map[string]any))
	otherFamily["correlation"].(map[string]any)["task_family_id"] = id("eval-report:family", "feature")
	secondLedger := clone(led)
	secondLedger["observation_id"] = id("ledger-usage", "second")
	secondLedger["model_ledger"].(map[string]any)["ledger_ref"] = id("ledger", "second")
	conflicting := clone(led)
	conflicting["observation_id"] = id("ledger-usage", "conflicting")
	conflicting["model_ledger"].(map[string]any)["components"].(map[string]any)["prompt"] = 6092
	inconsistent := clone(led)
	inconsistent["model_ledger"].(map[string]any)["counts"].(map[string]any)["admitted"] = 3
	otherCfgPlugin := clone(plugin)
	otherCfgPlugin["configuration"].(map[string]any)["budget"].(map[string]any)["max_requests"] = M{"state": "known", "value": 32}
	otherCfgLedger := clone(ledgerSrc)
	otherCfgLedger["configuration"].(map[string]any)["budget"].(map[string]any)["max_requests"] = M{"state": "known", "value": 32}
	lateFirst := rev(led, 0, 1, sealed)
	lateFirst["observation_id"] = led["observation_id"]
	healed := rev(led, 1, 0, sealed)
	rewritten := rev(led, 1, 1, sealed)
	rewritten["model_ledger"].(map[string]any)["components"].(map[string]any)["prompt"] = 106091
	lessLate := rev(lateFirst, 1, 1, sealed)
	lessLate["model_ledger"].(map[string]any)["counts"].(map[string]any)["late"] = 0
	lessLate["model_ledger"].(map[string]any)["reasons"] = A{"late_requests"}
	// 07B-J §23: a ledger whose attempt teardown did not complete is incomplete with every
	// count in place, and no later revision drops the reason
	teardown := clone(led)
	teardown["model_ledger"].(map[string]any)["state"], teardown["model_ledger"].(map[string]any)["reasons"] = "incomplete", A{"teardown_incomplete"}
	teardownDropped := rev(teardown, 1, 1, sealed)
	teardownDropped["model_ledger"].(map[string]any)["reasons"] = A{"late_requests"}
	dropped := rev(lateFirst, 1, 2, sealed)
	dropped["model_ledger"].(map[string]any)["reasons"] = A{"rejected_limit"}
	dropped["model_ledger"].(map[string]any)["counts"].(map[string]any)["rejected_limit"] = 1
	return []any{
		vector("bound complete ledger", "usage_normalization_unmapped", both, withAgents(led)...),
		vector("agent record of another run", "ledger_other_run", both, withAgents(led, otherRun)...),
		vector("agent record without a run", "ledger_other_run", both, withAgents(led, noRun)...),
		vector("agent record of another family", "ledger_binding_mismatch", both, withAgents(led, otherFamily)...),
		vector("two ledgers", "ledger_ambiguous", both, withAgents(led, secondLedger)...),
		vector("revision gap", "ledger_revision_gap", both, withAgents(rev(led, 1, 1, sealed))...),
		vector("two bodies of revision 0", "ledger_conflict", both, withAgents(led, conflicting)...),
		vector("late revision", "ledger_incomplete", both, withAgents(led, rev(led, 1, 1, sealed))...),
		vector("late revision with a new seal time", "ledger_binding_changed", both, withAgents(led, rev(led, 1, 1, "2026-10-11T11:00:06Z"))...),
		vector("inconsistent counts", "ledger_inconsistent", both, withAgents(inconsistent)...),
		vector("teardown not complete", "ledger_incomplete", both, withAgents(teardown)...),
		vector("teardown reason dropped by a revision", "ledger_lifecycle_violation", both, withAgents(teardown, teardownDropped)...),
		vector("ledger source with another configuration", "ledger_binding_mismatch", A{plugin, otherCfgLedger}, withAgents(led)...),
		vector("plugin source with another configuration", "ledger_configuration_mismatch", A{otherCfgPlugin, ledgerSrc}, withAgents(led)...),
		vector("ledger without behavioral records", "ledger_without_behavior", both, led),
		vector("later revision heals an incomplete ledger", "ledger_lifecycle_violation", both, withAgents(lateFirst, healed)...),
		vector("later revision rewrites a sealed component", "ledger_lifecycle_violation", both, withAgents(led, rewritten)...),
		vector("later revision lowers the late count", "ledger_lifecycle_violation", both, withAgents(lateFirst, lessLate)...),
		vector("later revision drops a reason", "ledger_lifecycle_violation", both, withAgents(lateFirst, dropped)...),
	}
}

// ledgerEntriesDoc is the ledger example with per-entry evidence (draft v0.2): four admitted
// requests that ended in four ways (a complete response, an upstream error status, a request
// whose context ended with partial usage kept, a request still open at the drain deadline), the
// aggregates derived from them, and the timing anchor with a 10 ms observed-minus-derived clock
// step. The aggregate-only example stays as it is.
func ledgerEntriesDoc(act M) M {
	d := ledgerDoc(act)
	d["document_id"] = id("document", "activity", "model-ledger-entries")
	l := obj(d, "payload", "observations", 6, "model_ledger")
	ledger := l["ledger_ref"].(string)
	n := func(v int) any { return v }
	tru, fal := true, false
	entry := func(ord int, state, term string, started bool, done any, class string, adm, fin int, p, c, k any) M {
		return M{"ordinal": ord, "entry_id": id("ledger-entry", ledger, string(rune('0'+ord))), "state": state, "termination": term,
			"response_started": started, "stream_done": done, "status_class": class, "model_check": "requested_model_observed",
			"admitted_ms": adm, "finished_ms": fin, "components": M{"prompt": p, "completion": c, "cached": k, "reasoning": nil}}
	}
	l["entries"] = A{
		entry(1, "complete", "response_ended", true, tru, "2xx", 1000, 40000, n(3000), n(300), n(2400)),
		entry(2, "error", "error_status", true, nil, "5xx", 41000, 41500, nil, nil, nil),
		entry(3, "cancelled", "context_lost", true, fal, "2xx", 42000, 90000, n(3091), nil, n(2400)),
		entry(4, "unknown", "seal_timeout", true, fal, "2xx", 300000, 309990, nil, nil, nil),
	}
	l["anchor_utc"], l["sealed_ms"], l["finalized_ms"], l["clock_step_ms"] = "2026-10-11T10:55:00Z", 304990, 309990, 10
	l["state"], l["reasons"] = "incomplete", A{"error", "cancelled", "unknown"}
	l["counts"] = M{"admitted": 4, "late": 0, "rejected_limit": 0, "complete": 1, "usage_partial": 0, "missing_usage": 0, "usage_conflict": 0,
		"unreadable": 0, "truncated": 0, "error": 1, "cancelled": 1, "unknown": 1}
	l["components"] = M{"prompt": 6091, "completion": 300, "cached": 4800, "reasoning": nil}
	l["component_coverage"] = M{"prompt": 2, "completion": 1, "cached": 2, "reasoning": 0}
	return reseal(d)
}

// ledgerEntriesRevisionDoc adds revision 1 of the same ledger: one late request, the entries and
// their timing repeated exactly (a valid revision).
func ledgerEntriesRevisionDoc(act M) M {
	d := ledgerEntriesDoc(act)
	d["document_id"] = id("document", "activity", "model-ledger-entries-revision")
	obs := obj(d, "payload")["observations"].([]any)
	r := plain(obs[6].(map[string]any))
	r["observation_id"] = id("ledger-usage", "entries", "1")
	x := r["model_ledger"].(map[string]any)
	x["revision"], x["reasons"] = 1, A{"error", "cancelled", "unknown", "late_requests"}
	x["counts"].(map[string]any)["late"] = 1
	obj(d, "payload")["observations"] = append(obs, r)
	return reseal(d)
}

// ledgerEntriesInvalid are the negative cases of the per-entry evidence.
func ledgerEntriesInvalid() []invalidCase {
	const base, agg = "activity-model-ledger-entries.json", "activity-model-ledger.json"
	l := func(d M) M { return obj(d, "payload", "observations", 6, "model_ledger") }
	e := func(d M, i int) M { return l(d)["entries"].([]any)[i].(map[string]any) }
	comps := func(d M, i int) M { return e(d, i)["components"].(map[string]any) }
	semantic := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "semantic", "ledger_inconsistent", f, nil}
	}
	schema := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "schema", "observation", f, nil}
	}
	revision := func(d M, change func(x M)) {
		obs := obj(d, "payload")["observations"].([]any)
		r := plain(obs[6].(map[string]any))
		r["observation_id"] = id("ledger-usage", "entries", "1")
		x := r["model_ledger"].(map[string]any)
		x["revision"], x["state"] = 1, "incomplete"
		x["reasons"] = append(x["reasons"].([]any), "late_requests")
		x["counts"].(map[string]any)["late"] = 1
		change(x)
		obj(d, "payload")["observations"] = append(obs, r)
	}
	return []invalidCase{
		schema("entries-unknown-termination", func(d M) { e(d, 0)["termination"] = "completed" }),
		schema("entries-content-field", func(d M) { e(d, 0)["prompt_text"] = "x" }),
		schema("entries-missing-field", func(d M) { delete(e(d, 1), "stream_done") }),
		schema("entries-exact-status-code", func(d M) { e(d, 1)["status_class"] = "502" }),
		schema("entries-negative-time", func(d M) { e(d, 0)["admitted_ms"] = -1 }),
		schema("entries-timing-without-entries", func(d M) { delete(l(d), "entries") }),
		schema("entries-without-timing", func(d M) { delete(l(d), "anchor_utc") }),
		schema("entries-raw-entry-id", func(d M) { e(d, 0)["entry_id"] = "request-1" }),
		semantic("entries-duplicate-ordinal", func(d M) { e(d, 1)["ordinal"] = 1 }),
		semantic("entries-ordinal-gap", func(d M) { e(d, 3)["ordinal"] = 5 }),
		semantic("entries-duplicate-id", func(d M) { e(d, 1)["entry_id"] = e(d, 0)["entry_id"] }),
		semantic("entries-fewer-than-admitted", func(d M) {
			l(d)["entries"] = l(d)["entries"].([]any)[:3]
		}),
		semantic("entries-state-count-mismatch", func(d M) {
			e(d, 2)["state"], e(d, 2)["termination"], e(d, 2)["status_class"] = "error", "error_status", "5xx"
		}),
		semantic("entries-sum-mismatch", func(d M) { comps(d, 0)["prompt"] = 3001 }),
		semantic("entries-partial-error-value-not-in-sum", func(d M) { comps(d, 1)["completion"] = 7 }),
		semantic("entries-coverage-mismatch", func(d M) { comps(d, 2)["cached"] = nil }),
		semantic("entries-termination-state-mismatch", func(d M) { e(d, 0)["termination"] = "context_lost" }),
		semantic("entries-response-ended-not-2xx", func(d M) { e(d, 0)["status_class"] = "4xx" }),
		semantic("entries-no-response-started", func(d M) {
			e(d, 2)["termination"], e(d, 2)["status_class"] = "no_response", "2xx"
		}),
		semantic("entries-truncated-with-done", func(d M) {
			e(d, 0)["state"] = "truncated"
			l(d)["counts"].(map[string]any)["complete"], l(d)["counts"].(map[string]any)["truncated"] = 0, 1
		}),
		semantic("entries-complete-without-completion", func(d M) {
			comps(d, 0)["completion"] = nil
			l(d)["components"].(map[string]any)["completion"] = nil
			l(d)["component_coverage"].(map[string]any)["completion"] = 0
		}),
		semantic("entries-admitted-after-seal", func(d M) { e(d, 3)["admitted_ms"] = 305000 }),
		semantic("entries-admission-out-of-order", func(d M) { e(d, 1)["admitted_ms"] = 500 }),
		semantic("entries-finish-after-drain", func(d M) { e(d, 2)["finished_ms"] = 400000 }),
		semantic("entries-seal-timeout-before-drain-end", func(d M) { e(d, 3)["finished_ms"] = 305000 }),
		semantic("entries-clock-step-not-observed", func(d M) { l(d)["clock_step_ms"] = 0 }),
		semantic("entries-with-over-limit-reason", func(d M) { l(d)["reasons"] = append(l(d)["reasons"].([]any), "entries_over_limit") }),
		semantic("entries-model-check-not-derived", func(d M) { e(d, 0)["model_check"] = "mismatch" }),
		semantic("entries-revision-changes-an-entry", func(d M) {
			revision(d, func(x M) { x["entries"].([]any)[0].(map[string]any)["finished_ms"] = 40001 })
		}),
		semantic("entries-revision-removes-entries", func(d M) {
			revision(d, func(x M) {
				for _, k := range append([]string{"entries"}, "anchor_utc", "sealed_ms", "finalized_ms", "clock_step_ms") {
					delete(x, k)
				}
			})
		}),
		semantic("entries-revision-rewrites-timing", func(d M) { revision(d, func(x M) { x["anchor_utc"] = "2026-10-11T10:54:59Z" }) }),
		{"entries-revision-enriches-aggregate", agg, "semantic", "ledger_inconsistent", func(d M) {
			// revision 1 of the aggregate-only ledger adds entries: refused (no later enrichment)
			obs := obj(d, "payload")["observations"].([]any)
			r := plain(obs[6].(map[string]any))
			r["observation_id"] = id("ledger-usage", "enriched")
			x := r["model_ledger"].(map[string]any)
			x["revision"], x["state"], x["reasons"] = 1, "incomplete", A{"late_requests"}
			x["counts"].(map[string]any)["late"] = 1
			x["entries"] = A{}
			x["anchor_utc"], x["sealed_ms"], x["finalized_ms"], x["clock_step_ms"] = "2026-10-11T11:00:05Z", 0, 0, 0
			obj(d, "payload")["observations"] = append(obs, r)
		}, nil},
	}
}

// agentRun is a launch/exit enclosure record of the ledger's agent container (draft v0.2).
func agentRun(outcome string, code, start, end any) M {
	return M{"outcome": outcome, "exit_code": code, "enclosure_start_ms": start, "enclosure_end_ms": end,
		"execution_digest":  "sha256:" + id("execution", "opencode", "2.0.26") + id("execution", "argv"),
		"engine_started_at": "2026-10-11T10:55:00.400Z", "engine_finished_at": "2026-10-11T11:00:01Z"}
}

// ledgerAgentRunDoc is the per-entry example plus the agent container's launch/exit enclosure:
// sampled before the start call (500 ms) and at the completed wait (302000 ms), every admission
// inside it, the end before the seal (304990 ms); the agent exited with status 0.
func ledgerAgentRunDoc(act M) M {
	d := ledgerEntriesDoc(act)
	d["document_id"] = id("document", "activity", "model-ledger-agent-run")
	obj(d, "payload", "observations", 6, "model_ledger")["agent_run"] = agentRun("exited", 0, 500, 302000)
	return reseal(d)
}

// ledgerAgentRunAggregateDoc is the aggregate-only example with an agent outcome but no offsets
// (no entries, no timing): the lifecycle record exists, its enclosure is unknown.
func ledgerAgentRunAggregateDoc(act M) M {
	d := ledgerDoc(act)
	d["document_id"] = id("document", "activity", "model-ledger-agent-run-aggregate")
	obj(d, "payload", "observations", 6, "model_ledger")["agent_run"] = agentRun("exited", 0, nil, nil)
	return reseal(d)
}

// ledgerAgentRunInvalid are the negative cases of the launch/exit enclosure.
func ledgerAgentRunInvalid() []invalidCase {
	const base, agg = "activity-model-ledger-agent-run.json", "activity-model-ledger.json"
	l := func(d M) M { return obj(d, "payload", "observations", 6, "model_ledger") }
	a := func(d M) M { return l(d)["agent_run"].(map[string]any) }
	semantic := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "semantic", "ledger_inconsistent", f, nil}
	}
	schema := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "schema", "observation", f, nil}
	}
	revision := func(d M, change func(x M)) {
		obs := obj(d, "payload")["observations"].([]any)
		r := plain(obs[6].(map[string]any))
		r["observation_id"] = id("ledger-usage", "agent-run", "1")
		x := r["model_ledger"].(map[string]any)
		x["revision"], x["state"] = 1, "incomplete"
		x["reasons"] = append(x["reasons"].([]any), "late_requests")
		x["counts"].(map[string]any)["late"] = 1
		change(x)
		obj(d, "payload")["observations"] = append(obs, r)
	}
	set := func(outcome string, code, start, end any) func(d M) {
		return func(d M) {
			x := a(d)
			x["outcome"], x["exit_code"], x["enclosure_start_ms"], x["enclosure_end_ms"] = outcome, code, start, end
		}
	}
	return []invalidCase{
		schema("agent-run-unknown-outcome", func(d M) { a(d)["outcome"] = "finished" }),
		schema("agent-run-extra-field", func(d M) { a(d)["pid"] = 42 }),
		schema("agent-run-bad-digest", func(d M) { a(d)["execution_digest"] = "opencode-2.0.26" }),
		schema("agent-run-missing-field", func(d M) { delete(a(d), "engine_finished_at") }),
		semantic("agent-run-exited-with-nonzero-code", set("exited", 3, 500, 302000)),
		semantic("agent-run-nonzero-with-zero-code", set("exited_nonzero", 0, 500, 302000)),
		semantic("agent-run-stopped-with-exit-code", set("stopped_after_timeout", 137, 500, 302000)),
		semantic("agent-run-exited-without-start", set("exited", 0, nil, 302000)),
		semantic("agent-run-exited-without-end", set("exited", 0, 500, nil)),
		semantic("agent-run-unconfirmed-stop-with-end", set("stop_unconfirmed", nil, 500, 302000)),
		semantic("agent-run-unconfirmed-start-with-end", set("start_unconfirmed", nil, 500, 302000)),
		semantic("agent-run-refused-with-entries", set("refused_before_launch", nil, nil, nil)),
		semantic("agent-run-admission-before-start", set("exited", 0, 1500, 302000)),
		semantic("agent-run-admission-after-end", set("exited", 0, 500, 299999)),
		semantic("agent-run-end-after-seal", set("exited", 0, 500, 305000)),
		semantic("agent-run-revision-changes-it", func(d M) { revision(d, func(x M) { x["agent_run"].(map[string]any)["enclosure_end_ms"] = 302001 }) }),
		semantic("agent-run-revision-removes-it", func(d M) { revision(d, func(x M) { delete(x, "agent_run") }) }),
		{"agent-run-offsets-without-entries", agg, "semantic", "ledger_inconsistent", func(d M) {
			l(d)["agent_run"] = agentRun("exited", 0, 500, 302000)
		}, nil},
		{"agent-run-refused-with-admitted-requests", agg, "semantic", "ledger_inconsistent", func(d M) {
			l(d)["agent_run"] = agentRun("refused_before_launch", nil, nil, nil)
		}, nil},
		{"agent-run-revision-enriches-aggregate", agg, "semantic", "ledger_inconsistent", func(d M) {
			// revision 1 of a ledger without a lifecycle adds one: refused (no later enrichment)
			obs := obj(d, "payload")["observations"].([]any)
			r := plain(obs[6].(map[string]any))
			r["observation_id"] = id("ledger-usage", "agent-run-enriched")
			x := r["model_ledger"].(map[string]any)
			x["revision"], x["state"], x["reasons"] = 1, "incomplete", A{"late_requests"}
			x["counts"].(map[string]any)["late"] = 1
			x["agent_run"] = agentRun("exited", 0, nil, nil)
			obj(d, "payload")["observations"] = append(obs, r)
		}, nil},
	}
}
