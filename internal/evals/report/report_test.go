package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/hint"
)

var fixedKey = []byte("synthetic-fixture-key-not-secret")

const fixedNS = "0123456789abcdef0123456789abcdef"

// seeded builds a library with a validated eval, a suite and three trials.
func seeded(t *testing.T) (*library.Library, []string) {
	t.Helper()
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	l.PutManifest("eval", v2)
	suite, _, err := suites.Freeze(l, nil, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	ten, five, two := int64(10), int64(400), int64(2)
	wall := 95.5
	one, zero := 1.0, 0.0
	in := suites.TrialInput{Suite: suite, Eval: fmt.Sprintf("%s@2", rec["eval_id"]), Arm: "baseline", Agent: "opencode", AgentVersion: "2.0.26",
		Model: "local-coder-7b", RunnerVersion: "0.23.0", MaxSeconds: 900, MaxRequests: 40, Requests: &ten, Retries: &two}
	var names []string
	for i, tr := range []runner.Trial{
		{Name: "a", Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: &five, OutputTokens: &ten, WallSeconds: &wall},
		{Name: "b", Reward: &zero, Checks: map[string]string{"C1": "fail", "C2": "pass"}, InputTokens: &five, OutputTokens: &ten, WallSeconds: &wall},
		{Name: "c", Exception: "AgentTimeoutError", Checks: map[string]string{}},
	} {
		x := in
		x.Attempt, x.Started = i+1, time.Date(2026, 10, 18, 9, i, 0, 0, time.UTC)
		n, err := suites.RecordTrial(l, x, tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return l, names
}

func build(t *testing.T) ([]byte, map[string]any) {
	t.Helper()
	l, names := seeded(t)
	b, used, err := Builder{Key: fixedKey, Namespace: fixedNS}.Build(l, names, time.Date(2026, 10, 18, 9, 41, 7, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 3 {
		t.Fatalf("used %v", used)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return b, m
}

func reseal(m map[string]any) []byte {
	delete(m, "content_digest")
	d, _ := hint.Digest(m)
	m["content_digest"] = d
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var c map[string]any
	json.Unmarshal(b, &c)
	return c
}

func variants(base map[string]any) map[string][]byte {
	tr := func(m map[string]any) map[string]any { return m["trials"].([]any)[0].(map[string]any) }
	out := map[string][]byte{}
	mk := func(name string, f func(m map[string]any), seal bool) {
		m := clone(base)
		f(m)
		if seal {
			out[name] = reseal(m)
		} else {
			b, _ := json.MarshalIndent(m, "", "  ")
			out[name] = b
		}
	}
	mk("unknown-field", func(m map[string]any) { tr(m)["note"] = "fix the login bug" }, true)
	mk("instruction-text", func(m map[string]any) { m["instruction"] = "Total ignores the discount" }, true)
	mk("path-as-model-ref", func(m map[string]any) { tr(m)["agent"].(map[string]any)["model_ref"] = "/home/dev/models/coder" }, true)
	mk("local-eval-id", func(m map[string]any) { tr(m)["eval_ref"] = "ev-0123456789abcdef@2" }, true)
	mk("content-digest-in-check", func(m map[string]any) {
		tr(m)["checks"].([]any)[0].(map[string]any)["check_ref"] = "sha256:" + strings.Repeat("a", 64)
	}, true)
	mk("wrong-digest", func(m map[string]any) { tr(m)["attempt"] = 7 }, false)
	mk("duplicate-trial", func(m map[string]any) { m["trials"] = append(m["trials"].([]any), m["trials"].([]any)[0]) }, true)
	mk("unknown-as-text", func(m map[string]any) { tr(m)["counters"].(map[string]any)["input_tokens"] = "unknown" }, true)
	mk("minute-resolution", func(m map[string]any) { m["created_at"] = "2026-10-18T09:41:00Z" }, true)
	mk("future-version", func(m map[string]any) { m["report_version"] = "0.3.0" }, true)
	mk("command-field", func(m map[string]any) { tr(m)["command"] = "go test ./..." }, true)
	mk("free-outcome", func(m map[string]any) { tr(m)["outcome"] = "mostly passed" }, true)
	mk("negative-tokens", func(m map[string]any) { tr(m)["counters"].(map[string]any)["output_tokens"] = -1 }, true)
	// v0.2: the context policy state, declared changes and observed config
	ref := strings.Repeat("c", 32)
	mk("context-policy-missing", func(m map[string]any) { delete(tr(m), "context_policy") }, true)
	mk("context-enabled-without-ref", func(m map[string]any) { tr(m)["context_policy"] = map[string]any{"state": "enabled", "version": 1} }, true)
	mk("context-disabled-with-ref", func(m map[string]any) { tr(m)["context_policy"] = map[string]any{"state": "disabled", "ref": ref} }, true)
	mk("context-unknown-with-version", func(m map[string]any) { tr(m)["context_policy"] = map[string]any{"state": "unknown", "version": 2} }, true)
	mk("context-policy-name", func(m map[string]any) {
		tr(m)["context_policy"] = map[string]any{"state": "enabled", "ref": "default-context-policy", "version": 1}
	}, true)
	mk("undeclared-change-kind", func(m map[string]any) { tr(m)["declared_changes"] = []any{"temperature 0.7"} }, true)
	mk("observed-config-raw", func(m map[string]any) { tr(m)["observed_config_refs"] = []any{"max_tokens=4096"} }, true)
	return out
}

// HINT_WRITE_FIXTURES=1 regenerates tests/evalreport (synthetic, fixed key).
func TestFixtures(t *testing.T) {
	b, m := build(t)
	dir := filepath.Join("..", "..", "..", "tests", "evalreport")
	if os.Getenv("HINT_WRITE_FIXTURES") == "1" {
		// v0.2 fixtures; the committed v0.1 fixtures stay as they are
		os.WriteFile(filepath.Join(dir, "valid", "report-v02-three-trials.json"), append(b, '\n'), 0o644)
		enabled := clone(m)
		tr := enabled["trials"].([]any)
		tr[1].(map[string]any)["context_policy"] = map[string]any{"state": "enabled", "ref": strings.Repeat("d", 32), "version": 3}
		tr[1].(map[string]any)["declared_changes"] = []any{"context_policy"}
		tr[2].(map[string]any)["context_policy"] = map[string]any{"state": "not_applicable"}
		os.WriteFile(filepath.Join(dir, "valid", "report-v02-context-states.json"), append(reseal(enabled), '\n'), 0o644)
		for name, v := range variants(m) {
			os.WriteFile(filepath.Join(dir, "invalid", "v02-"+name+".json"), append(v, '\n'), 0o644)
		}
	}
	valid, _ := filepath.Glob(filepath.Join(dir, "valid", "*.json"))
	invalid, _ := filepath.Glob(filepath.Join(dir, "invalid", "*.json"))
	if len(valid) < 2 || len(invalid) < 13 {
		t.Fatalf("fixtures: %d valid, %d invalid", len(valid), len(invalid))
	}
	for _, f := range valid {
		data, _ := os.ReadFile(f)
		if err := Check(data); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
	for _, f := range invalid {
		data, _ := os.ReadFile(f)
		if err := Check(data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", filepath.Base(f))
		}
	}
}

func TestReportHoldsNoLocalContent(t *testing.T) {
	b, m := build(t)
	for _, bad := range []string{"ev-", "su-", "tr-", "tf-", "local-coder", "discount", "sha256:", "cart.go", "/app", "go test"} {
		if strings.Contains(string(b), bad) && !(bad == "sha256:" && strings.Count(string(b), "sha256:") == 1) {
			t.Errorf("report holds %q", bad)
		}
	}
	trs := m["trials"].([]any)
	if len(trs) != 3 {
		t.Fatalf("trials %d", len(trs))
	}
	timeout := trs[2].(map[string]any)
	if timeout["outcome"] != "timeout" || timeout["counters"].(map[string]any)["input_tokens"] != nil {
		t.Fatalf("timeout trial %v", timeout)
	}
	if m["created_at"] != "2026-10-18T09:00:00Z" {
		t.Fatalf("created_at %v", m["created_at"])
	}
	// another project key gives unrelated references
	l, names := seeded(t)
	b2, _, _ := Builder{Key: []byte("another-project-key"), Namespace: fixedNS}.Build(l, names, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	var m2 map[string]any
	json.Unmarshal(b2, &m2)
	if m2["trials"].([]any)[0].(map[string]any)["eval_ref"] == trs[0].(map[string]any)["eval_ref"] {
		t.Fatal("references do not depend on the project key")
	}
}

func TestUnreportedTrials(t *testing.T) {
	l, names := seeded(t)
	got, _ := Unreported(l)
	if len(got) != 3 {
		t.Fatalf("unreported %v", got)
	}
	MarkReported(l, names[:2])
	got, _ = Unreported(l)
	if len(got) != 1 || got[0] != names[2] {
		t.Fatalf("after marking %v", got)
	}
}

// D02: an enabled context policy goes out as one project reference of its full identity
// (id, version, digest) and its version; another digest gives another reference; the other
// states stay distinct; a trial recorded without the field is unknown.
func TestContextPolicyReference(t *testing.T) {
	l, _ := seeded(t)
	suite, _, _ := suites.Freeze(l, nil, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	var evalRef string
	names, _ := l.Manifests("eval")
	for _, n := range names {
		if strings.HasSuffix(n, "@2") {
			evalRef = n
		}
	}
	one := 1.0
	record := func(i int, in suites.TrialInput) string {
		in.Suite, in.Eval, in.Arm, in.Agent, in.RunnerVersion, in.MaxSeconds = suite, evalRef, "baseline", "oracle", "0.23.0", 900
		in.Attempt, in.Started = 10+i, time.Date(2026, 10, 18, 10, i, 0, 0, time.UTC)
		n, err := suites.RecordTrial(l, in, runner.Trial{Name: fmt.Sprint("p", i), Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	ns := []string{
		record(0, suites.TrialInput{ContextPolicy: "enabled", ContextPolicyID: "default", ContextPolicyVersion: 1, ContextPolicyDigest: "sha256:" + strings.Repeat("a", 64)}),
		record(1, suites.TrialInput{ContextPolicy: "enabled", ContextPolicyID: "default", ContextPolicyVersion: 1, ContextPolicyDigest: "sha256:" + strings.Repeat("a", 64)}),
		record(2, suites.TrialInput{ContextPolicy: "enabled", ContextPolicyID: "default", ContextPolicyVersion: 1, ContextPolicyDigest: "sha256:" + strings.Repeat("b", 64)}),
		record(3, suites.TrialInput{ContextPolicy: "disabled"}),
		record(4, suites.TrialInput{ContextPolicy: "not_applicable", DeclaredChanges: []string{"context_policy"}}),
		record(5, suites.TrialInput{}),
	}
	b, _, err := Builder{Key: fixedKey, Namespace: fixedNS}.Build(l, ns, time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	cp := map[int]map[string]any{}
	for _, x := range m["trials"].([]any) {
		tr := x.(map[string]any)
		cp[int(tr["attempt"].(float64))-10] = tr["context_policy"].(map[string]any)
		if tr["attempt"].(float64) == 14 && fmt.Sprint(tr["declared_changes"]) != "[context_policy]" {
			t.Fatalf("declared changes %v", tr["declared_changes"])
		}
	}
	if cp[0]["ref"] == nil || cp[0]["ref"] != cp[1]["ref"] || cp[0]["ref"] == cp[2]["ref"] || cp[0]["version"] != 1.0 {
		t.Fatalf("enabled references %v %v %v", cp[0], cp[1], cp[2])
	}
	for i, want := range map[int]string{3: "disabled", 4: "not_applicable", 5: "unknown"} {
		if cp[i]["state"] != want || cp[i]["ref"] != nil {
			t.Fatalf("%d: %v", i, cp[i])
		}
	}
	if strings.Contains(string(b), "default") || strings.Contains(string(b), strings.Repeat("a", 64)) {
		t.Fatal("the policy id or digest left the machine")
	}
}
