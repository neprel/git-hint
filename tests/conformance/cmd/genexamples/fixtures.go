package main

import (
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

// ---------------------------------------------------------------------------
// navigation helpers for plain JSON values
// ---------------------------------------------------------------------------
func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

func obj(v any, path ...any) map[string]any { return at(v, path...).(map[string]any) }

func reseal(d M) M {
	// recompute snapshot digests and the content digest of a plain document
	p := d["payload"].(map[string]any)
	if d["document_type"] == "repository_snapshot" {
		for _, s := range p["snapshots"].([]any) {
			sm := s.(map[string]any)
			delete(sm, "digest")
			dg, err := hintconf.SnapshotDigest(p, sm["snapshot_id"].(string))
			check(err)
			sm["digest"] = dg
		}
	}
	dg, err := hintconf.Digest(p)
	check(err)
	d["content_digest"] = dg
	return d
}

func resealContent(d M) M {
	dg, err := hintconf.Digest(d["payload"])
	check(err)
	d["content_digest"] = dg
	return d
}

func walkTimestamps(v any, f func(string) string) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && (k == "start" || k == "end" || k == "captured_at" || k == "created_at" ||
				k == "expires_at" || k == "valid_from" || k == "valid_to") {
				t[k] = f(s)
				continue
			}
			walkTimestamps(x, f)
		}
	case []any:
		for _, x := range t {
			walkTimestamps(x, f)
		}
	}
}

// ---------------------------------------------------------------------------
// additional valid fixtures (plan 03 §9 and §11 cases)
// ---------------------------------------------------------------------------
func writeValidExtras(conf func(string) string, act M) {
	// coarse profile: timestamps truncated to the minute
	coarse := clone(act)
	obj(coarse, "redaction")["profile"] = "coarse"
	obj(coarse, "redaction")["time_resolution_seconds"] = 60
	walkTimestamps(coarse, func(s string) string { return s[:17] + "00Z" })
	write(conf("valid/activity-coarse.json"), reseal(coarse))

	// aggregate profile: metrics only, no observation records and no input references
	agg := clone(act)
	obj(agg, "redaction")["profile"] = "aggregate"
	obj(agg, "redaction")["record_types"] = A{"Metric"}
	p := obj(agg, "payload")
	p["observations"] = A{}
	for _, m := range p["metrics"].([]any) {
		m.(map[string]any)["inputs"] = A{}
	}
	write(conf("valid/activity-aggregate.json"), reseal(agg))

	// a later patch version of the same minor version is accepted
	patch := clone(act)
	patch["hint_version"] = "0.1.7"
	write(conf("valid/patch-version.json"), reseal(patch))

	// unseen tool, errors, compaction, coordination with a blocking wait, concurrent edits, time correction
	edge := clone(act)
	obs := obj(edge, "payload")["observations"].([]any)
	base := obs[1].(map[string]any)
	mk := func(n int, op string, extra M) M {
		o := plain(base)
		o["observation_id"] = id("event", "edge", string(rune('a'+n)))
		o["op"] = op
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	agentA, agentB := id("agent", "a"), id("agent", "b")
	req := id("request", "1")
	conflict := id("conflict", "1")
	edge["payload"].(map[string]any)["observations"] = append(obs,
		mk(0, "UNKNOWN", M{"precision": "unknown", "outcome": M{"status": "unknown"}}),
		mk(1, "ERROR", M{"outcome": M{"status": "failure", "exit_code": nil}}),
		mk(2, "COMPACTION", nil),
		mk(3, "DELEGATE", M{"coordination": M{"sender": agentA, "recipient": agentB, "request_id": req}}),
		mk(4, "REQUEST", M{"coordination": M{"sender": agentB, "recipient": agentA, "request_id": req,
			"owning_repo": hintconf.RepoID(K1, "shopkit")}}),
		mk(5, "WAIT", M{"coordination": M{"sender": agentA, "recipient": agentB, "request_id": req,
			"wait_evidence": "blocking_wait_observed"}}),
		mk(6, "RESPONSE", M{"coordination": M{"sender": agentB, "recipient": agentA, "request_id": req,
			"wait_evidence": "reply_only", "conflict_ids": A{conflict},
			"concurrent_scope": A{hintconf.FileID(K1, hintconf.RepoID(K1, "shopkit"), "billing/ledger.go")}}}),
		mk(7, "TOOL", M{"provenance": M{"captured_at": "2026-10-11T11:00:40Z", "valid_from": "2026-10-11T11:00:20Z",
			"transformations": A{M{"step": "normalize", "tool": "hint-cli", "version": "0.1.0"},
				M{"step": "time_correction", "tool": "hint-cli", "version": "0.1.0"}}}}),
	)
	write(conf("valid/activity-edge-cases.json"), reseal(edge))
}

// ---------------------------------------------------------------------------
// invalid fixtures
// ---------------------------------------------------------------------------
type invalidCase struct {
	name, base, level, code string
	mutate                  func(d M)
	seal                    func(d M) M
}

var forbidden = map[string]string{
	"prompt":     "HINTCANARY_PROMPT_9d1e07 fix the failing test",
	"command":    "cat canary_dir_e1d2/main.go | grep HINTCANARY_ARG_2b8c44",
	"output":     "ok HINTCANARY_TOOLOUT_5a6f32 0.012s",
	"path":       "billing/ledger.go",
	"identifier": "canarySymbolQ7",
	"branch":     "feature/canary",
	"url":        "https://git.example.invalid/org/shopkit.git",
	"email":      "dev@example.invalid",
	"host":       "build01.example.invalid",
	"env":        "HINT_CANARY_ENV=HINTCANARY_ENV_c4e819",
	"message":    "Fix canary bug in ledger",
	"package":    "@private-org/billing-core",
}

func writeInvalid(conf func(string) string, docs map[string]M) {
	other := id("other")
	cases := []invalidCase{
		// --- unknown, real zero, known part ---
		{"quantity-unknown-with-zero", "activity-trace.json", "schema", "quantity_state", func(d M) {
			obj(d, "payload", "metrics", 2)["value"] = M{"unit": "count", "state": "unknown", "reason": "not_collected", "value": 0}
		}, nil},
		{"quantity-known-without-value", "activity-trace.json", "schema", "quantity_state", func(d M) {
			delete(obj(d, "payload", "metrics", 4, "value"), "value")
		}, nil},
		{"quantity-partial-with-total", "activity-trace.json", "schema", "quantity_state", func(d M) {
			obj(d, "payload", "metrics", 0, "value")["value"] = 12217
		}, nil},
		{"quantity-unknown-without-reason", "activity-trace.json", "schema", "quantity_state", func(d M) {
			delete(obj(d, "payload", "metrics", 2, "value"), "reason")
		}, nil},
		{"usage-zero-without-state", "activity-trace.json", "schema", "quantity_state", func(d M) {
			obj(d, "payload", "observations", 9, "usage")["input_tokens"] = M{"unit": "tokens", "value": 0}
		}, nil},
		// --- absolute quantities are never negative; units constrain the value ---
		{"quantity-negative-size", "activity-trace.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "observations", 2, "size")["value"] = -1
		}, nil},
		{"quantity-negative-usage", "activity-trace.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "observations", 8, "usage", "output_tokens")["value"] = -98
		}, nil},
		{"quantity-negative-known-part", "activity-trace.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "metrics", 0, "value")["known_part"] = -12217
		}, nil},
		{"quantity-negative-bound", "activity-trace.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "metrics", 2, "value", "bounds")["lower"] = -2
		}, nil},
		{"quantity-negative-feature", "single-repo.json", "schema", "quantity_range", func(d M) {
			for _, e := range obj(d, "payload")["entities"].([]any) {
				if f, ok := e.(map[string]any)["features"].(map[string]any); ok && f["lines"] != nil {
					f["lines"].(map[string]any)["value"] = -410
					return
				}
			}
			panic("no entity with lines")
		}, nil},
		{"quantity-negative-change-magnitude", "change-set.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "changes", 0, "magnitude", "lines_deleted")["value"] = -1
		}, nil},
		{"quantity-count-fractional", "activity-trace.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "metrics", 4, "value")["value"] = 1.5
		}, nil},
		{"quantity-ratio-above-one", "activity-trace.json", "schema", "quantity_range", func(d M) {
			m := obj(d, "payload", "metrics", 4)
			m["metric"], m["unit"] = "success_rate", "ratio"
			m["value"] = M{"unit": "ratio", "state": "known", "value": 1.5, "precision": "derived", "method": "count_of_records"}
		}, nil},
		{"cost-estimate-negative", "recommendation.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "plans", 0, "refactoring_cost_estimate")["interval"] = interval(-320000, -180000, 0.9)
		}, nil},
		{"break-even-negative", "recommendation.json", "schema", "quantity_range", func(d M) {
			obj(d, "payload", "plans", 0, "break_even_horizon")["interval"] = interval(-131, -57, 0.9)
		}, nil},
		// --- forbidden content classes ---
		{"content-prompt", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "payload", "observations", 2)["prompt"] = forbidden["prompt"]
		}, nil},
		{"content-assistant-answer", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "payload", "observations", 1)["answer"] = "I changed canarySymbolQ7 in canary_dir_e1d2"
		}, nil},
		{"content-shell-command", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "payload", "observations", 4)["command"] = forbidden["command"]
		}, nil},
		{"content-terminal-output", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "payload", "observations", 4)["output"] = forbidden["output"]
		}, nil},
		{"content-env-var", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "payload", "observations", 4)["env"] = M{"HINT_CANARY_ENV": "HINTCANARY_ENV_c4e819"}
		}, nil},
		{"content-path-as-id", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "entities", 1)["entity_id"] = forbidden["path"]
		}, nil},
		{"content-identifier-name", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "entities", 1)["name"] = forbidden["identifier"]
		}, nil},
		{"content-source-code", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "entities", 1)["text"] = "func canarySymbolQ7() { return \"HINTCANARY_REPO_7f3a91\" } // comment"
		}, nil},
		{"content-branch-name", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "snapshots", 0)["branch"] = forbidden["branch"]
		}, nil},
		{"content-remote-url", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "snapshots", 0)["remote"] = forbidden["url"]
		}, nil},
		{"content-author-email", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "producer")["user"] = forbidden["email"]
		}, nil},
		{"content-hostname", "activity-trace.json", "schema", "content", func(d M) {
			obj(d, "sources", 0)["host"] = forbidden["host"]
		}, nil},
		{"content-commit-message", "change-set.json", "schema", "content", func(d M) {
			obj(d, "payload", "changes", 0)["message"] = forbidden["message"]
		}, nil},
		{"content-package-name", "multi-repo.json", "schema", "content", func(d M) {
			obj(d, "payload", "relations", 0)["target"] = forbidden["package"]
		}, nil},
		{"content-free-reason", "recommendation.json", "schema", "content", func(d M) {
			obj(d, "payload", "plans", 0, "reasons", 0)["code"] = "split because the ledger file is too big"
		}, nil},
		{"content-step-command", "recommendation.json", "schema", "content", func(d M) {
			obj(d, "payload", "plans", 0, "steps", 0)["command"] = "git mv billing/ledger.go billing/a.go"
		}, nil},
		{"content-version-label", "single-repo.json", "schema", "content", func(d M) {
			obj(d, "producer")["version"] = "0.1.0-canary"
		}, nil},
		{"content-policy-field-name", "export-policy.json", "schema", "content", func(d M) {
			f := obj(d, "payload", "fields")
			f["Entity"] = append(f["Entity"].([]any), "origin")
		}, nil},
		// --- evidence distinctions ---
		{"watcher-exact-attribution", "activity-trace.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "observations", 7)["mutation_attribution"] = "exact"
		}, nil},
		{"shell-parser-full-delivery", "activity-trace.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "observations", 5)["content_delivery"] = "full"
		}, nil},
		{"shell-parser-exact", "activity-trace.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "observations", 5)["precision"] = "exact"
		}, nil},
		{"read-without-delivery", "activity-trace.json", "schema", "evidence", func(d M) {
			delete(obj(d, "payload", "observations", 2), "content_delivery")
		}, nil},
		{"metric-unit-mismatch", "activity-trace.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "metrics", 0)["unit"] = "tokens"
		}, nil},
		{"derived-metric-estimated", "activity-trace.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "metrics", 4, "value")["precision"] = "estimated"
		}, nil},
		{"forecast-claims-l4", "recommendation.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "plans", 0, "simulation")["level"] = "L4"
		}, nil},
		{"effect-not-l4", "effect-report.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "effects", 0)["evidence_level"] = "L3"
		}, nil},
		{"effect-as-forecast", "effect-report.json", "schema", "evidence", func(d M) {
			obj(d, "payload", "effects", 0)["class"] = "forecast"
		}, nil},
		{"contract-without-evidence", "multi-repo.json", "schema", "evidence", func(d M) {
			for _, r := range obj(d, "payload")["relations"].([]any) {
				if r.(map[string]any)["kind"] == "contract_ref" {
					delete(r.(map[string]any), "contract_evidence")
				}
			}
		}, nil},
		{"contract-by-name-match", "multi-repo.json", "schema", "evidence", func(d M) {
			for _, r := range obj(d, "payload")["relations"].([]any) {
				if r.(map[string]any)["kind"] == "contract_ref" {
					r.(map[string]any)["resolution"] = "syntax"
				}
			}
		}, nil},
		// --- plans ---
		{"remove-probabilistic", "recommendation.json", "schema", "plan", func(d M) {
			p := obj(d, "payload", "plans", 0)
			p["transformation"] = "REMOVE_CANDIDATE"
			p["authorization_basis"] = "probabilistic"
		}, nil},
		{"remove-without-owner", "recommendation.json", "schema", "plan", func(d M) {
			p := obj(d, "payload", "plans", 0)
			p["transformation"] = "REMOVE_CANDIDATE"
			p["required_approvals"] = A{"code_owner"}
		}, nil},
		{"refusal-with-steps", "recommendation.json", "schema", "plan", func(d M) {
			p := obj(d, "payload", "plans", 1)
			p["steps"] = A{plain(obj(d, "payload", "plans", 0, "steps", 1))}
		}, nil},
		{"refactor-without-transformation", "recommendation.json", "schema", "plan", func(d M) {
			obj(d, "payload", "plans", 0)["transformation"] = nil
		}, nil},
		// --- changes ---
		{"move-with-two-targets", "change-set.json", "schema", "change", func(d M) {
			c := obj(d, "payload", "changes", 1)
			c["after"] = append(c["after"].([]any), other)
		}, nil},
		{"ambiguous-without-candidates", "change-set.json", "schema", "change", func(d M) {
			delete(obj(d, "payload", "changes", 3), "candidates")
		}, nil},
		{"matched-without-confidence", "change-set.json", "schema", "change", func(d M) {
			delete(obj(d, "payload", "changes", 0), "match_confidence")
		}, nil},
		// --- envelope and formats ---
		{"unsupported-minor-version", "single-repo.json", "schema", "version", func(d M) { d["hint_version"] = "0.3.0" }, nil},
		{"unknown-document-type", "single-repo.json", "schema", "envelope", func(d M) { d["document_type"] = "repository_dump" }, nil},
		{"free-text-allowed-claim", "single-repo.json", "schema", "envelope", func(d M) { obj(d, "redaction")["free_text"] = "allowed" }, nil},
		{"zero-trace-id", "activity-trace.json", "schema", "format", func(d M) {
			obj(d, "payload", "observations", 2, "correlation")["trace_id"] = strings.Repeat("0", 32)
		}, nil},
		{"timestamp-with-offset", "single-repo.json", "schema", "format", func(d M) { d["created_at"] = "2026-10-11T14:00:00+02:00" }, nil},
		// --- semantic rules ---
		{"digest-mismatch", "single-repo.json", "semantic", hintconf.CodeDigest, func(d M) {
			obj(d, "payload", "entities", 1)["flags"] = A{"test"}
		}, func(d M) M { return d }},
		{"snapshot-digest-mismatch", "single-repo.json", "semantic", hintconf.CodeSnapshotDigest, func(d M) {
			obj(d, "payload", "entities", 1)["flags"] = A{"test"}
		}, resealContent},
		{"unsorted-entities", "single-repo.json", "semantic", hintconf.CodeUnsorted, func(d M) {
			e := obj(d, "payload")["entities"].([]any)
			e[0], e[1] = e[1], e[0]
		}, nil},
		{"duplicate-entity", "single-repo.json", "semantic", hintconf.CodeDuplicate, func(d M) {
			p := obj(d, "payload")
			e := p["entities"].([]any)
			p["entities"] = append(e[:2], append(A{plain(e[1].(map[string]any))}, e[2:]...)...)
		}, nil},
		{"dangling-relation", "single-repo.json", "semantic", hintconf.CodeDangling, func(d M) {
			obj(d, "payload", "relations", 0)["target"] = other
		}, nil},
		{"repository-not-declared", "change-set.json", "semantic", hintconf.CodeRepoUndeclared, func(d M) {
			d["repositories"] = A{}
		}, nil},
		{"record-type-not-declared", "activity-trace.json", "semantic", hintconf.CodeRecordType, func(d M) {
			obj(d, "redaction")["record_types"] = A{"Observation"}
		}, nil},
		{"coarse-with-seconds", "activity-trace.json", "semantic", hintconf.CodeTimeResolution, func(d M) {
			obj(d, "redaction")["profile"] = "coarse"
			obj(d, "redaction")["time_resolution_seconds"] = 60
		}, nil},
		{"aggregate-with-observations", "activity-trace.json", "semantic", hintconf.CodePolicy, func(d M) {
			obj(d, "redaction")["profile"] = "aggregate"
		}, nil},
		{"end-before-start", "activity-trace.json", "semantic", hintconf.CodeTimeOrder, func(d M) {
			obj(d, "payload", "observations", 2, "time")["end"] = "2026-10-11T11:00:01Z"
		}, nil},
		{"source-not-declared", "activity-trace.json", "semantic", hintconf.CodeDangling, func(d M) {
			obj(d, "payload", "observations", 0)["source_id"] = other
		}, nil},
		{"metric-input-dangling", "activity-trace.json", "semantic", hintconf.CodeDangling, func(d M) {
			m := obj(d, "payload", "metrics", 0)
			m["inputs"] = append(m["inputs"].([]any), other)
		}, nil},
		{"partial-with-full-coverage", "activity-trace.json", "semantic", hintconf.CodeCoverage, func(d M) {
			obj(d, "payload", "metrics", 0, "value", "coverage")["covered"] = 3
		}, nil},
		{"step-cycle", "recommendation.json", "semantic", hintconf.CodeStepGraph, func(d M) {
			obj(d, "payload", "plans", 0, "steps", 0)["depends_on"] = A{2}
		}, nil},
		{"step-target-outside-plan", "recommendation.json", "semantic", hintconf.CodeDangling, func(d M) {
			obj(d, "payload", "plans", 0, "steps", 0)["targets"] = A{other}
		}, nil},
		{"plan-namespace-mismatch", "recommendation.json", "semantic", hintconf.CodeNamespace, func(d M) {
			obj(d, "payload", "plans", 0)["project_namespace"] = hintconf.NamespaceID(K2)
		}, nil},
		{"interval-order", "recommendation.json", "semantic", hintconf.CodeInterval, func(d M) {
			obj(d, "payload", "plans", 0, "predictions", 0)["interval"] = interval(-1500, -6000, 0.9)
		}, nil},
		{"expires-before-created", "recommendation.json", "semantic", hintconf.CodeTimeOrder, func(d M) {
			obj(d, "payload", "plans", 0)["expires_at"] = "2026-10-10T12:00:00Z"
		}, nil},
		{"plan-snapshot-missing", "recommendation.json", "semantic", hintconf.CodeSnapshotSet, planSnapshotMissing, nil},
		{"plan-snapshot-repeated", "recommendation.json", "semantic", hintconf.CodeSnapshotSet, planSnapshotRepeated, nil},
		{"plan-snapshot-extra", "recommendation.json", "semantic", hintconf.CodeSnapshotSet, planSnapshotExtra, nil},
	}
	expected := M{}
	cases = append(cases, v02Invalid()...)
	for _, c := range cases {
		d := clone(docs[c.base])
		c.mutate(d)
		if c.seal == nil {
			d = reseal(d)
		} else {
			d = c.seal(d)
		}
		write(conf(filepath.Join("invalid", c.name+".json")), d)
		expected[c.name+".json"] = M{"level": c.level, "code": c.code, "base": c.base}
	}
	write(conf("invalid/expected.json"), expected)
}

// otherRepo is a second repository of the same namespace for the plan snapshot set cases.
var otherRepo = hintconf.RepoID(K1, "storefront")

// planSnapshotMissing adds a second plan repository without a snapshot.
func planSnapshotMissing(d M) {
	d["repositories"] = append(d["repositories"].([]any), otherRepo)
	p := obj(d, "payload", "plans", 0)
	p["repo_ids"] = append(p["repo_ids"].([]any), otherRepo)
}

// planSnapshotRepeated gives the plan repository a second snapshot.
func planSnapshotRepeated(d M) {
	p := obj(d, "payload", "plans", 0)
	s := plain(p["snapshots"].([]any)[0])
	s["snapshot_id"] = id("snapshot", "second")
	p["snapshots"] = append(p["snapshots"].([]any), s)
}

// planSnapshotExtra adds a snapshot of a declared repository that the plan does not name.
func planSnapshotExtra(d M) {
	d["repositories"] = append(d["repositories"].([]any), otherRepo)
	p := obj(d, "payload", "plans", 0)
	s := plain(p["snapshots"].([]any)[0])
	s["repo_id"], s["snapshot_id"] = otherRepo, id("snapshot", otherRepo, "c1")
	p["snapshots"] = append(p["snapshots"].([]any), s)
}

// ---------------------------------------------------------------------------
// client import (resolution) fixtures
// ---------------------------------------------------------------------------
func writeResolve(conf func(string) string, rec, snap M) {
	repoID := hintconf.RepoID(K1, "shopkit")
	digest := obj(snap, "payload", "snapshots", 0)["digest"]
	var ids []string
	for _, e := range obj(snap, "payload")["entities"].([]any) {
		ids = append(ids, e.(map[string]any)["entity_id"].(string))
	}
	sort.Strings(ids)
	local := M{"supported_versions": A{"0.1"}, "project_namespace": hintconf.NamespaceID(K1),
		"now": "2026-10-12T00:00:00Z", "snapshot_digests": M{repoID: digest}, "resolvable": ids, "ambiguous": A{}}
	write(conf("resolve/local-state.json"), local)
	amb := plain(local)
	amb["ambiguous"] = A{hintconf.SymbolID(K1, repoID, "go", "shopkit/billing.Rate")}
	write(conf("resolve/local-state-ambiguous.json"), amb)

	type rc struct {
		name, local string
		mutate      func(d M)
		seal        bool
		expected    M
	}
	p1, p2, p3 := id("plan", "split-ledger"), id("plan", "graph-context"), id("plan", "collect-evidence")
	pr := func(pid, status string, codes ...string) M {
		c := A{}
		for _, x := range codes {
			c = append(c, x)
		}
		return M{"plan_id": pid, "status": status, "codes": c}
	}
	okRest := A{pr(p2, "render_only"), pr(p3, "render_only")}
	cases := []rc{
		{"accept", "local-state.json", func(d M) {}, true, M{"document": "accepted", "plans": append(A{pr(p1, "accept")}, okRest...)}},
		{"expired", "local-state.json", func(d M) {
			obj(d, "payload", "plans", 0)["expires_at"] = "2026-10-11T18:00:00Z"
		}, true, M{"document": "accepted", "plans": append(A{pr(p1, "reject", "expired")}, okRest...)}},
		{"snapshot-mismatch", "local-state.json", func(d M) {
			p := obj(d, "payload", "plans", 0)
			p["snapshots"] = A{M{"repo_id": repoID, "snapshot_id": id("snapshot", repoID, "c0"), "digest": "sha256:" + strings.Repeat("ab", 32)}}
		}, true, M{"document": "accepted", "plans": append(A{pr(p1, "reject", "snapshot_mismatch")}, okRest...)}},
		{"unresolved-id", "local-state.json", func(d M) {
			p := obj(d, "payload", "plans", 0)
			p["targets"] = append(p["targets"].([]any), id("entity", "not-local"))
		}, true, M{"document": "accepted", "plans": append(A{pr(p1, "reject", "unresolved_id")}, okRest...)}},
		{"ambiguous-id", "local-state-ambiguous.json", func(d M) {}, true,
			M{"document": "accepted", "plans": append(A{pr(p1, "reject", "ambiguous_id")}, okRest...)}},
		{"unsupported-step", "local-state.json", func(d M) {
			obj(d, "payload", "plans", 0, "steps", 0)["support"] = "unsupported"
		}, true, M{"document": "accepted", "plans": append(A{pr(p1, "partial", "unsupported_step")}, okRest...)}},
		{"namespace-mismatch", "local-state.json", func(d M) {
			ns := hintconf.NamespaceID(K2)
			d["project_namespace"] = ns
			for _, p := range obj(d, "payload")["plans"].([]any) {
				p.(map[string]any)["project_namespace"] = ns
			}
		}, true, M{"document": hintconf.CodeNamespace, "plans": nil}},
		{"unsupported-version", "local-state.json", func(d M) { d["hint_version"] = "0.2.0" }, true,
			M{"document": "unsupported_version", "plans": nil}},
		{"plan-snapshot-missing", "local-state.json", planSnapshotMissing, true,
			M{"document": hintconf.CodeSnapshotSet, "plans": nil}},
		{"plan-snapshot-repeated", "local-state.json", planSnapshotRepeated, true,
			M{"document": hintconf.CodeSnapshotSet, "plans": nil}},
		{"plan-snapshot-extra", "local-state.json", planSnapshotExtra, true,
			M{"document": hintconf.CodeSnapshotSet, "plans": nil}},
		{"tampered", "local-state.json", func(d M) {
			obj(d, "payload", "plans", 0)["decision"] = "recommend"
			obj(d, "payload", "plans", 1)["intervention_class"] = "COLLECT_MORE_EVIDENCE"
		}, false, M{"document": hintconf.CodeDigest, "plans": nil}},
	}
	expected := M{}
	for _, c := range cases {
		d := clone(rec)
		c.mutate(d)
		if c.seal {
			d = reseal(d)
		}
		write(conf(filepath.Join("resolve", "cases", c.name+".json")), d)
		expected[c.name+".json"] = M{"local": c.local, "expected": c.expected}
	}
	write(conf("resolve/expected.json"), expected)
}

// ---------------------------------------------------------------------------
// test vectors
// ---------------------------------------------------------------------------
func writeVectors(conf func(string) string) {
	type vec struct {
		key, domain string
		parts       []string
	}
	r1 := hintconf.RepoID(K1, "shopkit")
	vs := []vec{
		{"K1", "namespace", nil}, {"K2", "namespace", nil},
		{"K1", "repo", []string{"shopkit"}}, {"K2", "repo", []string{"shopkit"}}, {"K1", "repo", []string{"shop-kit"}},
		{"K1", "file", []string{r1, "billing/ledger.go"}}, {"K1", "file", []string{r1, "billing/ledger_core.go"}},
		{"K1", "symbol", []string{r1, "go", "shopkit/billing.Apply"}},
		{"K1", "contract", []string{"idl_service", "shopkit.billing.v1.BillingService"}},
		{"K2", "contract", []string{"idl_service", "shopkit.billing.v1.BillingService"}},
	}
	keys := map[string][]byte{"K1": K1, "K2": K2}
	out := A{}
	for _, v := range vs {
		parts := A{}
		for _, p := range v.parts {
			parts = append(parts, p)
		}
		out = append(out, M{"key": v.key, "domain": v.domain, "parts": parts,
			"id": hintconf.DeriveID(keys[v.key], v.domain, v.parts...)})
	}
	// trace and span IDs (hint-data §4.1): truncated to 16 and 8 bytes
	tr, tr2, sp := "4bf92f3577b34da6a3ce929d0e0e4736", "5bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, v := range []struct {
		key        string
		parts      []string
		trace, out string
	}{{"K1", []string{tr}, "trace", ""}, {"K2", []string{tr}, "trace", ""},
		{"K1", []string{tr, sp}, "span", ""}, {"K2", []string{tr, sp}, "span", ""}, {"K1", []string{tr2, sp}, "span", ""}} {
		var got string
		var ok bool
		n := 16
		if v.trace == "trace" {
			got, ok = hintconf.TraceID(keys[v.key], v.parts[0])
		} else {
			got, ok = hintconf.SpanID(keys[v.key], v.parts[0], v.parts[1])
			n = 8
		}
		if !ok {
			panic("trace vector")
		}
		parts := A{}
		for _, p := range v.parts {
			parts = append(parts, p)
		}
		out = append(out, M{"key": v.key, "domain": v.trace, "parts": parts, "bytes": n, "id": got})
	}
	// observation_id of a span: the trace-id is part of the input (hint-data §4.1)
	src := id("source", "otlp-receiver")
	for _, t := range []string{tr, tr2} {
		out = append(out, M{"key": "K1", "domain": "event", "parts": A{src, t, sp},
			"id": hintconf.SpanObservationID(K1, src, t, sp)})
	}
	write(conf("vectors/ids-v0.1.json"), M{
		"note": "Test keys only. Real project keys are random 32-byte secrets kept in the local store.",
		"keys": M{"K1": hex.EncodeToString(K1), "K2": hex.EncodeToString(K2)}, "vectors": out})

	values := A{
		M{"b": 2, "a": 1},
		M{"interval": M{"lower": -6000, "upper": -1500, "level": 0.9}, "point": -3200},
		A{1e21, 0.1, 1, -0.0, 123456789.125},
		M{"state": "known", "unit": "bytes", "value": 0, "precision": "exact"},
	}
	dv := A{}
	for _, v := range values {
		cb, err := hintconf.Canonical(v)
		check(err)
		dg, err := hintconf.Digest(v)
		check(err)
		dv = append(dv, M{"value": v, "canonical": string(cb), "digest": dg})
	}
	write(conf("vectors/digests-v0.1.json"), M{"vectors": dv})
}
