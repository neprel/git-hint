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
	mk("future-version", func(m map[string]any) { m["report_version"] = "0.2.0" }, true)
	mk("command-field", func(m map[string]any) { tr(m)["command"] = "go test ./..." }, true)
	mk("free-outcome", func(m map[string]any) { tr(m)["outcome"] = "mostly passed" }, true)
	mk("negative-tokens", func(m map[string]any) { tr(m)["counters"].(map[string]any)["output_tokens"] = -1 }, true)
	return out
}

// HINT_WRITE_FIXTURES=1 regenerates tests/evalreport (synthetic, fixed key).
func TestFixtures(t *testing.T) {
	b, m := build(t)
	dir := filepath.Join("..", "..", "..", "tests", "evalreport")
	if os.Getenv("HINT_WRITE_FIXTURES") == "1" {
		os.WriteFile(filepath.Join(dir, "valid", "report-three-trials.json"), append(b, '\n'), 0o644)
		empty := clone(m)
		empty["trials"] = []any{}
		empty["report_id"] = strings.Repeat("e", 32)
		os.WriteFile(filepath.Join(dir, "valid", "report-empty.json"), append(reseal(empty), '\n'), 0o644)
		for name, v := range variants(m) {
			os.WriteFile(filepath.Join(dir, "invalid", name+".json"), append(v, '\n'), 0o644)
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
