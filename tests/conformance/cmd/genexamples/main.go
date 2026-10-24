// Command genexamples writes the synthetic v0.1 examples and conformance fixtures.
//
// All names are synthetic. Identifiers are derived with the published test keys of
// tests/conformance/vectors/ids-v0.1.json, so every example can be recomputed.
// Usage (from tests/conformance): go run ./cmd/genexamples -root ../..
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

type M = map[string]any
type A = []any

var (
	K1 = seq(0)  // project key of namespace 1 (test key)
	K2 = seq(32) // project key of an unrelated namespace (test key)
)

func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func id(domain string, parts ...string) string { return hintconf.DeriveID(K1, domain, parts...) }

// ---------------------------------------------------------------------------
// quantities
// ---------------------------------------------------------------------------
func known(unit string, v float64, prec, method string) M {
	q := M{"unit": unit, "state": "known", "value": v, "precision": prec}
	if method != "" {
		q["method"] = method
	}
	return q
}

func unknown(unit, reason string) M { return M{"unit": unit, "state": "unknown", "reason": reason} }

func partial(unit string, part float64, covered, total int, prec, reason string) M {
	return M{"unit": unit, "state": "partial", "known_part": part, "precision": prec, "reason": reason,
		"coverage": M{"covered": covered, "total": total}}
}

func interval(lo, hi, level float64) M { return M{"lower": lo, "upper": hi, "level": level} }

// ---------------------------------------------------------------------------
// envelope
// ---------------------------------------------------------------------------
const created = "2026-10-11T12:00:00Z"

var allTypes = A{"Snapshot", "Entity", "Relation", "PolicyConstraint", "Change", "Observation", "Metric", "Plan", "Effect"}

func envelope(docType string, repos []string, recordTypes A, payload M) M {
	r := A{}
	for _, x := range repos {
		r = append(r, x)
	}
	return M{
		"hint_version":      "0.1.0",
		"document_type":     docType,
		"document_id":       id("document", docType, strings.Join(repos, ",")),
		"project_namespace": hintconf.NamespaceID(K1),
		"repositories":      r,
		"producer":          M{"tool": "hint-cli", "version": "0.1.0"},
		"created_at":        created,
		"redaction": M{"policy_id": policyID, "profile": "detailed", "record_types": recordTypes,
			"free_text": "denied", "time_resolution_seconds": 0},
		"payload": payload,
	}
}

var policyID = id("policy", "default")

func seal(doc M) M {
	if doc["document_type"] == "repository_snapshot" {
		p := doc["payload"].(M)
		for _, s := range p["snapshots"].(A) {
			sm := s.(M)
			delete(sm, "digest")
			d, err := hintconf.SnapshotDigest(plain(p), sm["snapshot_id"].(string))
			check(err)
			sm["digest"] = d
		}
	}
	d, err := hintconf.Digest(doc["payload"])
	check(err)
	doc["content_digest"] = d
	return doc
}

// plain round-trips through JSON so the digest functions see decoded values.
func plain(v any) map[string]any {
	b, err := json.Marshal(v)
	check(err)
	var m map[string]any
	check(json.Unmarshal(b, &m))
	return m
}

// ---------------------------------------------------------------------------
// repository snapshots
// ---------------------------------------------------------------------------
type repo struct {
	name, id, snap string
	ents           []M
	rels           []M
	cons           []M
}

func newRepo(name, commit string) *repo {
	r := &repo{name: name, id: hintconf.RepoID(K1, name)}
	r.snap = id("snapshot", r.id, commit)
	r.ents = append(r.ents, M{"entity_id": r.id, "kind": "repository", "parent_id": nil, "repo_id": r.id,
		"snapshot_id": r.snap, "id_stability": "stable", "language": nil, "resolution": "declared"})
	return r
}

func (r *repo) path(p string) string { return hintconf.FileID(K1, r.id, p) }
func (r *repo) sym(q string) string  { return hintconf.SymbolID(K1, r.id, "go", q) }

func (r *repo) ent(eid, kind, parent string, lang any, extra M) string {
	e := M{"entity_id": eid, "kind": kind, "parent_id": parent, "repo_id": r.id, "snapshot_id": r.snap,
		"id_stability": "stable", "language": lang, "resolution": "syntax"}
	for k, v := range extra {
		e[k] = v
	}
	r.ents = append(r.ents, e)
	r.rel("contains", parent, eid, "syntax", "tree_sitter", nil)
	return eid
}

func (r *repo) rel(kind, src, dst, res, ev string, extra M) {
	x := M{"relation_id": id("relation", r.snap, kind, src, dst), "kind": kind, "source": src, "target": dst,
		"resolution": res, "evidence": ev, "snapshot_id": r.snap}
	for k, v := range extra {
		x[k] = v
	}
	r.rels = append(r.rels, x)
}

func (r *repo) snapshotRecord(commit string, langs A) M {
	return M{"snapshot_id": r.snap, "repo_id": r.id, "commit_ref": id("commit", r.id, commit),
		"parent_snapshot_ids": A{}, "captured_at": created, "worktree_state": "clean",
		"scanner": M{"name": "hint-scan", "version": "0.1.0"}, "languages": langs, "digest": ""}
}

func sortRecs(recs []M, key string) A {
	sort.Slice(recs, func(i, j int) bool {
		a := recs[i]["snapshot_id"].(string) + "/" + recs[i][key].(string)
		b := recs[j]["snapshot_id"].(string) + "/" + recs[j][key].(string)
		return a < b
	})
	out := A{}
	for _, r := range recs {
		out = append(out, r)
	}
	return out
}

func snapshotDoc(repos []*repo, commits []string, langs A) M {
	snaps, ents, rels, cons := A{}, []M{}, []M{}, []M{}
	ids := []string{}
	for i, r := range repos {
		snaps = append(snaps, r.snapshotRecord(commits[i], langs))
		ents = append(ents, r.ents...)
		rels = append(rels, r.rels...)
		cons = append(cons, r.cons...)
		ids = append(ids, r.id)
	}
	p := M{"snapshots": snaps, "entities": sortRecs(ents, "entity_id"), "relations": sortRecs(rels, "relation_id")}
	types := A{"Snapshot", "Entity", "Relation"}
	if len(cons) > 0 {
		p["constraints"] = sortRecs(cons, "constraint_id")
		types = append(types, "PolicyConstraint")
	}
	return seal(envelope("repository_snapshot", ids, types, p))
}

// shopkit builds the single-repository example; after=true gives the state after a file
// rename (billing/ledger.go -> billing/ledger_core.go), a function move and a split.
func shopkit(after bool) *repo {
	commit := "c1"
	if after {
		commit = "c2"
	}
	r := newRepo("shopkit", commit)
	billing := r.ent(r.path("billing"), "directory", r.id, nil, nil)
	invoices := r.ent(r.path("invoices"), "directory", r.id, nil, nil)
	ledgerPath := "billing/ledger.go"
	if after {
		ledgerPath = "billing/ledger_core.go"
	}
	ledger := r.ent(r.path(ledgerPath), "file", billing, "go", M{"features": M{
		"size_bytes": known("bytes", 12000, "exact", "file_size"), "lines": known("lines", 410, "exact", "scanner"),
		"fan_in": unknown("count", "not_collected")}})
	format := r.ent(r.path("invoices/format.go"), "file", invoices, "go", nil)
	apply := r.ent(r.sym("shopkit/billing.Apply"), "function", ledger, "go", M{"flags": A{"public_api"},
		"features": M{"parameters": known("count", 2, "exact", "scanner"), "cyclomatic": known("count", 7, "exact", "scanner")}})
	rateParent := ledger
	if !after {
		rates := r.ent(r.path("billing/rates.go"), "file", billing, "go", nil)
		rateParent = rates
		r.ent(r.sym("shopkit/billing.Fee"), "function", rates, "go", nil)
	} else {
		r.ent(r.sym("shopkit/billing.FeeFlat"), "function", ledger, "go", nil)
		r.ent(r.sym("shopkit/billing.FeePct"), "function", ledger, "go", nil)
	}
	rate := r.ent(r.sym("shopkit/billing.Rate"), "function", rateParent, "go", nil)
	fmtFn := r.ent(r.sym("shopkit/invoices.Format"), "function", format, "go", nil)
	blk := id("local", r.snap, apply, "1")
	r.ents = append(r.ents, M{"entity_id": blk, "kind": "block", "parent_id": apply, "repo_id": r.id,
		"snapshot_id": r.snap, "id_stability": "snapshot_local", "language": "go", "resolution": "syntax"})
	r.rel("contains", apply, blk, "syntax", "tree_sitter", nil)
	r.rel("calls", apply, rate, "syntax", "tree_sitter", M{"confidence": 0.9})
	r.rel("calls", fmtFn, rate, "type_checked", "compiler", nil)
	r.rel("imports", format, billing, "compiler", "compiler", nil)
	r.cons = append(r.cons, M{"constraint_id": id("constraint", r.snap, "api"), "snapshot_id": r.snap,
		"kind": "api_compatibility", "check": "deterministic", "scope": A{apply}, "blocks_destructive": true})
	return r
}

func multiRepo() M {
	contract := hintconf.ContractID(K1, "idl_service", "shopkit.billing.v1.BillingService")
	mk := func(name, file, lang string) *repo {
		r := newRepo(name, "c1")
		f := r.ent(r.path(file), "file", r.id, lang, nil)
		r.ents = append(r.ents, M{"entity_id": contract, "kind": "contract", "parent_id": r.id, "repo_id": r.id,
			"snapshot_id": r.snap, "id_stability": "stable", "language": nil, "resolution": "declared"})
		r.rel("contains", r.id, contract, "declared", "idl", nil)
		r.rel("contract_ref", f, contract, "declared", "idl", M{"contract_evidence": "idl_definition"})
		return r
	}
	api := mk("shopkit", "api/billing.proto", "proto")
	web := mk("shopkit-web", "web/billing_client.ts", "typescript")
	return snapshotDoc([]*repo{api, web}, []string{"c1", "c1"}, A{"proto", "typescript"})
}

// ---------------------------------------------------------------------------
// change set
// ---------------------------------------------------------------------------
func changeSet(before, after M, b, a *repo) M {
	snap := func(doc M) M {
		s := doc["payload"].(M)["snapshots"].(A)[0].(M)
		return M{"snapshot_id": s["snapshot_id"], "digest": s["digest"]}
	}
	prov := M{"source": "entity_matcher", "tool": "hint-scan", "version": "0.1.0"}
	oldLedger, newLedger := b.path("billing/ledger.go"), a.path("billing/ledger_core.go")
	ch := func(op, level string, bef, aft A, method string, extra M) M {
		c := M{"change_id": id("change", op, fmt.Sprint(bef), fmt.Sprint(aft)), "operation": op, "level": level,
			"before": bef, "after": aft, "match_method": method, "provenance": prov}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	changes := A{
		ch("RENAME", "artifact", A{oldLedger}, A{newLedger}, "git_rename", M{"match_confidence": 0.97,
			"provenance": M{"source": "git_diff", "tool": "hint-scan", "version": "0.1.0"},
			"magnitude":  M{"lines_added": known("lines", 3, "exact", "git_diff"), "lines_deleted": known("lines", 1, "exact", "git_diff")}}),
		ch("MOVE", "symbol", A{b.sym("shopkit/billing.Rate")}, A{a.sym("shopkit/billing.Rate")}, "key",
			M{"match_confidence": 1.0, "deltas": M{"signature": "none", "control": "none", "data": "none"}}),
		ch("DELETE", "artifact", A{b.path("billing/rates.go")}, A{}, "none", nil),
		ch("AMBIGUOUS", "symbol", A{b.sym("shopkit/billing.Fee")}, A{a.sym("shopkit/billing.FeeFlat"), a.sym("shopkit/billing.FeePct")},
			"similarity", M{"match_confidence": 0.61, "candidates": A{
				M{"before": b.sym("shopkit/billing.Fee"), "after": a.sym("shopkit/billing.FeeFlat"), "score": 0.61},
				M{"before": b.sym("shopkit/billing.Fee"), "after": a.sym("shopkit/billing.FeePct"), "score": 0.58}}}),
		ch("MODIFY", "symbol", A{b.sym("shopkit/billing.Apply")}, A{a.sym("shopkit/billing.Apply")}, "key", M{
			"match_confidence": 1.0, "structural_similarity": 0.88,
			"magnitude": M{"lines_added": known("lines", 4, "exact", "git_diff"), "lines_deleted": known("lines", 2, "exact", "git_diff")},
			"deltas": M{"signature": "compatible", "control": "changed", "data": "none",
				"dependency": M{"added": known("count", 1, "exact", "scanner"), "removed": known("count", 0, "exact", "scanner")}}}),
	}
	return seal(envelope("change_set", []string{b.id}, A{"Change"}, M{
		"repo_id": b.id, "before_snapshot": snap(before), "after_snapshot": snap(after), "changes": changes}))
}

// ---------------------------------------------------------------------------
// activity
// ---------------------------------------------------------------------------
var (
	srcPlugin  = id("source", "opencode-plugin")
	srcWatcher = id("source", "watcher")
	srcProxy   = id("source", "model-proxy")
	taskID     = id("task", "t1")
	sessionID  = id("session", "s1")
	turnID     = id("turn", "s1", "1")
)

func obs(n int, op, source, src, prec string, start, end any, status string, extra M) M {
	o := M{"observation_id": id("event", src, fmt.Sprint(n)), "class": "observation", "op": op, "source": source,
		"source_id": src, "precision": prec,
		"time":        M{"start": start, "end": end, "clock": "agent"},
		"correlation": M{"task_id": taskID, "session_id": sessionID, "turn_id": turnID},
		"outcome":     M{"status": status}, "missing": A{},
		"provenance": M{"captured_at": "2026-10-11T11:00:30Z", "source_event_id": id("native", src, fmt.Sprint(n)),
			"transformations": A{M{"step": "normalize", "tool": "hint-cli", "version": "0.1.0"},
				M{"step": "redact", "tool": "hint-cli", "version": "0.1.0"}}}}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

func ts(s string) string { return "2026-10-11T11:00:" + s + "Z" }

func activity(r *repo) (M, []M) {
	ledger := r.path("billing/ledger.go")
	o := []M{
		obs(1, "SESSION", "plugin", srcPlugin, "exact", ts("00"), ts("59"), "success", nil),
		obs(2, "TURN", "plugin", srcPlugin, "exact", ts("01"), ts("58"), "success", nil),
		obs(3, "READ", "plugin", srcPlugin, "exact", ts("02.120"), ts("02.135"), "success", M{
			"targets": A{M{"entity_id": ledger}}, "content_delivery": "full",
			"size":        known("bytes", 12000, "exact", "plugin_output_length"),
			"correlation": M{"task_id": taskID, "session_id": sessionID, "turn_id": turnID, "tool_call_id": id("call", "3")}}),
		obs(4, "SEARCH", "plugin", srcPlugin, "exact", ts("03"), ts("03.010"), "success", M{
			"targets": A{}, "missing": A{"targets"}, "size": known("bytes", 217, "exact", "plugin_output_length")}),
		obs(5, "EXECUTE", "plugin", srcPlugin, "exact", ts("04"), ts("05"), "success", M{
			"command_class": "UNKNOWN", "outcome": M{"status": "success", "exit_code": 0}}),
		obs(6, "TEST", "shell_parser", srcPlugin, "inferred", ts("06"), ts("12"), "failure", M{
			"outcome": M{"status": "failure", "exit_code": 1}, "targets": A{M{"scope_id": r.path("billing")}}}),
		obs(7, "EDIT_INTENT", "plugin", srcPlugin, "exact", ts("13"), ts("13.050"), "success", M{
			"targets": A{M{"entity_id": ledger, "range": M{"start_line": 10, "end_line": 14}}}}),
		obs(8, "FILE_MUTATION", "watcher", srcWatcher, "exact", ts("13.060"), ts("13.060"), "success", M{
			"targets": A{M{"entity_id": ledger}}, "mutation_attribution": "none",
			"time":        M{"start": ts("13.060"), "end": ts("13.060"), "clock": "collector"},
			"correlation": M{}, "missing": A{"correlation.session_id", "correlation.task_id"}}),
		obs(9, "USAGE", "transport", srcProxy, "exact", ts("02"), ts("02.900"), "success", M{
			"usage": M{"input_tokens": known("tokens", 7710, "exact", "model_server_usage"),
				"output_tokens":     known("tokens", 98, "exact", "model_server_usage"),
				"cache_read_tokens": unknown("tokens", "not_supported")}}),
		obs(10, "USAGE", "transport", srcProxy, "unknown", ts("14"), ts("15"), "success", M{
			"usage":   M{"input_tokens": unknown("tokens", "source_error"), "output_tokens": unknown("tokens", "source_error")},
			"missing": A{"usage"}}),
		obs(11, "READ", "log", srcPlugin, "derived", nil, ts("16"), "unknown", M{
			"targets": A{M{"entity_id": ledger}}, "content_delivery": "unknown",
			"size":        unknown("bytes", "not_collected"),
			"correlation": M{"task_id": taskID}, "missing": A{"correlation.session_id", "time.start", "size"}}),
	}
	oid := func(i int) string { return o[i-1]["observation_id"].(string) }
	metric := func(name, class, metric, unit, scopeKind string, v M, inputs ...int) M {
		in := A{}
		for _, i := range inputs {
			in = append(in, oid(i))
		}
		return M{"metric_id": id("metric", name), "class": class, "metric": metric, "unit": unit,
			"scope": M{"kind": scopeKind, "id": taskID}, "value": v, "inputs": in, "definition_version": "0.1.0"}
	}
	crossings := unknown("count", "not_collected")
	crossings["bounds"] = M{"lower": 0, "upper": 2}
	metrics := []M{
		metric("context_bytes", "derived", "context_bytes", "bytes", "task", partial("bytes", 12217, 2, 3, "exact", "not_collected"), 3, 4, 11),
		metric("context_tokens", "estimate", "context_tokens", "tokens", "task", partial("tokens", 3054, 2, 3, "estimated", "not_collected"), 3, 4, 11),
		metric("boundary_crossings", "derived", "boundary_crossings", "count", "task", crossings, 3, 4, 11),
		metric("model_tokens", "derived", "model_tokens", "tokens", "task", partial("tokens", 7808, 1, 2, "exact", "source_error"), 9, 10),
		metric("validation_failures", "derived", "validation_failures", "count", "task", known("count", 1, "derived", "count_of_records"), 6),
		metric("task_elapsed_seconds", "derived", "task_elapsed_seconds", "seconds", "task", known("seconds", 59, "derived", "clock_difference"), 1),
	}
	cov := func(recv M, drop M) M { return M{"events_received": recv, "events_dropped": drop} }
	dropped := unknown("count", "dropped")
	dropped["bounds"] = M{"lower": 1, "upper": nil}
	all := []string{"time.start", "time.end", "correlation.task_id", "correlation.session_id", "correlation.turn_id",
		"correlation.tool_call_id", "targets", "outcome.status", "outcome.exit_code", "size", "command_class", "content_delivery"}
	toA := func(l []string) A {
		out := A{}
		for _, s := range l {
			out = append(out, s)
		}
		return out
	}
	sources := A{
		M{"source_id": srcPlugin, "agent": "opencode", "agent_version": "2.0.26",
			"adapter": M{"name": "opencode-plugin", "version": "0.1.0"}, "capabilities": toA(all),
			"unavailable": A{"correlation.trace_id", "correlation.span_id", "correlation.parent_span_id", "usage", "coordination"},
			"coverage":    cov(known("count", 9, "exact", "count_of_records"), known("count", 0, "exact", "count_of_records"))},
		M{"source_id": srcWatcher, "agent": "none", "agent_version": nil,
			"adapter": M{"name": "watcher", "version": "0.1.0"}, "capabilities": A{"time.start", "time.end", "targets"},
			"unavailable": A{"correlation.session_id", "correlation.task_id", "outcome.exit_code"},
			"coverage":    cov(known("count", 1, "exact", "count_of_records"), dropped)},
		M{"source_id": srcProxy, "agent": "none", "agent_version": nil,
			"adapter": M{"name": "other", "version": "0.1.0"}, "capabilities": A{"time.start", "time.end", "usage"},
			"unavailable": A{"targets"}},
	}
	oa, ma := A{}, A{}
	for _, x := range o {
		oa = append(oa, x)
	}
	for _, x := range metrics {
		ma = append(ma, x)
	}
	doc := envelope("activity_stream", []string{r.id}, A{"Observation", "Metric"}, M{"observations": oa, "metrics": ma})
	doc["sources"] = sources
	return seal(doc), o
}

// ---------------------------------------------------------------------------
// recommendations, effects, policy, receipt
// ---------------------------------------------------------------------------
func recommendations(r *repo, snapDoc M, o []M) M {
	s := snapDoc["payload"].(M)["snapshots"].(A)[0].(M)
	snaps := A{M{"repo_id": r.id, "snapshot_id": s["snapshot_id"], "digest": s["digest"]}}
	ledger, apply, rate := r.path("billing/ledger.go"), r.sym("shopkit/billing.Apply"), r.sym("shopkit/billing.Rate")
	constraintID := id("constraint", r.snap, "api")
	plan := func(name, class string, transform any, decision string, extra M) M {
		p := M{"plan_id": id("plan", name), "plan_schema": "0.1.0", "project_namespace": hintconf.NamespaceID(K1),
			"repo_ids": A{r.id}, "snapshots": snaps, "intervention_class": class, "transformation": transform,
			"decision": decision, "targets": A{}, "preconditions": A{}, "steps": A{}, "reasons": A{},
			"risk_constraints": A{}, "required_approvals": A{}, "created_at": created, "expires_at": "2026-10-18T12:00:00Z"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	sim := func(level, status string) M {
		return M{"level": level, "task_distribution_id": id("taskdist", "shopkit-2026-10"),
			"agent_configuration_id": id("agentcfg", "opencode-2.0.26-local"), "model_version": "0.1.0",
			"calibration_status": status, "out_of_distribution": false}
	}
	p1 := plan("split-ledger", "REFACTOR_CODE", "SPLIT", "recommend", M{
		"targets": A{ledger, apply, rate},
		"preconditions": A{M{"check": "snapshot_matches"}, M{"check": "entities_resolve", "targets": A{ledger, apply, rate}},
			M{"check": "worktree_clean"}, M{"check": "tests_pass", "targets": A{ledger}}},
		"steps": A{
			M{"step_id": 1, "action": "SPLIT", "targets": A{ledger}, "depends_on": A{}, "requires_approval": true, "support": "supported"},
			M{"step_id": 2, "action": "RUN_CHECKS", "targets": A{ledger}, "depends_on": A{1}, "requires_approval": false, "support": "supported"}},
		"reasons": A{M{"code": "context_size", "observation_ids": A{o[2]["observation_id"]}, "metric_ids": A{id("metric", "context_bytes")}},
			M{"code": "boundary_crossings", "metric_ids": A{id("metric", "boundary_crossings")}}},
		"predictions": A{
			M{"class": "forecast", "metric": "context_bytes", "unit": "bytes", "per": "task", "interval": interval(-6000, -1500, 0.9),
				"point": -3200, "probability_improvement": 0.86},
			M{"class": "forecast", "metric": "model_tokens", "unit": "tokens", "per": "task", "interval": interval(-4410, -1903, 0.9),
				"point": -2963, "probability_improvement": 0.97}},
		"simulation":                sim("L3", "backtested"),
		"refactoring_cost_estimate": M{"unit": "tokens", "interval": interval(180000, 320000, 0.9)},
		"break_even_horizon":        M{"unit": "tasks", "interval": interval(57, 131, 0.9), "unbounded": false},
		"risk_constraints": A{M{"kind": "test", "requirement": "tests_unchanged_pass", "deterministic": true},
			M{"kind": "api", "requirement": "public_api_unchanged", "constraint_id": constraintID, "deterministic": true}},
		"required_approvals":  A{"owner"},
		"authorization_basis": "deterministic",
	})
	p2 := plan("graph-context", "CONTEXT_ONLY", nil, "refuse_uncertain", M{
		"reasons": A{M{"code": "low_adoption"}, M{"code": "weak_evidence"}},
		"predictions": A{M{"class": "forecast", "metric": "model_tokens", "unit": "tokens", "per": "task",
			"interval": interval(-9001, 2118, 0.9), "probability_improvement": 0.55}},
		"simulation": sim("L3", "uncalibrated"),
	})
	p3 := plan("collect-evidence", "COLLECT_MORE_EVIDENCE", nil, "collect_more_evidence", M{
		"reasons": A{M{"code": "weak_evidence"}}, "simulation": M{"level": "NONE"}})
	return seal(envelope("recommendation_set", []string{r.id}, A{"Plan"}, M{"plans": A{p1, p2, p3}}))
}

func effects(r *repo, after *repo) M {
	tasks := func(prefix string) A {
		return A{id("task", prefix, "1"), id("task", prefix, "2"), id("task", prefix, "3")}
	}
	e := M{"effect_id": id("effect", "split-ledger"), "class": "confirmed_effect", "evidence_level": "L4",
		"plan_id": id("plan", "split-ledger"), "metric": "model_tokens", "unit": "tokens",
		"control":   M{"snapshot_id": r.snap, "task_ids": tasks("control")},
		"treatment": M{"snapshot_id": after.snap, "task_ids": tasks("treatment")},
		"pairs":     3, "estimate": interval(-5200, 800, 0.9), "method": "paired_difference_bootstrap",
		"constraints_checked":    A{"tests_unchanged_pass", "public_api_unchanged"},
		"agent_configuration_id": id("agentcfg", "opencode-2.0.26-local")}
	return seal(envelope("effect_report", []string{r.id}, A{"Effect"}, M{"effects": A{e}}))
}

func exportPolicy(fields M) M {
	doc := envelope("export_policy", nil, A{}, M{"policy_id": policyID, "profile": "detailed",
		"record_types": allTypes, "fields": fields, "time_resolution_seconds": 0})
	return seal(doc)
}

func receipt(accepted []M) M {
	acc := A{}
	for _, d := range accepted {
		acc = append(acc, M{"document_id": d["document_id"], "content_digest": d["content_digest"]})
	}
	doc := envelope("receipt", nil, A{}, M{"accepted": acc, "rejected": A{M{"document_id": nil, "code": "schema_invalid"}}})
	doc["producer"] = M{"tool": "hint-server", "version": "0.1.0"}
	return seal(doc)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------
func check(err error) {
	if err != nil {
		panic(err)
	}
}

func write(path string, v any) {
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	b, err := json.MarshalIndent(v, "", "  ")
	check(err)
	check(os.WriteFile(path, append(b, '\n'), 0o644))
}

func clone(v M) M { return plain(v) }

func main() {
	root := flag.String("root", "../..", "repository root")
	flag.Parse()
	ex := func(n string) string { return filepath.Join(*root, "examples", "v0.1", n) }
	conf := func(n string) string { return filepath.Join(*root, "tests", "conformance", n) }

	before, after := shopkit(false), shopkit(true)
	single := snapshotDoc([]*repo{before}, []string{"c1"}, A{"go"})
	singleAfter := snapshotDoc([]*repo{after}, []string{"c2"}, A{"go"})
	multi := multiRepo()
	changes := changeSet(single, singleAfter, before, after)
	act, o := activity(before)
	rec := recommendations(before, single, o)
	eff := effects(before, after)
	fields := policyFields()
	pol := exportPolicy(fields)
	docs := map[string]M{
		"single-repo.json": single, "single-repo-after-rename.json": singleAfter, "multi-repo.json": multi,
		"change-set.json": changes, "activity-trace.json": act, "recommendation.json": rec,
		"effect-report.json": eff, "export-policy.json": pol,
	}
	docs["receipt.json"] = receipt([]M{single, act})
	for n, d := range docs {
		write(ex(n), d)
	}
	docs["activity-context.json"], docs["recommendation-findings.json"], docs["change-set-declared.json"] = writeV02(*root, act, pol, rec, changes)
	docs["activity-model-call.json"] = modelCallDoc(act)
	docs["activity-model-ledger.json"] = ledgerDoc(act)
	docs["activity-model-ledger-entries.json"] = ledgerEntriesDoc(act)
	docs["activity-model-ledger-agent-run.json"] = ledgerAgentRunDoc(act)
	docs["activity-model-ledger-backend-capture.json"] = ledgerBackendCaptureDoc(act)
	writeValidExtras(conf, act)
	writeInvalid(conf, docs)
	writeResolve(conf, rec, single)
	writeVectors(conf)
}

func policyFields() M {
	// every top-level field of every record type (the default detailed policy)
	f := M{}
	for rt, names := range recordFields {
		l := A{}
		for _, n := range names {
			l = append(l, n)
		}
		f[rt] = l
	}
	return f
}

var recordFields = map[string][]string{}

func init() {
	// read the field lists from the policy schema so the default policy matches the schema
	b, err := os.ReadFile(filepath.Join(rootGuess(), "schemas", "v0.1", "policy.schema.json"))
	check(err)
	var s map[string]any
	check(json.Unmarshal(b, &s))
	props := s["$defs"].(map[string]any)["PolicyPayload"].(map[string]any)["properties"].(map[string]any)["fields"].(map[string]any)["properties"].(map[string]any)
	for rt, v := range props {
		var names []string
		for _, n := range v.(map[string]any)["items"].(map[string]any)["enum"].([]any) {
			names = append(names, n.(string))
		}
		recordFields[rt] = names
	}
}

func rootGuess() string {
	for i, a := range os.Args {
		if a == "-root" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if strings.HasPrefix(a, "-root=") {
			return strings.TrimPrefix(a, "-root=")
		}
	}
	return "../.."
}
