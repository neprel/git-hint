package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

// ledgerInputJSON is one runner hand-over: a complete ledger of 2 requests, or (late > 0) the
// revision after late requests.
func ledgerInputJSON(revision, late int) string {
	counts := map[string]any{"complete": 2}
	state, reasons := "complete", []string{}
	if late > 0 {
		counts["late"] = late
		state, reasons = "incomplete", []string{"late_requests"}
	}
	in := map[string]any{"trial": "trial-1", "run": "run-1", "eval": "eval-1", "attempt": 1, "task": "task-1", "family": "refactor", "agent": "opencode",
		"configuration": map[string]any{"agent": map[string]any{"name": "opencode", "version": map[string]any{"state": "known", "value": "2.0.26"}},
			"model":          map[string]any{"state": "known", "ref": strings.Repeat("a", 32), "provenance": "launch_parameter"},
			"harness":        map[string]any{"kind": "harbor", "version": map[string]any{"state": "known", "value": "0.23.0"}},
			"context_policy": map[string]any{"state": "disabled"}, "config_refs": map[string]any{"state": "known", "refs": []any{strings.Repeat("b", 32)}},
			"budget": map[string]any{"max_requests": map[string]any{"state": "known", "value": 16}, "max_seconds": map[string]any{"state": "known", "value": 300}}},
		"revision": revision, "sealed_at": "2026-10-19T10:00:00.000Z",
		"report": map[string]any{"state": state, "reasons": reasons, "revision": late, "counts": counts,
			"components": map[string]any{"prompt": 1200, "completion": 80, "cached": 900}, "components_n": map[string]any{"prompt": 2, "completion": 2, "cached": 2},
			"total_complete": late == 0, "normalization": "openai-compat-usage-v1", "model_check": "verified", "bypass": "enforced", "scope": "isolated_container"}}
	b, _ := json.Marshal(in)
	return string(b)
}

// The runner's sealed ledger enters the spool as a draft v0.2 USAGE record with unknown
// canonical usage; replays write nothing; a late revision follows the exported one and the
// earlier record stays as exported; a skipped revision, a second body and an inconsistent
// account are refused.
func TestAdapterLedgerRevisionsAndExport(t *testing.T) {
	shop, _ := setup(t)
	must(t, run(t, shop, ledgerInputJSON(0, 0), "adapter", "ledger"))
	if r := must(t, run(t, shop, ledgerInputJSON(0, 0), "adapter", "ledger")); !strings.Contains(r.stdout, "already written") {
		t.Fatalf("replay: %s", r.stdout)
	}
	if r := run(t, shop, ledgerInputJSON(2, 1), "adapter", "ledger"); r.code == 0 {
		t.Fatal("a skipped revision was accepted")
	}
	if r := run(t, shop, strings.Replace(ledgerInputJSON(0, 0), `"prompt":1200`, `"prompt":1300`, 1), "adapter", "ledger"); r.code == 0 {
		t.Fatal("a second body for revision 0 was accepted")
	}
	if r := run(t, shop, strings.Replace(ledgerInputJSON(0, 0), `"components_n":{"cached":2,"completion":2,"prompt":2}`, `"components_n":{"cached":2,"completion":1,"prompt":2}`, 1), "adapter", "ledger"); r.code == 0 {
		t.Fatal("an inconsistent account was accepted")
	}
	d1 := filepath.Join(t.TempDir(), "first")
	must(t, run(t, shop, "", "export", "--out", d1))
	first := ledgerRecords(t, d1)
	if len(first) != 1 {
		t.Fatalf("first export: %d ledger records", len(first))
	}
	must(t, run(t, shop, ledgerInputJSON(1, 1), "adapter", "ledger")) // a late request after the seal
	d2 := filepath.Join(t.TempDir(), "second")
	must(t, run(t, shop, "", "export", "--out", d2))
	second := ledgerRecords(t, d2)
	if len(second) != 1 || second[0]["model_ledger"].(map[string]any)["revision"] != 1.0 || second[0]["model_ledger"].(map[string]any)["state"] != "incomplete" {
		t.Fatalf("second export: %v", second)
	}
	if first[0]["observation_id"] == second[0]["observation_id"] {
		t.Fatal("a revision reused the record ID")
	}
	u := first[0]["usage"].(map[string]any)
	if u["input_tokens"].(map[string]any)["state"] != "unknown" || u["output_tokens"].(map[string]any)["state"] != "unknown" {
		t.Fatalf("canonical usage of a ledger record: %v", u)
	}
	ml := first[0]["model_ledger"].(map[string]any)
	if ml["model_check"] != "requested_model_observed" || ml["components"].(map[string]any)["reasoning"] != nil || ml["component_coverage"].(map[string]any)["reasoning"] != 0.0 {
		t.Fatalf("ledger: %v", ml)
	}
	if b, _ := os.ReadFile(filepath.Join(d1, firstActivity(t, d1))); strings.Contains(string(b), "trial-1") || strings.Contains(string(b), "run-1") || strings.Contains(string(b), "task-1") {
		t.Fatal("a raw local name reached the export")
	}
}

func firstActivity(t *testing.T, dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "activity_stream-*.json"))
	if len(files) != 1 {
		t.Fatalf("%d activity documents", len(files))
	}
	return filepath.Base(files[0])
}

// ledgerRecords are the ledger USAGE records of the exported activity document, which must be a
// valid draft v0.2 document.
func ledgerRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, firstActivity(t, dir)))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["hint_version"] != "0.2.0" {
		t.Fatalf("version %v", doc["hint_version"])
	}
	if fs := hint.Check(doc, nil); len(fs) > 0 {
		t.Fatalf("semantic: %v", fs)
	}
	var out []map[string]any
	for _, o := range doc["payload"].(map[string]any)["observations"].([]any) {
		if m := o.(map[string]any); m["model_ledger"] != nil {
			out = append(out, m)
		}
	}
	return out
}

// C7BE-30: the writer refuses a revision that heals or rewrites the sealed ledger, before any
// write (the spool and the sources stay byte-identical).
func TestAdapterLedgerRefusesHealingAndRewriting(t *testing.T) {
	for name, pair := range map[string][2]string{
		"heal":    {ledgerInputJSON(0, 1), ledgerInputJSON(1, 0)},
		"rewrite": {ledgerInputJSON(0, 0), strings.Replace(ledgerInputJSON(1, 1), `"prompt":1200`, `"prompt":101200`, 1)},
	} {
		shop, _ := setup(t)
		must(t, run(t, shop, pair[0], "adapter", "ledger"))
		state := filepath.Join(shop, ".hint", "state", "activity")
		spool, _ := os.ReadFile(filepath.Join(state, "spool.jsonl"))
		sources, _ := os.ReadFile(filepath.Join(state, "sources.json"))
		if r := run(t, shop, pair[1], "adapter", "ledger"); r.code == 0 || !strings.Contains(r.stderr, "heals") {
			t.Fatalf("%s: accepted: %s %s", name, r.stdout, r.stderr)
		}
		spool2, _ := os.ReadFile(filepath.Join(state, "spool.jsonl"))
		sources2, _ := os.ReadFile(filepath.Join(state, "sources.json"))
		if string(spool) != string(spool2) || string(sources) != string(sources2) {
			t.Fatalf("%s: a refused revision changed local state", name)
		}
	}
}
