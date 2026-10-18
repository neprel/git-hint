package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
)

// liveRunner is a fake Harbor run of a live agent: per attempt it sends one model request
// through HINT's proxy (the URL from the runner environment) and returns a scripted result.
type liveRunner struct {
	model   string         // the model field the agent sends
	results []runner.Trial // per attempt, in order
	n       int
}

func (r *liveRunner) Run(_ context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	var base string
	for _, e := range o.Env {
		if v, ok := strings.CutPrefix(e, "OPENAI_BASE_URL="); ok {
			base = v
		}
	}
	if r.model != "" {
		resp, err := http.Post(base+"/chat/completions", "application/json", bytes.NewReader([]byte(`{"model":"`+r.model+`","messages":[]}`)))
		if err == nil {
			resp.Body.Close()
		}
	}
	t := r.results[r.n%len(r.results)]
	r.n++
	t.Name = fmt.Sprintf("%s-%d", o.JobName, r.n)
	return []runner.Trial{t}, nil
}

func i64(v int64) *int64 { return &v }

func liveProject(t *testing.T) (*runner.Live, func()) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); io.WriteString(w, `{}`) }))
	t.Setenv("HINT_TEST_LAN_KEY", "k")
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: up.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	lv.ContainerHost = "127.0.0.1"
	return lv, func() { lv.Close(); up.Close() }
}

// TestControlledUnitsOfALiveRun: the manifest and an open unit exist before the first
// attempt; each trial appends one revision; a stopped run keeps the unit open; failures,
// errors, null usage and the model check stay in the attempts; the report validates.
func TestControlledUnitsOfALiveRun(t *testing.T) {
	s, l := validatedProject(t)
	names, _ := l.Manifests("eval")
	for _, n := range names { // make the validated eval a refactor task
		if strings.HasSuffix(n, "@2") {
			var rec map[string]any
			l.Manifest("eval", n, &rec)
			rec["family"], rec["version"], rec["previous"] = "refactor", 3, n
			if _, err := l.PutManifest("eval", rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	lv, stop := liveProject(t)
	defer stop()
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "harbor", MaxTrialsPerRun: 2} // stops after two attempts
	p.Model = &evals.Model{Use: "lan", BaseURL: "http://192.168.1.2/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY"}
	s.Cfg.Evals = &p
	pass, fail := 1.0, 0.0
	r := &liveRunner{model: "local-coder", results: []runner.Trial{
		{Reward: &fail, Checks: map[string]string{"C1": "fail", "C2": "pass"}, InputTokens: i64(1000), OutputTokens: i64(100)},
		{Reward: &pass, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: nil, OutputTokens: nil}, // usage missing
	}}
	rep, err := RunSuite(context.Background(), s, r, "0.23.0", SuiteOptions{Agent: "opencode", Attempts: 3, Arm: "baseline", Live: lv}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Trials != 2 || rep.Stopped == "" {
		t.Fatalf("run %+v", rep)
	}
	revs, err := workunits.Unreported(s)
	if err != nil || len(revs) != 3 { // revision 1 (open, no attempt) + one per trial
		t.Fatalf("revisions %d %v", len(revs), err)
	}
	last := revs[2].(map[string]any)
	sc := last["scope"].(map[string]any)
	if last["state"] != "open" || sc["planned_attempts"].(float64) != 3 || sc["run_ref"] == nil || len(sc["base_snapshots"].([]any)) != 0 {
		t.Fatalf("unit %v", last)
	}
	if sc["configuration"].(map[string]any)["model"].(map[string]any)["provenance"] != "launch_intent" {
		t.Fatal("the scope claims an effective model")
	}
	atts := last["attempts"].([]any)
	a1, a2 := atts[0].(map[string]any), atts[1].(map[string]any)
	if a1["outcome"] != "failed" || a1["usage_provenance"] != "harness_reported" || a1["model_check"] != "verified" {
		t.Fatalf("attempt 1 %v", a1)
	}
	// attempt 2 passed its checks, but the eval was qualified as validated, so it completes;
	// its usage is missing and stays null
	if a2["outcome"] != "completed" || a2["closure"].(map[string]any)["validation_ref"] != a2["trial_ref"] {
		t.Fatalf("attempt 2 closure %v", a2)
	}
	if a2["usage_provenance"] != "unknown" || a2["usage"].(map[string]any)["input_tokens"] != nil {
		t.Fatalf("attempt 2 usage %v", a2)
	}
	if revs[1].(map[string]any)["predecessor"] != revs[0].(map[string]any)["revision_digest"] {
		t.Fatal("broken chain")
	}
	// the report with the journal validates (no trials needed)
	b, _, err := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace, WorkUnits: revs}.Build(l, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(b, &doc)
	if len(doc["work_units"].([]any)) != 3 {
		t.Fatal("work units missing from the report")
	}
	// a rerun of the same suite is another run: another manifest and unit
	r2 := &liveRunner{model: "other-model", results: []runner.Trial{{Reward: &pass, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: i64(5), OutputTokens: i64(5)}}}
	if _, err := RunSuite(context.Background(), s, r2, "0.23.0", SuiteOptions{Agent: "opencode", Attempts: 1, Arm: "baseline", Live: lv}, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	revs, _ = workunits.Unreported(s)
	var units = map[any]bool{}
	var other map[string]any
	for _, x := range revs {
		m := x.(map[string]any)
		units[m["unit_ref"]] = true
		if m["revision"].(float64) == 2 && m["state"] == "closed" {
			other = m
		}
	}
	if len(units) != 2 || other == nil {
		t.Fatalf("units %d other %v", len(units), other)
	}
	if a := other["attempts"].([]any)[0].(map[string]any); a["model_check"] != "mismatch" || a["outcome"] != "completed" {
		t.Fatalf("a different model is not visible: %v", a) // the server keeps it out of complete coverage
	}
}
