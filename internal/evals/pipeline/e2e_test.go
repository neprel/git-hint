package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
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
type e2eRunner struct{ calls int }

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
		tr := runner.Trial{Name: fmt.Sprintf("%s-%d", o.JobName, k), Reward: &rw, Checks: map[string]string{"C1": c1, "C2": "pass"}}
		if probe {
			p := filepath.Join(o.JobsDir, o.JobName, tr.Name, "agent", "oracle.txt")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("HINT_PROBE_NET=blocked\n"), 0o644)
		}
		out = append(out, tr)
	}
	return out, nil
}

func TestSessionToQualifiedEval(t *testing.T) {
	s, env := e2eProject(t)
	p := evals.Enabled([]string{"opencode"})
	s.Cfg.Evals = &p
	r := Run(context.Background(), s, env)
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
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	var rn qualify.Runner = &e2eRunner{}
	if os.Getenv("HINT_HARBOR") != "" {
		h, err := runner.FindHarbor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rn = h
	}
	q := Qualify(context.Background(), s, rn, env.Now())
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
	if q := Qualify(context.Background(), s, rn, env.Now().Add(time.Minute)); q.Qualified != 0 {
		t.Fatalf("repeat %+v", q)
	}
}
