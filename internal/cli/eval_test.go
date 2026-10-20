package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/store"
)

// noAgents answers like a machine without agent CLIs.
type noAgents struct{ calls []string }

func (n *noAgents) Output(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	n.calls = append(n.calls, name+" "+strings.Join(args, " "))
	return nil, errors.New("not installed")
}

func runEval(t *testing.T, dir string, spawned *[]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	home := filepath.Join(dir, ".test-home")
	env := &Env{Dir: dir, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Spawn: func(a ...string) error { *spawned = append(*spawned, strings.Join(a, " ")); return nil },
		EvalEnv: func() pipeline.Env {
			return pipeline.Env{Home: home, DataHome: filepath.Join(home, "data"), Cmd: &noAgents{}, Now: time.Now}
		}}
	code := Run(args, env)
	return result{code, out.String(), errb.String()}
}

func config(t *testing.T, dir string) map[string]any {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.Cfg)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestEvalOffByDefault(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	r := must(t, runEval(t, shop, &spawned, "collect"))
	if strings.Contains(r.stdout, "Evals:") || len(spawned) != 0 {
		t.Fatalf("collect without a policy ran the eval pipeline:\n%s", r.stdout)
	}
	if _, ok := config(t, shop)["evals"]; ok {
		t.Fatal("init or collect wrote an eval policy")
	}
	if out := must(t, run(t, shop, "", "status")).stdout; !strings.Contains(out, "Evals: off") {
		t.Fatalf("status: %s", out)
	}
	if _, err := os.Stat(filepath.Join(shop, ".hint", "state", "evals")); err == nil {
		t.Fatal("eval library created while off")
	}
}

func TestEvalOnCollectOffForget(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	r := must(t, runEval(t, shop, &spawned, "eval", "on"))
	if !strings.Contains(r.stdout, "Container runs: off") || !strings.Contains(r.stdout, "nothing is sent") {
		t.Fatalf("eval on: %s", r.stdout)
	}
	ev := config(t, shop)["evals"].(map[string]any)
	if ev["capture"] != "on" || ev["run"].(map[string]any)["use"] != "off" || ev["model"].(map[string]any)["use"] != "off" {
		t.Fatalf("policy %v", ev)
	}
	r = must(t, runEval(t, shop, &spawned, "collect"))
	if !strings.Contains(r.stdout, "Evals: 0 sessions imported") || len(spawned) != 0 {
		t.Fatalf("collect with policy: %s spawned %v", r.stdout, spawned)
	}
	if out := must(t, runEval(t, shop, &spawned, "eval")).stdout; !strings.Contains(out, "No evals yet") {
		t.Fatalf("eval list: %s", out)
	}
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	if ev := config(t, shop)["evals"].(map[string]any); ev["run"].(map[string]any)["use"] != "docker" {
		t.Fatalf("run not enabled: %v", ev)
	}
	if r := runEval(t, shop, &spawned, "eval", "on", "--run", "host"); r.code != 2 {
		t.Fatal("a host runner was accepted")
	}
	must(t, runEval(t, shop, &spawned, "eval", "off"))
	if ev := config(t, shop)["evals"].(map[string]any); ev["capture"] != "off" {
		t.Fatal("eval off kept capture on")
	}
	r = must(t, runEval(t, shop, &spawned, "collect"))
	if strings.Contains(r.stdout, "Evals:") {
		t.Fatal("collect ran the pipeline after eval off")
	}
	r = must(t, runEval(t, shop, &spawned, "eval", "forget", "--all"))
	if !strings.Contains(r.stdout, "Deleted") {
		t.Fatalf("forget: %s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(shop, ".hint", "state", "project.json")); err != nil {
		t.Fatal("forget --all removed project state")
	}
}

func TestEvalHelpNamesTheBenefit(t *testing.T) {
	r := run(t, t.TempDir(), "", "help")
	if !strings.Contains(r.stdout, "eval") || !strings.Contains(r.stdout, "measure agent efficiency") {
		t.Fatalf("help: %s", r.stdout)
	}
	var spawned []string
	r = runEval(t, t.TempDir(), &spawned, "eval", "help")
	if !strings.Contains(r.stderr, "before and after a refactoring") {
		t.Fatalf("eval help: %s", r.stderr)
	}
}

func TestCollectStartsTheWorkerOnlyWhenRunsAreAllowed(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	s, err := store.Open(shop)
	if err != nil {
		t.Fatal(err)
	}
	l := library.Open(s, 0)
	if _, err := l.PutManifest("eval", evaltest.GoBugFix(t, l)); err != nil {
		t.Fatal(err)
	}
	r := must(t, runEval(t, shop, &spawned, "collect"))
	if len(spawned) != 0 || !strings.Contains(r.stdout, "1 waiting for qualification") {
		t.Fatalf("runs off: %s %v", r.stdout, spawned)
	}
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	r = must(t, runEval(t, shop, &spawned, "collect"))
	if len(spawned) != 1 || spawned[0] != "eval __qualify" || !strings.Contains(r.stdout, "runs in the background") {
		t.Fatalf("runs on: %s %v", r.stdout, spawned)
	}
	out := must(t, runEval(t, shop, &spawned, "eval")).stdout
	if !strings.Contains(out, "packaged") {
		t.Fatalf("list: %s", out)
	}
	name := strings.Fields(strings.SplitN(out, "\n", 3)[1])[0]
	if show := must(t, runEval(t, shop, &spawned, "eval", "show", name, "--text")).stdout; !strings.Contains(show, "Instruction:") || !strings.Contains(show, "subtracts the discount") {
		t.Fatalf("show: %s", show)
	}
}

func TestPackAndUnpackBetweenProjects(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	name, err := l.PutManifest("eval", evaltest.GoBugFix(t, l))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "evals.bundle")
	id, _, _ := strings.Cut(name, "@")
	must(t, runEval(t, shop, &spawned, "eval", "pack", id, "-o", file))
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle file %v %v", err, fi)
	}
	if r := runEval(t, shop, &spawned, "eval", "pack", id, "-o", file); r.code == 0 {
		t.Fatal("pack overwrote an existing file")
	}
	other, _ := setup(t)
	r := must(t, runEval(t, other, &spawned, "eval", "unpack", file))
	if !strings.Contains(r.stdout, name) {
		t.Fatalf("unpack: %s", r.stdout)
	}
	if out := must(t, runEval(t, other, &spawned, "eval")).stdout; !strings.Contains(out, name) {
		t.Fatalf("list after unpack: %s", out)
	}
}

func TestEvalReportFileAndConsentedUpload(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	l.PutManifest("eval", v2)
	suite, _, err := suites.Freeze(l, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reward := 1.0
	if _, err := suites.RecordTrial(l, suites.TrialInput{Suite: suite, Eval: fmt.Sprintf("%s@2", rec["eval_id"]), Attempt: 1, Arm: "baseline", Agent: "oracle",
		RunnerVersion: "0.23.0", Started: time.Now(), MaxSeconds: 600}, runner.Trial{Name: "a", Reward: &reward, Checks: map[string]string{"C1": "pass"}}); err != nil {
		t.Fatal(err)
	}

	var got [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, b)
		var m map[string]any
		json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/vnd.hint-evalreport+json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"report_version": "0.1.0", "report_id": m["report_id"], "content_digest": m["content_digest"], "status": "accepted"})
	}))
	defer srv.Close()

	r := must(t, runEval(t, shop, &spawned, "eval", "report"))
	if !strings.Contains(r.stdout, "1 trial results") || len(got) != 0 {
		t.Fatalf("report without send: %s (%d uploads)", r.stdout, len(got))
	}
	if r := runEval(t, shop, &spawned, "eval", "report", "--send", "--yes", "--server", srv.URL); r.code == 0 || !strings.Contains(r.stderr, "result export is off") {
		t.Fatalf("send without the export permission: %d %s", r.code, r.stderr)
	}
	s, _ = store.Open(shop)
	s.Cfg.Evals.Export = "results"
	s.SaveConfig()
	if r := runEval(t, shop, &spawned, "eval", "report", "--send", "--server", srv.URL); r.code == 0 || !strings.Contains(r.stderr, "needs approval") || len(got) != 0 {
		t.Fatalf("send without consent: %d %s", r.code, r.stderr)
	}
	must(t, runEval(t, shop, &spawned, "eval", "report", "--send", "--yes", "--server", srv.URL))
	if len(got) != 1 {
		t.Fatalf("uploads %d", len(got))
	}
	if err := report.Check(got[0]); err != nil {
		t.Fatalf("uploaded report: %v", err)
	}
	for _, bad := range []string{"discount", "ev-", "cart.go", "oracle.txt"} {
		if strings.Contains(string(got[0]), bad) {
			t.Fatalf("uploaded report holds %q", bad)
		}
	}
	if r := must(t, runEval(t, shop, &spawned, "eval", "report", "--send", "--yes", "--server", srv.URL)); !strings.Contains(r.stdout, "No new trial results") {
		t.Fatalf("second report: %s", r.stdout)
	}
}

func TestPinKeepsAnEvalThroughRetention(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	rec["created_at"] = "2025-01-01T00:00:00Z" // older than the retention period
	name, err := l.PutManifest("eval", rec)
	if err != nil {
		t.Fatal(err)
	}
	must(t, runEval(t, shop, &spawned, "eval", "pin", strings.SplitN(name, "@", 2)[0]))
	must(t, runEval(t, shop, &spawned, "collect")) // the pass prunes unpinned old records
	if names, _ := l.Manifests("eval"); len(names) != 1 {
		t.Fatal("a pinned eval was pruned")
	}
	must(t, runEval(t, shop, &spawned, "eval", "pin", name, "--off"))
	must(t, runEval(t, shop, &spawned, "collect"))
	if names, _ := l.Manifests("eval"); len(names) != 0 {
		t.Fatalf("an unpinned old eval survived retention: %v", names)
	}
}

// Q-10-2: git hint eval map prints the mapping file; --apply stores a new packaged
// version and starts the qualification worker only when runs are allowed.
func TestEvalMapPrintAndApply(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on"))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	name, err := l.PutManifest("eval", evaltest.GoBugFix(t, l))
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := strings.Cut(name, "@")
	out := must(t, runEval(t, shop, &spawned, "eval", "map", id)).stdout
	var f pipeline.MapFile
	if err := json.Unmarshal([]byte(out), &f); err != nil || f.Eval != name {
		t.Fatalf("map: %v %s", err, out)
	}
	f.Requirements[0].Quote, f.Requirements[0].Tests = "Fix Total so it subtracts the discount from the price", []string{evaltest.GoBugFixTest}
	f.Context = []string{"Total ignores the discount."}
	file := filepath.Join(t.TempDir(), "map.json")
	b, _ := json.Marshal(f)
	os.WriteFile(file, b, 0o600)
	// G10-6: another positional eval is refused before anything is written
	for _, wrong := range []string{"NONEXISTENT-WRONG-TARGET", id + "@7"} {
		if r := runEval(t, shop, &spawned, "eval", "map", wrong, "--apply", file); r.code == 0 || !strings.Contains(r.stderr, "another eval") {
			t.Fatalf("%s: %+v", wrong, r)
		}
	}
	if names, _ := l.Manifests("eval"); len(names) != 1 || len(spawned) != 0 {
		t.Fatalf("a refused apply changed state: %v %v", names, spawned)
	}
	r := must(t, runEval(t, shop, &spawned, "eval", "map", id, "--apply", file))
	if !strings.Contains(r.stdout, id+"@2 (packaged)") || !strings.Contains(r.stdout, "Container runs are off") || len(spawned) != 0 {
		t.Fatalf("apply: %s %v", r.stdout, spawned)
	}
	if r := runEval(t, shop, &spawned, "eval", "map", id, "--apply", file); r.code == 0 || !strings.Contains(r.stderr, "latest") {
		t.Fatalf("a stale file was applied: %+v", r)
	}
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	out = must(t, runEval(t, shop, &spawned, "eval", "map", id)).stdout
	json.Unmarshal([]byte(out), &f)
	b, _ = json.Marshal(f)
	os.WriteFile(file, b, 0o600)
	r = must(t, runEval(t, shop, &spawned, "eval", "map", id, "--apply", file))
	if len(spawned) != 1 || spawned[0] != "eval __qualify" || !strings.Contains(r.stdout, "runs in the background") {
		t.Fatalf("runs on: %s %v", r.stdout, spawned)
	}
}

// E14: --with makes a variant for a variant arm only; a variant that is not validated yet
// does not run: the command lists it and starts the qualification worker.
func TestEvalRunWithVariant(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	if _, err := l.PutManifest("eval", v2); err != nil {
		t.Fatal(err)
	}
	id := rec["eval_id"].(string)
	os.WriteFile(filepath.Join(shop, "AGENTS.md"), []byte("Run go test ./... before you finish.\n"), 0o644)
	if r := runEval(t, shop, &spawned, "eval", "run", id, "--agent", "oracle", "--with", "AGENTS.md"); r.code == 0 || !strings.Contains(r.stderr, "only for --arm context_only") {
		t.Fatalf("--with on the baseline arm: %+v", r)
	}
	// D07: a context policy as a treatment cannot be applied inside the container yet: refused,
	// nothing runs, nothing is recorded
	if r := runEval(t, shop, &spawned, "eval", "run", id, "--agent", "oracle", "--arm", "context_only", "--context-policy", "depth=2"); r.code == 0 ||
		!strings.Contains(r.stderr, "--context-policy is not supported") || len(spawned) != 0 {
		t.Fatalf("--context-policy: %+v (spawned %v)", r, spawned)
	}
	outside := filepath.Join(t.TempDir(), "NOTES.md")
	os.WriteFile(outside, []byte("x"), 0o644)
	if r := runEval(t, shop, &spawned, "eval", "run", id, "--agent", "oracle", "--arm", "context_only", "--with", outside); r.code == 0 || !strings.Contains(r.stderr, "inside the repository") {
		t.Fatalf("a file outside the repository: %+v", r)
	}
	r := runEval(t, shop, &spawned, "eval", "run", id, "--agent", "oracle", "--arm", "context_only", "--with", "AGENTS.md")
	if r.code == 0 || !strings.Contains(r.stdout, "packaged (reference: original)") || len(spawned) != 1 {
		t.Fatalf("first run: %+v %v", r, spawned)
	}
	if trials, _ := l.Manifests("trial"); len(trials) != 0 {
		t.Fatal("an unqualified variant ran")
	}
}
