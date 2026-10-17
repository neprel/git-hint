package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func validatedProject(t *testing.T) (*store.Store, *library.Library) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := map[string]any{}
	for k, v := range rec {
		v2[k] = v
	}
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	if _, err := l.PutManifest("eval", v2); err != nil {
		t.Fatal(err)
	}
	return s, l
}

func TestRunSuiteRecordsEveryAttempt(t *testing.T) {
	s, l := validatedProject(t)
	p := evals.Enabled(nil)
	s.Cfg.Evals = &p
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle"}, time.Now()); err == nil {
		t.Fatal("a suite ran without the execution permission")
	}
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "opencode"}, time.Now()); err == nil || !strings.Contains(err.Error(), "local model") {
		t.Fatalf("a live agent ran without the model policy: %v", err)
	}
	rep, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Attempts: 2, Arm: "baseline"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Trials != 2 || rep.Outcomes["passed"] != 2 || rep.Requests == nil || *rep.Requests != 0 {
		t.Fatalf("report %+v", rep)
	}
	nop, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "nop", Arm: "configuration"}, time.Now().Add(time.Minute))
	if err != nil || nop.Outcomes["failed"] != 1 {
		t.Fatalf("nop %+v %v", nop, err)
	}
	rows, err := Compare(l, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v %v", rows, err)
	}
	for _, r := range rows {
		if r.Arm == "baseline" && (r.Passed != 2 || r.Trials != 2) || r.Arm == "configuration" && r.Failed != 1 {
			t.Fatalf("row %+v", r)
		}
		if r.InputTokens != nil { // the fake runner reports no tokens: unknown, not zero
			t.Fatalf("unknown tokens summed as %d", *r.InputTokens)
		}
	}
}

func TestCompareKeepsFailuresAndUnknowns(t *testing.T) {
	_, l := validatedProject(t)
	ten, five := int64(10), int64(5)
	w1, w2, w3 := 10.0, 30.0, 600.0
	r1 := 1.0
	r0 := 0.0
	base := suites.TrialInput{Suite: "su-0000000000000001", Eval: "ev-0000000000000001@1", Arm: "baseline", Agent: "opencode", Model: "m", RunnerVersion: "0.23.0", MaxSeconds: 600}
	for i, tr := range []runner.Trial{
		{Name: "a", Reward: &r1, Checks: map[string]string{"C1": "pass"}, InputTokens: &ten, OutputTokens: &five, WallSeconds: &w1},
		{Name: "b", Reward: &r0, Checks: map[string]string{"C1": "fail"}, InputTokens: &ten, OutputTokens: &five, WallSeconds: &w2}, // fast but wrong
		{Name: "c", Exception: "AgentTimeoutError", Checks: map[string]string{}, WallSeconds: &w3},                                  // usage unknown
	} {
		in := base
		in.Attempt, in.Started = i+1, time.Unix(int64(i), 0)
		if _, err := suites.RecordTrial(l, in, tr); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := Compare(l, "su-0000000000000001")
	r := rows[0]
	if r.Trials != 3 || r.Passed != 1 || r.Failed != 1 || r.Timeouts != 1 || r.UnknownUsage != 1 || r.InputTokens != nil || *r.MedianWall != 30 {
		t.Fatalf("row %+v", r)
	}
}
