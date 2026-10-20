package qualify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
)

func ioEval(t *testing.T, l *library.Library) map[string]any {
	t.Helper()
	base := evaltest.Tree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "cat\n"})
	result := evaltest.Tree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	b, err := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Family: "bug_fix", Repo: "shop", Base: base, Result: result,
		Request: "tool.sh must print its input in upper case.", RequestAt: map[string]any{"source": library.Digest([]byte("src")), "record": "u1"},
		Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}, {ID: "mixed", Input: []byte("Ab c\nx")}},
		CaseSeconds: 5, Now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return b.Record
}

// Export keeps the kind and the archive in agreement and puts the stored reference on the
// verifier side for a cli-file-v1 eval.
func TestExportIOKind(t *testing.T) {
	l := evaltest.Library(t)
	rec := ioEval(t, l)
	dir := filepath.Join(t.TempDir(), "task")
	if _, err := runner.Export(l, rec, dir, runner.DefaultSettings); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "tests", "io", "reference", "files", "tool.sh")); err != nil || string(b) != "tr a-z A-Z\n" {
		t.Fatalf("the stored reference is not on the verifier side: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "environment", "repo", "io")); err == nil {
		t.Fatal("the declaration is in the agent context")
	}
	gen := evaltest.GoBugFix(t, l)
	for name, r := range map[string]map[string]any{
		"cli kind with a test archive":     withVerifier(rec, map[string]any{"archive": gen["verifier"].(map[string]any)["archive"]}),
		"generated kind with a B1 archive": withVerifier(gen, map[string]any{"archive": rec["verifier"].(map[string]any)["archive"], "kind": nil}),
		"cli kind without reference":       without(rec, "reference"),
	} {
		if _, err := runner.Export(l, r, filepath.Join(t.TempDir(), "task"), runner.DefaultSettings); err == nil {
			t.Errorf("%s: exported", name)
		}
	}
}

func withVerifier(rec map[string]any, set map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range rec {
		out[k] = v
	}
	v := map[string]any{}
	for k, x := range rec["verifier"].(map[string]any) {
		v[k] = x
	}
	for k, x := range set {
		if x == nil {
			delete(v, k)
		} else {
			v[k] = x
		}
	}
	out["verifier"] = v
	return out
}

func without(rec map[string]any, key string) map[string]any {
	out := map[string]any{}
	for k, v := range rec {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// The whole qualification of a declared cli-file-v1 eval through the container runner on
// the local engine (offline; no model): the base fails B1, the reference passes it
// repeatedly, the empty change is the near miss, the forgery probe (a candidate that
// writes reward files and verdict lines and tries the network) gets reward 0, every run
// had no network, the agent context holds no input, and every verdict is process-separated.
func TestQualifyIOOnTheEngine(t *testing.T) {
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1 to qualify on the local engine")
	}
	d, err := runner.FindDocker(context.Background())
	if err != nil {
		t.Skip(err)
	}
	defer d.Close()
	l := evaltest.Library(t)
	rec := ioEval(t, l)
	res, err := Run(context.Background(), l, d, rec, Options{Work: t.TempDir(), Settings: runner.DefaultSettings})
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	for _, c := range res.Checks {
		st[c.Name] = c.Status
	}
	for _, c := range []string{"leak_probe", "base_fails", "reference_passes", "repeatable", "near_miss_fails", "forgery_probe", "isolation"} {
		if st[c] != "pass" {
			t.Errorf("%s: %s (%+v)", c, st[c], res)
		}
	}
	if res.ResultTrust != runner.TrustProcessSeparated {
		t.Errorf("result trust %s", res.ResultTrust)
	}
	if res.State == "validated" {
		t.Errorf("validated with an unmapped request: %+v", res) // R1 stays unmapped until a user links it
	}
}

func ioEvalWith(t *testing.T, l *library.Library, base map[string]string, cases []build.IOCase) map[string]any {
	t.Helper()
	res := map[string]string{}
	for k, v := range base {
		res[k] = v
	}
	res["tool.sh"] = "tr a-z A-Z\n"
	b, err := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Family: "bug_fix", Repo: "shop", Base: evaltest.Tree(t, l, base),
		Result: evaltest.Tree(t, l, res), Request: "tool.sh must print its input in upper case.", RequestAt: map[string]any{"source": library.Digest([]byte("src")), "record": "u1"},
		Entry: []string{"sh", "tool.sh"}, Cases: cases, CaseSeconds: 5, Now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return b.Record
}

// C7BJ-8: the record's B1 tests must be exactly the declared cases in order.
func TestExportIOCaseBinding(t *testing.T) {
	l := evaltest.Library(t)
	for name, tests := range map[string][]any{
		"renamed":   {"cases/lower::output", "cases/other::output"},
		"omitted":   {"cases/lower::output"},
		"extra":     {"cases/lower::output", "cases/mixed::output", "cases/more::output"},
		"reordered": {"cases/mixed::output", "cases/lower::output"},
		"empty":     {},
	} {
		rec := ioEval(t, l)
		rec["verifier"].(map[string]any)["checks"].([]any)[0].(map[string]any)["tests"] = tests
		if _, err := runner.Export(l, rec, filepath.Join(t.TempDir(), "task"), runner.DefaultSettings); err == nil {
			t.Errorf("%s: exported", name)
		}
	}
	if _, err := runner.Export(l, ioEval(t, l), filepath.Join(t.TempDir(), "task"), runner.DefaultSettings); err != nil {
		t.Fatalf("exact match: %v", err)
	}
}

// C7BJ-9: ordinary cases (a blank line, a short common word also in the base) are no leak;
// copies of evaluator artifacts are: a distinctive input in the base or the instruction,
// the declaration in the context, an input file copied whole, a file the export never
// writes into the agent context.
func TestIOLeakProbe(t *testing.T) {
	l := evaltest.Library(t)
	export := func(rec map[string]any) string {
		dir := filepath.Join(t.TempDir(), "task")
		if _, err := runner.Export(l, rec, dir, runner.DefaultSettings); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	base := map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "cat\n", "README": "ok\n"}
	ordinary := export(ioEvalWith(t, l, base, []build.IOCase{{ID: "blank", Input: []byte("\n")}, {ID: "word", Input: []byte("ok\n")}}))
	if why := leakProbe(ordinary); why != "" {
		t.Fatalf("ordinary cases: %s", why)
	}
	canary := "HINTCANARY input line that only the evaluator holds"
	leaked := map[string]string{"go.mod": base["go.mod"], "tool.sh": base["tool.sh"], "notes.txt": "see " + canary + "\n"}
	if why := leakProbe(export(ioEvalWith(t, l, leaked, []build.IOCase{{ID: "c", Input: []byte(canary + "\n")}}))); why != "solution_leak" {
		t.Errorf("a distinctive input in the base: %q", why)
	}
	clean := func() string {
		return export(ioEvalWith(t, l, base, []build.IOCase{{ID: "c", Input: []byte(canary + "\n")}}))
	}
	if why := leakProbe(clean()); why != "" {
		t.Fatalf("clean distinctive case: %s", why)
	}
	for name, mutate := range map[string]func(dir string){
		"input in the instruction": func(dir string) { appendFile(t, filepath.Join(dir, "instruction.md"), canary+"\n") },
		"declaration in the context": func(dir string) {
			spec, _ := os.ReadFile(filepath.Join(dir, "tests", "io", "spec.json"))
			os.WriteFile(filepath.Join(dir, "environment", "repo", "x.json"), spec, 0o644)
		},
		"input file copied": func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "tests", "io", "inputs", "c.in"))
			os.WriteFile(filepath.Join(dir, "environment", "repo", "c.in"), b, 0o644)
		},
		"file outside the export": func(dir string) {
			os.MkdirAll(filepath.Join(dir, "environment", "io"), 0o755)
			os.WriteFile(filepath.Join(dir, "environment", "io", "x"), []byte("x"), 0o644)
		},
	} {
		dir := clean()
		mutate(dir)
		if why := leakProbe(dir); why != "solution_leak" {
			t.Errorf("%s: %q", name, why)
		}
	}
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}
