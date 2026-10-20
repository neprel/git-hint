package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/project"
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

// TestLiveOpenCodeTrial is one session of the bounded §11 check of the pinned OpenCode on the
// built-in runner: the validated synthetic bug fix, 1 eval x 1 attempt, at most 8 forwarded
// model requests (retries included: the proxy counts every request; at most 1 retry) and
// 300 seconds of agent time, no trial retry. The endpoint is checked first by a TCP connect
// only (no request); when it is unreachable nothing starts. Set HINT_DOCKER_TESTS=1 too;
// HINT_OPENCODE_TGZ is a local copy of the pinned OpenCode archive (else it is fetched once).
// The run checks the chain (gate, ledger, result trust, report) and that the key appears in
// no output; it does not measure canonical tokens or savings.
func TestLiveOpenCodeTrial(t *testing.T) {
	m := liveModel(t)
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1")
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", u.Host, 3*time.Second); err != nil {
		t.Skipf("the LAN endpoint is not reachable (TCP %s): 0 model requests", u.Host)
	} else {
		c.Close()
	}
	const maxRequests, maxSeconds = 8, 300
	ctx := context.Background()
	h, err := runner.FindDocker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, l := validatedProject(t)
	p := evals.Enabled(nil)
	p.Model = m
	p.Model.MaxRetries = 1
	p.Run = &evals.Run{Use: "docker", Runner: "docker", MaxTrialsPerRun: 1, MaxSecondsPerTrial: maxSeconds, MaxRequestsPerTrial: maxRequests, CPUs: 2, MemoryMB: 2048}
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
	summary := map[string]any{"runner": "hint-docker " + runner.DockerRunnerSemver, "opencode": runner.DockerOpenCodeVersion, "trials": rep.Trials, "outcomes": rep.Outcomes,
		"model_requests": deref(rep.Requests), "ledgers_not_written": rep.LedgersNotWritten, "wall_seconds": time.Since(start).Round(time.Second).Seconds()}
	sess := liveSession{Trials: rep.Trials, Stop: rep.Stopped, Requests: rep.Requests, MaxRequests: maxRequests, LedgersNotWritten: rep.LedgersNotWritten}
	var outputs [][]byte
	names, _ := l.Manifests("trial")
	for _, n := range names {
		var tr map[string]any
		if l.Manifest("trial", n, &tr) != nil {
			continue
		}
		sess.Outcome, _ = tr["outcome"].(string)
		summary["trial"] = map[string]any{"outcome": tr["outcome"], "checks": tr["checks"], "counters": tr["counters"], "budget": tr["budget"], "agent": tr["agent"], "runner": tr["runner"], "result_trust": tr["result_trust"]}
		if arts, ok := tr["artifacts"].([]any); ok && len(arts) == 1 {
			if a, err := l.GetObject(arts[0].(string)); err == nil {
				outputs = append(outputs, a)
				liveOut(t, "opencode-trial-run.tar", a)
			}
		}
	}
	// the ledger handed over after teardown (gate, enforcement, completeness)
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	var ledgers []any
	for _, o := range obs {
		if m := o.ModelLedger; m != nil {
			ledgers = append(ledgers, map[string]any{"state": m.State, "bypass": m.Bypass, "reasons": m.Reasons, "counts": m.Counts})
			sess.Ledgers++
			sess.LedgerState, sess.LedgerBypass, sess.LedgerReasons, sess.CompleteResponses = m.State, m.Bypass, m.Reasons, m.Counts["complete"]
		}
	}
	summary["ledgers"] = ledgers
	// the report with the journal validates
	revs, _ := workunits.Unreported(s)
	doc, _, rerr := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace, WorkUnits: revs}.Build(l, names, time.Now())
	summary["report_error"] = fmt.Sprint(rerr)
	sess.ReportErr = rerr
	outputs = append(outputs, doc)
	b, _ := json.MarshalIndent(summary, "", "  ")
	outputs = append(outputs, b)
	// the key never appears in an output (the value is not printed)
	if key := os.Getenv(m.KeyEnv); key != "" {
		for _, o := range outputs {
			sess.KeyFound = sess.KeyFound || bytes.Contains(o, []byte(key))
		}
	}
	// the series continues (macOS before Linux) only on a verified session, never on a test
	// PASS alone; the actual result is kept either way
	verr := sess.verdict()
	summary["series_may_continue"], summary["verdict"] = verr == nil, fmt.Sprint(verr)
	b, _ = json.MarshalIndent(summary, "", "  ")
	liveOut(t, "opencode-trial-summary.json", b)
	t.Logf("summary:\n%s", b)
	if verr != nil {
		t.Fatalf("the session did not verify the chain; stop the series: %v", verr)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
