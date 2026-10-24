package main

import "github.com/neprel/git-hint/tests/conformance/hintconf"

// orchestratorSource is the project orchestrator's administrative source (draft v0.2).
func orchestratorSource() M {
	return M{"source_id": id("source", "hint-project-orchestrator"), "agent": "other", "agent_version": nil,
		"adapter":      M{"name": "hint-project-orchestrator", "version": "0.1.0"},
		"capabilities": A{"membership"},
		"unavailable": A{"usage", "request_kind", "collection", "context", "coordination", "targets", "correlation.session_id", "correlation.turn_id",
			"correlation.trace_id", "correlation.span_id", "correlation.task_id", "correlation.run_ref"}}
}

// rechain recomputes every prev_digest and the head_digest of a membership epoch in order.
func rechain(mem M) {
	prev := any(nil)
	for _, e := range mem["events"].([]any) {
		ev := e.(map[string]any)
		ev["prev_digest"] = prev
		d, err := hintconf.Digest(ev)
		check(err)
		prev = d
	}
	mem["head_digest"] = prev
}

func digestOf(parts ...string) string {
	d, err := hintconf.Digest(parts)
	check(err)
	return d
}

// membershipEpoch is one sealed epoch with two executions: one launched and exited, one refused
// before its launch.
func membershipEpoch(unit string) M {
	e1, e2 := id("execution", unit, "run-1"), id("execution", unit, "run-2")
	cfg := digestOf("configuration")
	events := A{
		M{"seq": 1, "kind": "epoch_open", "epoch": 1, "original_scope_digest": digestOf("scope"), "enrollment_start_revision_digest": digestOf("open-1"),
			"policy_digest": digestOf("policy"), "coverage": "hint_orchestrated_executions", "predecessor_epoch_digest": nil},
		M{"seq": 2, "kind": "enroll", "execution_ref": e1, "run_ref": id("run", "run-1"), "attempt_ref": id("attempt", "a1"), "task_id": id("task", "t1"),
			"planned_configuration_digest": cfg},
		M{"seq": 3, "kind": "launch", "execution_ref": e1, "launch_digest": digestOf("launch"), "execution_digest": digestOf("execution"), "installed_target": "linux-arm64",
			"model_ref": id("model", "m"), "gate_policy_digest": digestOf("gate"), "source_configuration_digest": cfg,
			"prestart_checks": M{"target_matches": true, "model_matches": true, "policy_matches": true}, "launch_audit_ref": id("audit", "launch-1")},
		M{"seq": 4, "kind": "enroll", "execution_ref": e2, "run_ref": id("run", "run-2"), "attempt_ref": id("attempt", "a1"), "task_id": id("task", "t1"),
			"planned_configuration_digest": cfg},
		M{"seq": 5, "kind": "finish", "execution_ref": e1, "finish_state": "exited", "finish_audit_ref": id("audit", "finish-1"), "ledger_ref": id("ledger", "run-1")},
		M{"seq": 6, "kind": "finish", "execution_ref": e2, "finish_state": "refused_before_launch", "finish_audit_ref": id("audit", "finish-2"), "ledger_ref": nil},
		M{"seq": 7, "kind": "epoch_seal", "frozen_open_revision_digest": digestOf("open-2"), "executions": 2},
	}
	mem := M{"unit_ref": id("unit", unit), "events": events}
	rechain(mem)
	return mem
}

// membershipDoc is the model ledger example with a project-origin binding plus the orchestrator
// source and one sealed membership epoch.
func membershipDoc(act M) M {
	d := ledgerDoc(act)
	d["document_id"] = id("document", "activity", "membership")
	src := orchestratorSource()
	d["sources"] = append(d["sources"].([]any), src)
	b := obj(d, "payload", "observations", 6, "model_ledger", "binding")
	b["origin"], b["execution_ref"] = "project", id("execution", "unit-1", "run-1")
	at := "2026-10-11T11:00:06Z"
	rec := M{"observation_id": id("membership", "unit-1", "1"), "class": "observation", "op": "MEMBERSHIP", "source": "orchestrator", "source_id": src["source_id"],
		"precision": "exact", "time": M{"clock": "collector", "start": at, "end": at}, "correlation": M{}, "outcome": M{"status": "success"},
		"membership": membershipEpoch("unit-1"), "missing": A{},
		"provenance": M{"captured_at": at, "source_event_id": id("membership-event", "unit-1", "1"),
			"transformations": A{M{"step": "normalize", "tool": "hint-cli", "version": "0.1.0"}}}}
	obj(d, "payload")["observations"] = append(obj(d, "payload")["observations"].([]any), rec)
	return reseal(d)
}

// membershipInvalid are the negative cases of membership records, the orchestrator source and
// the project binding variant.
func membershipInvalid() []invalidCase {
	const base = "activity-membership.json"
	last := func(d M) M {
		obs := obj(d, "payload")["observations"].([]any)
		return obs[len(obs)-1].(map[string]any)
	}
	mem := func(d M) M { return last(d)["membership"].(map[string]any) }
	events := func(d M) []any { return mem(d)["events"].([]any) }
	ev := func(d M, i int) M { return events(d)[i].(map[string]any) }
	src := func(d M) M {
		ss := d["sources"].([]any)
		return ss[len(ss)-1].(map[string]any)
	}
	binding := func(d M) M { return obj(d, "payload", "observations", 6, "model_ledger", "binding") }
	// a semantic case changes the events and then rechains them, so only the intended rule fails
	chained := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "semantic", hintconf.CodeMembership, func(d M) { f(d); rechain(mem(d)) }, nil}
	}
	remove := func(d M, i int) {
		es := events(d)
		mem(d)["events"] = append(append(A{}, es[:i]...), es[i+1:]...)
		for j, e := range mem(d)["events"].([]any) {
			e.(map[string]any)["seq"] = j + 1
		}
	}
	schema := func(name, code string, f func(d M)) invalidCase {
		return invalidCase{name, base, "schema", code, f, nil}
	}
	return []invalidCase{
		schema("membership-unknown-event-kind", "observation", func(d M) { ev(d, 1)["kind"] = "join" }),
		schema("membership-extra-field", "observation", func(d M) { ev(d, 2)["raw_command"] = "x" }),
		schema("membership-missing-prestart", "observation", func(d M) { delete(ev(d, 2), "prestart_checks") }),
		schema("membership-other-coverage", "observation", func(d M) { ev(d, 0)["coverage"] = "all_project_work" }),
		schema("project-binding-without-execution", "observation", func(d M) { delete(binding(d), "execution_ref") }),
		schema("project-binding-other-origin", "observation", func(d M) { binding(d)["origin"] = "controlled" }),
		{"membership-broken-chain", base, "semantic", hintconf.CodeMembership, func(d M) { ev(d, 3)["prev_digest"] = digestOf("other") }, nil},
		{"membership-head-mismatch", base, "semantic", hintconf.CodeMembership, func(d M) { mem(d)["head_digest"] = digestOf("other") }, nil},
		chained("membership-seq-gap", func(d M) { ev(d, 3)["seq"] = 5 }),
		chained("membership-without-seal", func(d M) { remove(d, 6) }),
		chained("membership-two-openers", func(d M) {
			es := events(d)
			second := plain(ev(d, 0))
			mem(d)["events"] = append(A{es[0], second}, es[1:]...)
			for j, e := range mem(d)["events"].([]any) {
				e.(map[string]any)["seq"] = j + 1
			}
		}),
		chained("membership-event-after-seal", func(d M) {
			es := events(d)
			es[5], es[6] = es[6], es[5]
			ev(d, 5)["seq"], ev(d, 6)["seq"] = 6, 7
		}),
		chained("membership-launch-without-enroll", func(d M) { ev(d, 2)["execution_ref"] = id("execution", "unit-1", "run-9") }),
		chained("membership-exited-without-launch", func(d M) { ev(d, 5)["finish_state"] = "exited" }),
		chained("membership-refused-after-launch", func(d M) { ev(d, 4)["finish_state"] = "refused_before_launch" }),
		chained("membership-execution-without-terminal", func(d M) { remove(d, 5); ev(d, 5)["executions"] = 2 }),
		chained("membership-seal-count", func(d M) { ev(d, 6)["executions"] = 3 }),
		chained("membership-duplicate-run", func(d M) { ev(d, 3)["run_ref"] = ev(d, 1)["run_ref"] }),
		chained("membership-first-epoch-with-predecessor", func(d M) { ev(d, 0)["predecessor_epoch_digest"] = digestOf("earlier") }),
		chained("membership-later-epoch-without-predecessor", func(d M) { ev(d, 0)["epoch"] = 2 }),
		{"membership-correlation-session", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { last(d)["correlation"] = M{"session_id": id("session", "s")} }, nil},
		{"membership-correlation-turn", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) {
			last(d)["correlation"] = M{"session_id": id("session", "s"), "turn_id": id("turn", "t")} // a turn needs its session (schema)
		}, nil},
		{"membership-correlation-trace", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { last(d)["correlation"] = M{"trace_id": id("trace", "t")} }, nil},
		{"membership-correlation-task", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { last(d)["correlation"] = M{"task_id": id("task", "t1")} }, nil},
		{"membership-correlation-run", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) {
			last(d)["correlation"] = M{"task_id": id("task", "t1"), "run_ref": id("run", "run-1")} // a run binding needs its task (schema)
		}, nil},
		{"orchestrator-correlation-not-unavailable", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) {
			src(d)["unavailable"] = A{"usage", "request_kind", "collection", "context", "coordination", "targets"}
		}, nil},
		{"orchestrator-claims-usage", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { src(d)["capabilities"] = A{"membership", "usage"} }, nil},
		{"orchestrator-other-version", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { src(d)["adapter"].(map[string]any)["version"] = "9.0.0" }, nil},
		{"membership-from-other-source", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) { last(d)["source_id"] = d["sources"].([]any)[0].(map[string]any)["source_id"] }, nil},
		{"orchestrator-observation-not-membership", base, "semantic", hintconf.CodeSourceOrchestrator, func(d M) {
			r := plain(last(d)) // a second orchestrator record that is not a MEMBERSHIP record
			delete(r, "membership")
			r["observation_id"], r["op"] = id("membership", "unit-1", "other"), "UNKNOWN"
			obj(d, "payload")["observations"] = append(obj(d, "payload")["observations"].([]any), r)
		}, nil},
	}
}

// membershipEpochsDoc is the membership example plus the second sealed epoch of the same unit:
// its predecessor is the digest of the whole first epoch, and its one execution was launched
// (a durable prestart intent) and then start_unconfirmed.
func membershipEpochsDoc(act M) M {
	d := membershipDoc(act)
	d["document_id"] = id("document", "activity", "membership-epochs")
	obs := obj(d, "payload")["observations"].([]any)
	first := obs[len(obs)-1].(map[string]any)
	pred, err := hintconf.Digest(first["membership"])
	check(err)
	e3 := id("execution", "unit-1", "run-3")
	cfg := digestOf("configuration")
	events := A{
		M{"seq": 1, "kind": "epoch_open", "epoch": 2, "original_scope_digest": digestOf("scope"), "enrollment_start_revision_digest": digestOf("open-3"),
			"policy_digest": digestOf("policy"), "coverage": "hint_orchestrated_executions", "predecessor_epoch_digest": pred},
		M{"seq": 2, "kind": "enroll", "execution_ref": e3, "run_ref": id("run", "run-3"), "attempt_ref": id("attempt", "a2"), "task_id": id("task", "t2"),
			"planned_configuration_digest": cfg},
		M{"seq": 3, "kind": "launch", "execution_ref": e3, "launch_digest": digestOf("launch"), "execution_digest": digestOf("execution-3"), "installed_target": "linux-arm64",
			"model_ref": id("model", "m"), "gate_policy_digest": digestOf("gate"), "source_configuration_digest": cfg,
			"prestart_checks": M{"target_matches": true, "model_matches": true, "policy_matches": true}, "launch_audit_ref": id("audit", "launch-3")},
		M{"seq": 4, "kind": "finish", "execution_ref": e3, "finish_state": "start_unconfirmed", "finish_audit_ref": id("audit", "finish-3"), "ledger_ref": nil},
		M{"seq": 5, "kind": "epoch_seal", "frozen_open_revision_digest": digestOf("open-4"), "executions": 1},
	}
	mem := M{"unit_ref": id("unit", "unit-1"), "events": events}
	rechain(mem)
	at := "2026-10-11T11:00:07Z"
	rec := plain(first)
	rec["observation_id"], rec["membership"] = id("membership", "unit-1", "2"), mem
	rec["time"] = M{"clock": "collector", "start": at, "end": at}
	rec["provenance"].(map[string]any)["captured_at"] = at
	rec["provenance"].(map[string]any)["source_event_id"] = id("membership-event", "unit-1", "2")
	obj(d, "payload")["observations"] = append(obs, rec)
	return reseal(d)
}

// membershipEpochsInvalid are the negative cases of the predecessor rule.
func membershipEpochsInvalid() []invalidCase {
	const base = "membership-epochs.json"
	second := func(d M) M {
		obs := obj(d, "payload")["observations"].([]any)
		return obs[len(obs)-1].(map[string]any)["membership"].(map[string]any)
	}
	first := func(d M) M {
		obs := obj(d, "payload")["observations"].([]any)
		return obs[len(obs)-2].(map[string]any)["membership"].(map[string]any)
	}
	opener := func(d M) M { return second(d)["events"].([]any)[0].(map[string]any) }
	// each case rechains the second epoch, so only the predecessor rule fails
	pred := func(name string, f func(d M) any) invalidCase {
		return invalidCase{name, base, "semantic", hintconf.CodeMembership, func(d M) { opener(d)["predecessor_epoch_digest"] = f(d); rechain(second(d)) }, nil}
	}
	return []invalidCase{
		pred("membership-predecessor-other", func(d M) any { return digestOf("other") }),
		pred("membership-predecessor-is-head", func(d M) any { return first(d)["head_digest"] }),
		pred("membership-predecessor-of-events-only", func(d M) any {
			dd, err := hintconf.Digest(first(d)["events"])
			check(err)
			return dd
		}),
	}
}
