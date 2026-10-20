package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// writeTree stores files of dir into the bare snapshot repository and returns the tree ID.
func writeTree(t *testing.T, snap string, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	for p, c := range files {
		testkit.WriteFile(t, src, p, c)
	}
	cmd := exec.Command("sh", "-c", `git --git-dir "$1" --work-tree "$2" add -A . && git --git-dir "$1" --work-tree "$2" write-tree`, "sh", snap, src)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "idx"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

// project builds a Go project, a snapshot repository with its base and result trees, and
// an OpenCode export of the session that fixed the bug.
func e2eProject(t *testing.T) (*store.Store, Env) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	base := map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoBug, "cart_test.go": evaltest.GoOld}
	result := map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoFix, "cart_test.go": evaltest.GoOld, "discount_test.go": evaltest.GoAdded}
	testkit.Git(t, dir, "init", "-q")
	for p, c := range result {
		testkit.WriteFile(t, dir, p, c)
	}
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "state")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	snap := filepath.Join(data, "opencode", "snapshot", "prj_e2e", "wt")
	if out, err := exec.Command("git", "init", "-q", "--bare", snap).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	bt, rt := writeTree(t, snap, base), writeTree(t, snap, result)
	t0 := int64(1792000000000)
	tool := func(id, name string, input map[string]any, status string, at int64) map[string]any {
		return map[string]any{"type": "tool", "id": id, "name": name, "state": map[string]any{"status": status, "input": input, "content": []any{}, "metadata": map[string]any{}},
			"time": map[string]any{"created": at, "completed": at + 500}}
	}
	exp := map[string]any{
		"info": map[string]any{"id": "ses_e2e_0001", "projectID": "prj_e2e", "title": "e2e", "agent": "build", "cost": 0,
			"location": map[string]any{"directory": dir}, "model": map[string]any{"id": "m", "providerID": "lan", "variant": "default"},
			"time": map[string]any{"created": t0, "updated": t0 + 90000}, "tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}},
		"messages": []any{
			map[string]any{"type": "user", "id": "msg_u1", "text": "Total ignores the discount. Fix Total so it subtracts the discount from the price.", "time": map[string]any{"created": t0}},
			map[string]any{"type": "assistant", "id": "msg_a1", "agent": "build", "finish": "tool-calls", "cost": 0, "model": map[string]any{"id": "m", "providerID": "lan", "variant": "default"},
				"time": map[string]any{"created": t0 + 1000, "completed": t0 + 60000}, "tokens": map[string]any{"input": 900, "output": 80, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
				"snapshot": map[string]any{"start": bt, "end": rt},
				"content": []any{
					tool("t1", "read", map[string]any{"filePath": dir + "/cart.go"}, "completed", t0+2000),
					tool("t2", "edit", map[string]any{"filePath": dir + "/cart.go", "oldString": "price", "newString": "price - discount"}, "completed", t0+3000),
					tool("t3", "write", map[string]any{"filePath": dir + "/discount_test.go"}, "completed", t0+4000),
					tool("t4", "bash", map[string]any{"command": "go test ./..."}, "completed", t0+5000),
				}},
			map[string]any{"type": "assistant", "id": "msg_a2", "agent": "build", "finish": "stop", "cost": 0, "model": map[string]any{"id": "m", "providerID": "lan", "variant": "default"},
				"time": map[string]any{"created": t0 + 61000, "completed": t0 + 62000}, "tokens": map[string]any{"input": 100, "output": 10, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
				"snapshot": map[string]any{"start": rt, "end": rt}, "content": []any{map[string]any{"type": "text", "text": "Fixed; tests pass."}}},
		},
	}
	b, _ := json.Marshal(exp)
	env := Env{Home: t.TempDir(), DataHome: data, Cmd: fakeOC{root: dir, exportID: "ses_e2e_0001", export: string(b)}, Now: func() time.Time { return time.UnixMilli(t0 + 3600_000) }}
	return s, env
}

// e2eRunner evaluates tasks like the generated verifier (see qualify tests).
type e2eRunner struct{ calls, agentLog int }

func (r *e2eRunner) Run(_ context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	r.calls++
	solve, _ := os.ReadFile(filepath.Join(task, "solution", "solve.sh"))
	probe := strings.Contains(string(solve), "HINT_PROBE")
	fixed := false
	if o.Agent == "oracle" && !probe {
		b, _ := os.ReadFile(filepath.Join(task, "solution", "files", "cart.go"))
		fixed = strings.Contains(string(b), "price - discount")
	}
	var out []runner.Trial
	for k := 0; k < max(1, o.Attempts); k++ {
		rw := 0.0
		c1 := "fail"
		if fixed {
			rw, c1 = 1, "pass"
		}
		tr := runner.Trial{Name: fmt.Sprintf("%s-%d", o.JobName, k), Reward: &rw, Checks: map[string]string{"C1": c1, "C2": "pass"},
			Tests: map[string]string{"C1 " + evaltest.GoBugFixTest: c1, "C2 cart_test.go::TestNoDiscount": "pass"}}
		// the files a Harbor trial leaves (results, verifier output, agent log, artifact)
		dir := filepath.Join(o.JobsDir, o.JobName, tr.Name)
		for rel, body := range map[string]string{"result.json": `{"trial_name":"` + tr.Name + `"}`, "verifier/test-stdout.txt": "CHECK C1 " + strings.ToUpper(c1) + "\n",
			"verifier/c1.txt": "ok\n", "verifier/network.txt": "HINT_VERIFIER_NET=blocked\n", "agent/trajectory.json": strings.Repeat("x", r.agentLog), "artifacts/app/cart.go": "package shop\n"} {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755)
			os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644)
		}
		if probe {
			os.WriteFile(filepath.Join(dir, "agent", "oracle.txt"), []byte("HINT_PROBE_NET=blocked\n"), 0o644)
		}
		out = append(out, tr)
	}
	return out, nil
}

// mappingServer is a local model that answers with a requirement mapping.
func mappingServer(t *testing.T, answer string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HINT_TEST_MODEL_KEY", "model-key")
	return srv, &calls
}

// e2eMapping links every sentence of the request to the test; a model's context sentence
// would stay an unmapped requirement (TestModelContextNeedsReview).
const e2eMapping = `{"requirements": [{"source": "R1", "quote": "Total ignores the discount.", "tests": ["discount_test.go::TestDiscount"]}, {"source": "R1", "quote": "Fix Total so it subtracts the discount from the price.", "tests": ["discount_test.go::TestDiscount"]}], "context": []}`

// G10-3: without the local model the request stays unmapped, and the eval is not
// validated although every other check passes.
func TestSessionWithoutMappingNeedsReview(t *testing.T) {
	s, env := e2eProject(t)
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	s.Cfg.Evals = &p
	Run(context.Background(), s, env)
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	var v1 map[string]any
	if len(names) != 1 || l.Manifest("eval", names[0], &v1) != nil || !strings.Contains(fmt.Sprint(v1["reasons"]), "model_disabled") {
		t.Fatalf("v1 %v", v1["reasons"])
	}
	q := Qualify(context.Background(), s, &e2eRunner{}, env.Now())
	if q.States["needs_review"] != 1 {
		t.Fatalf("qualify %+v", q)
	}
	names, _ = l.Manifests("eval")
	var v2 map[string]any
	l.Manifest("eval", names[1], &v2)
	qual := v2["qualification"].(map[string]any)
	if !strings.Contains(fmt.Sprint(v2["reasons"]), "requirement_coverage_missing") || qual["policy"] != "q3" || len(qual["requirements"].([]any)) != 3 {
		t.Fatalf("v2 %v %v", v2["reasons"], qual)
	}
	if err := evals.ValidateValue("eval", v2); err != nil {
		t.Fatal(err)
	}
}

// Q-10-2, offline mapping path: without a model the user maps the request with
// git hint eval map; the new packaged version goes through the existing qualification
// and becomes validated there, not at apply.
func TestUserMappingFileToValidated(t *testing.T) {
	s, env := e2eProject(t)
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	s.Cfg.Evals = &p
	Run(context.Background(), s, env)
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	f, err := MapDraft(l, names[0])
	if err != nil {
		t.Fatal(err)
	}
	f.Requirements = []build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: []string{"discount_test.go::TestDiscount"}}}
	f.Context = []string{"Total ignores the discount."}
	b, _ := json.Marshal(f)
	next, unmapped, err := ApplyMapFile(context.Background(), l, f.Eval, b, "", env.Now())
	if err != nil || len(unmapped) != 0 {
		t.Fatalf("apply %v %v", err, unmapped)
	}
	q := Qualify(context.Background(), s, &e2eRunner{}, env.Now())
	if q.Qualified != 1 || q.States["validated"] != 1 {
		t.Fatalf("qualify %+v", q)
	}
	names, _ = l.Manifests("eval")
	var v map[string]any
	l.Manifest("eval", names[len(names)-1], &v)
	if v["previous"] != next || v["state"] != "validated" {
		t.Fatalf("%v %v", v["previous"], v["state"])
	}
}

// A mapping that leaves out a requested behavior is refused; the request stays unmapped.
func TestIncompleteMappingIsRefused(t *testing.T) {
	s, env := e2eProject(t)
	srv, calls := mappingServer(t, `{"requirements": [{"source": "R1", "quote": "Fix Total", "tests": ["discount_test.go::TestDiscount"]}], "context": ["Total ignores the discount."]}`)
	p := evals.Enabled([]string{"opencode"})
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	s.Cfg.Evals = &p
	Run(context.Background(), s, env)
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	var v1 map[string]any
	l.Manifest("eval", names[0], &v1)
	if *calls != 1 || len(build.Unmapped(v1)) != 1 || !strings.Contains(fmt.Sprint(v1["reasons"]), "requirement_mapping_incomplete") {
		t.Fatalf("calls %d reasons %v", *calls, v1["reasons"])
	}
}

// G10-3, review round 2: a model answer that calls a sentence of the request context
// does not remove it; the eval needs review although every other check passes.
func TestModelContextNeedsReview(t *testing.T) {
	s, env := e2eProject(t)
	srv, calls := mappingServer(t, `{"requirements": [{"source": "R1", "quote": "Fix Total so it subtracts the discount from the price.", "tests": ["discount_test.go::TestDiscount"]}], "context": ["Total ignores the discount."]}`)
	p := evals.Enabled([]string{"opencode"})
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	s.Cfg.Evals = &p
	Run(context.Background(), s, env)
	q := Qualify(context.Background(), s, &e2eRunner{}, env.Now())
	if *calls != 1 || q.States["needs_review"] != 1 {
		t.Fatalf("calls %d qualify %+v", *calls, q)
	}
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	var v2 map[string]any
	l.Manifest("eval", names[len(names)-1], &v2)
	if u := build.Unmapped(v2); len(u) != 1 || !strings.Contains(fmt.Sprint(v2["reasons"]), "requirement_coverage_missing") {
		t.Fatalf("unmapped %v reasons %v", u, v2["reasons"])
	}
}

func TestSessionToQualifiedEval(t *testing.T) {
	s, env := e2eProject(t)
	srv, calls := mappingServer(t, e2eMapping)
	p := evals.Enabled([]string{"opencode"})
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	s.Cfg.Evals = &p
	r := Run(context.Background(), s, env)
	if *calls != 1 {
		t.Fatalf("model calls %d", *calls)
	}
	if r.Episodes.Proposed != 1 || r.Packages.Packaged != 1 || r.Pending != 1 {
		t.Fatalf("pass: episodes %+v packages %+v pending %d", r.Episodes, r.Packages, r.Pending)
	}
	l := library.Open(s, 0)
	names, _ := l.Manifests("eval")
	if len(names) != 1 {
		t.Fatalf("evals %v", names)
	}
	var v1 map[string]any
	l.Manifest("eval", names[0], &v1)
	if v1["state"] != "packaged" || !strings.Contains(fmt.Sprint(v1["reasons"]), "execution_disabled") {
		t.Fatalf("v1 %v %v", v1["state"], v1["reasons"])
	}
	// qualification needs the execution permission
	if q := Qualify(context.Background(), s, &e2eRunner{}, env.Now()); q.Blocked == "" {
		t.Fatal("qualification ran without the execution permission")
	}
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	q := Qualify(context.Background(), s, &e2eRunner{}, env.Now())
	if q.Qualified != 1 || q.States["validated"] != 1 {
		t.Fatalf("qualify %+v", q)
	}
	names, _ = l.Manifests("eval")
	var v2 map[string]any
	if len(names) != 2 || l.Manifest("eval", names[1], &v2) != nil || v2["state"] != "validated" || v2["previous"] != names[0] {
		t.Fatalf("versions %v %v", names, v2["state"])
	}
	if err := evals.ValidateValue("eval", v2); err != nil {
		t.Fatal(err)
	}
	if len(Pending(l)) != 0 {
		t.Fatal("a qualified eval is still pending")
	}
	st, _ := l.Jobs()
	for _, j := range st {
		if j.Kind == "qualify" && (j.Trials == 0 || !j.Ended) {
			t.Fatalf("budget journal %+v", j)
		}
	}
	// a second qualification run finds nothing to do
	if q := Qualify(context.Background(), s, &e2eRunner{}, env.Now().Add(time.Minute)); q.Qualified != 0 {
		t.Fatalf("repeat %+v", q)
	}
}

// Replay of the LAN model's answer for the reviewer's G10-3 case (recorded 2026-10-18,
// 1 request): the model linked the subtraction to TestDiscount, gave the panic no test and
// put the defect description in context with the prompt's source label. The label is
// removed; the panic requirement and the model's context sentence stay unmapped, so the
// eval cannot be validated.
func TestReplayLiveMappingOfThePanicCase(t *testing.T) {
	answer, err := os.ReadFile(filepath.Join("testdata", "live-map-panic-answer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	srv, calls := mappingServer(t, string(answer))
	l := evaltest.Library(t)
	c := evaltest.PanicRequirement()
	b, err := evaltest.Package(t, l, c, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled(nil)
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "ai01", KeyEnv: "HINT_TEST_MODEL_KEY"}
	rec, why, d := mapRequirementsDetail(context.Background(), l, &p, b, evaltest.Tree(t, l, c.Result))
	if *calls != 1 || d.Cause != "" || why != "requirement_coverage_missing" {
		t.Fatalf("calls %d why %q cause %q", *calls, why, d.Cause)
	}
	if u := build.Unmapped(rec); len(u) != 2 || u[0] != "R2" || u[1] != "R3" {
		t.Fatalf("unmapped %v", u)
	}
	r1 := rec["requirements"].([]any)[0].(map[string]any)
	if r1["mapping"] != "model" || r1["tests"].([]any)[0] != evaltest.GoBugFixTest || r1["generator"] == nil {
		t.Fatalf("R1 %v", r1)
	}
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatal(err)
	}
}
