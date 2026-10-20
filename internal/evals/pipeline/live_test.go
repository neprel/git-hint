package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/runner"
)

// Live tests use only the owner's LAN model (AGENTS.md §11) with finite limits. Set
// HINT_LIVE_MODEL_URL (private address, .../v1), HINT_LIVE_MODEL and HINT_LOCAL_MODEL_KEY.
// HINT_LIVE_OUT is a directory that keeps the outputs for review and offline fixtures.

func liveModel(t *testing.T) *evals.Model {
	t.Helper()
	url := os.Getenv("HINT_LIVE_MODEL_URL")
	if url == "" {
		t.Skip("set HINT_LIVE_MODEL_URL")
	}
	return &evals.Model{Use: "lan", BaseURL: url, Model: os.Getenv("HINT_LIVE_MODEL"), KeyEnv: "HINT_LOCAL_MODEL_KEY",
		MaxRequestsPerJob: 2, MaxRetries: 1, MaxSecondsPerJob: 300}
}

func liveOut(t *testing.T, name string, data []byte) {
	if dir := os.Getenv("HINT_LIVE_OUT"); dir != "" {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
}

// TestLiveRequirementMap asks the LAN model for the requirement mapping of the reviewer's
// G10-3 case (a requested panic that no test checks). At most 2 requests. The outcome is
// logged; the test fails only on the budget, not on the model's judgment.
func TestLiveRequirementMap(t *testing.T) {
	m := liveModel(t)
	l := evaltest.Library(t)
	c := evaltest.PanicRequirement()
	b, err := evaltest.Package(t, l, c, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled(nil)
	p.Model = m
	if err := p.Check(evals.OpModel, ""); err != nil {
		t.Fatal(err) // a refused policy would end without a request
	}
	start := time.Now()
	rec, why, detail := mapRequirementsDetail(context.Background(), l, &p, b, evaltest.Tree(t, l, c.Result))
	liveOut(t, "requirement-map-raw-answer.txt", []byte(detail.Answer))
	t.Logf("refusal cause: %q", detail.Cause)
	jobs, _ := l.Jobs()
	var requests, retries int
	for _, j := range jobs {
		requests, retries = requests+j.Requests, retries+j.Retries
	}
	t.Logf("requests %d retries %d wall %s reason %q unmapped %v", requests, retries, time.Since(start).Round(time.Second), why, build.Unmapped(rec))
	for _, r := range rec["requirements"].([]any) {
		q := r.(map[string]any)
		text, _ := l.GetObject(q["text"].(string))
		t.Logf("  %s %-12s %-12v checks %v tests %v: %s", q["req_id"], q["origin"], q["mapping"], q["checks"], q["tests"], text)
		if g, ok := q["generator"].(map[string]any); ok {
			if out, err := l.GetObject(g["output"].(string)); err == nil {
				liveOut(t, "requirement-map-answer.json", out)
			}
		}
	}
	summary, _ := json.MarshalIndent(map[string]any{"requests": requests, "retries": retries, "reason": why, "unmapped": build.Unmapped(rec)}, "", "  ")
	liveOut(t, "requirement-map-summary.json", summary)
	if requests > 2 {
		t.Fatalf("budget exceeded: %d requests", requests)
	}
}

// TestLiveOpenCodeTrial runs one OpenCode trial of the validated synthetic bug fix through
// the built-in runner and HINT's counting proxy behind the gate: 1 eval x 1 attempt, at most
// 40 model requests (retries included: the proxy counts every request) and 900 seconds, no
// trial retry. Set HINT_DOCKER_TESTS=1 too; HINT_OPENCODE_TGZ is a local copy of the pinned
// OpenCode archive (else the pinned archive is fetched once).
func TestLiveOpenCodeTrial(t *testing.T) {
	m := liveModel(t)
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1")
	}
	ctx := context.Background()
	h, err := runner.FindDocker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, l := validatedProject(t)
	p := evals.Enabled(nil)
	p.Model = m
	p.Run = &evals.Run{Use: "docker", Runner: "docker", MaxTrialsPerRun: 1, MaxSecondsPerTrial: 900, MaxRequestsPerTrial: 40, CPUs: 2, MemoryMB: 2048}
	s.Cfg.Evals = &p
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: m.BaseURL, Model: m.Model, KeyEnv: m.KeyEnv, AgentVersion: runner.DockerOpenCodeVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	start := time.Now()
	rep, err := RunSuite(ctx, s, h, runner.DockerRunnerSemver, SuiteOptions{Agent: "opencode", Attempts: 1, Arm: "baseline", Live: lv, OpenCodeTgz: os.Getenv("HINT_OPENCODE_TGZ")}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("hint-docker %s: trials %d outcomes %v model requests %v kept %d wall %s", runner.DockerRunnerSemver, rep.Trials, rep.Outcomes, deref(rep.Requests), rep.Kept, time.Since(start).Round(time.Second))
	names, _ := l.Manifests("trial")
	for _, n := range names {
		var tr map[string]any
		if l.Manifest("trial", n, &tr) != nil {
			continue
		}
		b, _ := json.MarshalIndent(map[string]any{"outcome": tr["outcome"], "checks": tr["checks"], "counters": tr["counters"], "budget": tr["budget"], "agent": tr["agent"]}, "", "  ")
		t.Logf("trial %s:\n%s", n, b)
		liveOut(t, "opencode-trial-summary.json", b)
		if arts, ok := tr["artifacts"].([]any); ok && len(arts) == 1 {
			if a, err := l.GetObject(arts[0].(string)); err == nil {
				liveOut(t, "opencode-trial-run.tar", a)
			}
		}
	}
	if rep.Trials != 1 || rep.Requests == nil || *rep.Requests > 40 {
		t.Fatalf("limits: %+v", rep)
	}
	if rep.Outcomes["infrastructure_error"] > 0 || rep.Outcomes["verifier_error"] > 0 {
		t.Errorf("the trial did not run as a measurement: %v", rep.Outcomes)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
