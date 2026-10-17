package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
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
