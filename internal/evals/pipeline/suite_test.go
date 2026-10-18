package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
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
	v2 := evaltest.MapGoBugFix(t, l, rec)
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

// An episode without a test of its own gets a hidden check drafted by the local model
// (when the policy allows it); the check is labeled generated with its provenance.
func TestDraftedVerifier(t *testing.T) {
	draft := "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif Total(10, 3) != 7 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": draft}}}})
	}))
	defer srv.Close()
	t.Setenv("HINT_TEST_MODEL_KEY", "model-key")
	l := evaltest.Library(t)
	base := evaltest.Tree(t, l, map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoBug, "cart_test.go": evaltest.GoOld})
	result := evaltest.Tree(t, l, map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoFix, "cart_test.go": evaltest.GoOld})
	in := build.Input{EpisodeID: "ep-00000000000000d1", Family: "bug_fix", Repo: "shop", Base: base, Result: result, Paths: []string{"cart.go"},
		Attribution: "exclusive", Outcome: "test_passed", Request: "Fix Total so it subtracts the discount.",
		RequestAt: map[string]any{"source": library.Digest([]byte("s")), "record": "u1"}, CheckHint: "go test ./...", Now: time.Now()}
	if _, err := build.Build(l, in); err == nil {
		t.Fatal("an episode without a test was packaged without a draft")
	}
	off := evals.Enabled(nil)
	if _, ok := draftVerifier(context.Background(), l, &off, in); ok || calls != 0 {
		t.Fatal("a draft was requested without the model permission")
	}
	p := evals.Enabled(nil)
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	drafted, ok := draftVerifier(context.Background(), l, &p, in)
	if !ok || calls != 1 {
		t.Fatalf("draft %v calls %d", ok, calls)
	}
	b, err := build.Build(l, drafted)
	if err != nil {
		t.Fatal(err)
	}
	c1 := b.Record["verifier"].(map[string]any)["checks"].([]any)[0].(map[string]any)
	g, _ := c1["generator"].(map[string]any)
	if c1["origin"] != "generated" || g == nil || g["model"] != "local-coder" || g["requests"] != int64(1) && g["requests"] != 1 {
		t.Fatalf("C1 %v", c1)
	}
	if err := evals.ValidateValue("eval", b.Record); err != nil {
		t.Fatal(err)
	}
	if _, ok := draftVerifier(context.Background(), l, &p, in); ok || calls != 1 {
		t.Fatal("the same episode got a second draft budget")
	}
}

// G10-4: the results of a trial (verifier output, agent log, collected artifact) are
// archived in the library before the work directory is removed; when they cannot be
// archived, the directory stays.
func TestRunSuiteKeepsRunResults(t *testing.T) {
	s, l := validatedProject(t)
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	rep, err := RunSuite(context.Background(), s, &e2eRunner{agentLog: 100}, "0.23.0", SuiteOptions{Agent: "oracle"}, time.Now())
	if err != nil || rep.Trials != 1 || rep.Kept != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	names, _ := l.Manifests("trial")
	var tr map[string]any
	if len(names) != 1 || l.Manifest("trial", names[0], &tr) != nil {
		t.Fatalf("trials %v", names)
	}
	arts, _ := tr["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("trial artifacts %v", tr["artifacts"])
	}
	files := archiveNames(t, l, arts[0].(string))
	for _, want := range []string{"result.json", "verifier/test-stdout.txt", "verifier/c1.txt", "agent/trajectory.json", "artifacts/app/cart.go"} {
		if !files[want] {
			t.Errorf("archive lacks %s: %v", want, files)
		}
	}
	if ents, _ := os.ReadDir(s.Path("cache", "evals", "work")); len(ents) != 0 {
		t.Fatalf("work directories left: %v", ents)
	}
	// the archived results survive garbage collection
	if _, err := l.CollectGarbage(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetObject(arts[0].(string)); err != nil {
		t.Fatal("garbage collection removed the run results")
	}
	// a run whose results do not fit the quota keeps its directory
	p.QuotaMB = 16
	rep, err = RunSuite(context.Background(), s, &e2eRunner{agentLog: 17 << 20}, "0.23.0", SuiteOptions{Agent: "oracle"}, time.Now().Add(time.Minute))
	if err != nil || rep.Trials != 1 || rep.Kept != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	ents, _ := os.ReadDir(s.Path("cache", "evals", "work"))
	if len(ents) != 1 {
		t.Fatalf("the unarchived run directory was removed: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(s.Path("cache", "evals", "work"), ents[0].Name(), "jobs")); err != nil {
		t.Fatal(err)
	}
}

// G10-4: a qualification archives the full verifier output, not only the reward.
func TestQualifyKeepsRunResults(t *testing.T) {
	s, env := e2eProject(t)
	srv, _ := mappingServer(t, e2eMapping)
	p := evals.Enabled([]string{"opencode"})
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	Run(context.Background(), s, env)
	if q := Qualify(context.Background(), s, &e2eRunner{agentLog: 10}, env.Now()); q.Qualified != 1 || q.Kept != 0 {
		t.Fatalf("%+v", q)
	}
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	var v2 map[string]any
	l.Manifest("eval", names[1], &v2)
	ev := v2["qualification"].(map[string]any)["checks"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(string)
	files := archiveNames(t, l, ev)
	for _, want := range []string{"jobs/base/base-0/verifier/c1.txt", "jobs/reference/reference-0/artifacts/app/cart.go"} {
		if !files[want] {
			t.Errorf("evidence lacks %s: %v", want, files)
		}
	}
}

func archiveNames(t *testing.T, l *library.Library, digest string) map[string]bool {
	t.Helper()
	b, err := l.GetObject(digest)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		out[strings.TrimPrefix(h.Name, "jobs/trial/trial-0/")] = true
	}
	return out
}
