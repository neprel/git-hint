package qualify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/runner"
)

// fakeRunner evaluates a task the way the generated verifier would, without containers:
// the work tree is the base repo plus the solution files (oracle) or unchanged (nop); C1
// passes when the work tree holds the fixed line, C2 passes when the old test still holds.
type fakeRunner struct {
	fixed      string // the line that the hidden test needs
	brokenC2   string // a line whose presence breaks the existing tests
	forgeable  bool   // simulate a shared verifier that trusts agent-written rewards
	netOpen    bool   // simulate a container with network
	infraFails map[string]bool
	tests      map[string]string // per-test results: test ID -> the line it needs (default: the hidden test needs fixed)
	unmapped   bool              // keep the request unmapped
	calls      []string
}

func (f *fakeRunner) Run(_ context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	f.calls = append(f.calls, o.JobName)
	if f.infraFails[o.JobName] {
		return []runner.Trial{{Name: o.JobName, Exception: "EnvironmentStartTimeoutError", Checks: map[string]string{}}}, nil
	}
	var out []runner.Trial
	for k := 0; k < max(1, o.Attempts); k++ {
		work := map[string]string{}
		filepath.Walk(filepath.Join(task, "environment", "repo"), func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				rel, _ := filepath.Rel(filepath.Join(task, "environment", "repo"), p)
				b, _ := os.ReadFile(p)
				work[rel] = string(b)
			}
			return nil
		})
		solve, _ := os.ReadFile(filepath.Join(task, "solution", "solve.sh"))
		probe := strings.Contains(string(solve), "HINT_PROBE")
		if o.Agent == "oracle" && !probe {
			filepath.Walk(filepath.Join(task, "solution", "files"), func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					rel, _ := filepath.Rel(filepath.Join(task, "solution", "files"), p)
					b, _ := os.ReadFile(p)
					work[rel] = string(b)
				}
				return nil
			})
		}
		all := strings.Join(mapValues(work), "\n")
		tests := f.tests
		if tests == nil {
			tests = map[string]string{evaltest.GoBugFixTest: f.fixed}
		}
		c1 := true
		per := map[string]string{}
		for id, line := range tests {
			ok := strings.Contains(all, line)
			c1 = c1 && ok
			per["C1 "+id] = pf(ok)
		}
		c2 := f.brokenC2 == "" || !strings.Contains(all, f.brokenC2)
		reward := 0.0
		if c1 && c2 {
			reward = 1
		}
		if probe && f.forgeable {
			reward = 1
		}
		tr := runner.Trial{Name: fmt.Sprintf("%s-%d", o.JobName, k), Reward: &reward, Checks: map[string]string{"C1": pf(c1), "C2": pf(c2)}, Tests: per}
		if o.Agent == "oracle" {
			net := "blocked"
			if f.netOpen {
				net = "reachable"
			}
			p := filepath.Join(o.JobsDir, o.JobName, tr.Name, "agent", "oracle.txt")
			os.MkdirAll(filepath.Dir(p), 0o755)
			body := ""
			if probe {
				body = "HINT_PROBE_NET=" + net + "\n"
			}
			os.WriteFile(p, []byte(body), 0o644)
		}
		out = append(out, tr)
	}
	return out, nil
}

func pf(b bool) string {
	if b {
		return "pass"
	}
	return "fail"
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func qualify(t *testing.T, f *fakeRunner) Result {
	t.Helper()
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	if !f.unmapped {
		rec = evaltest.MapGoBugFix(t, l, rec)
	}
	res, err := Run(context.Background(), l, f, rec, Options{Work: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func check(r Result, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestValidatedBugFix(t *testing.T) {
	f := &fakeRunner{fixed: "price - discount"}
	r := qualify(t, f)
	if r.State != "validated" || len(r.Reasons) != 0 {
		t.Fatalf("%+v", r)
	}
	if c := check(r, "alternative_passes"); c.Status != "missing" || c.Independent == nil || *c.Independent {
		t.Fatalf("alternative %+v", c)
	}
	if c := check(r, "reference_passes"); c.Runs != ReferenceRuns {
		t.Fatalf("reference runs %+v", c)
	}
}

// G10-3: a request that no one linked to tests is missing evidence; the eval is not
// validated even when every other check passes.
func TestUnmappedRequestBlocksValidation(t *testing.T) {
	r := qualify(t, &fakeRunner{fixed: "price - discount", unmapped: true})
	if r.State != "needs_review" || !has(r.Reasons, "requirement_coverage_missing") || check(r, "requirement_coverage").Status != "missing" {
		t.Fatalf("%+v", r)
	}
	if len(r.Requirements) != 3 || r.Requirements[0].Status != "missing" {
		t.Fatalf("%+v", r.Requirements)
	}
}

// G10-3: a requirement whose linked test already passes on the base has no discriminating
// evidence.
func TestRequirementWithoutDiscriminatingTest(t *testing.T) {
	// the hidden test holds on the base too, but C1 fails on the base through a second test
	r := qualify(t, &fakeRunner{fixed: "price - discount", tests: map[string]string{evaltest.GoBugFixTest: "package shop", "other_test.go::TestOther": "price - discount"}})
	if r.State != "needs_review" || !has(r.Reasons, "requirement_not_discriminating") || check(r, "requirement_coverage").Status != "fail" {
		t.Fatalf("%+v", r)
	}
}

func TestBasePassingIsNotDiscriminating(t *testing.T) {
	r := qualify(t, &fakeRunner{fixed: "package shop"}) // the base already holds it
	if r.State != "needs_review" || !has(r.Reasons, "base_passes") {
		t.Fatalf("%+v", r)
	}
}

func TestFailingReferenceIsQuarantined(t *testing.T) {
	r := qualify(t, &fakeRunner{fixed: "price - discount", brokenC2: "price - discount"})
	if r.State != "quarantined" || !has(r.Reasons, "reference_fails") {
		t.Fatalf("%+v", r)
	}
}

func TestForgeryAndNetworkAreQuarantined(t *testing.T) {
	r := qualify(t, &fakeRunner{fixed: "price - discount", forgeable: true})
	if r.State != "quarantined" || !has(r.Reasons, "verifier_forgeable") {
		t.Fatalf("forgery %+v", r)
	}
	r = qualify(t, &fakeRunner{fixed: "price - discount", netOpen: true})
	if r.State != "quarantined" || !has(r.Reasons, "isolation_failed") {
		t.Fatalf("network %+v", r)
	}
}

func TestInfrastructureFailureIsNotAPass(t *testing.T) {
	r := qualify(t, &fakeRunner{fixed: "price - discount", infraFails: map[string]bool{"reference": true}})
	if r.State == "validated" || r.State == "reproducible" || !has(r.Reasons, "runner_unavailable") {
		t.Fatalf("%+v", r)
	}
	if c := check(r, "reference_passes"); c.Status != "missing" {
		t.Fatalf("%+v", c)
	}
}

func TestNearMissVariants(t *testing.T) {
	task := t.TempDir()
	os.MkdirAll(filepath.Join(task, "environment", "repo"), 0o755)
	os.MkdirAll(filepath.Join(task, "solution", "files"), 0o755)
	base := "a\nb\nc\nd\ne\nf\ng\n"
	ref := "a\nB\nc\nd\ne\nF\ng\n" // two hunks
	os.WriteFile(filepath.Join(task, "environment", "repo", "x.txt"), []byte(base), 0o644)
	os.WriteFile(filepath.Join(task, "solution", "files", "x.txt"), []byte(ref), 0o644)
	vs, err := nearMisses(filepath.Join(task), 3)
	if err != nil || len(vs) != 2 {
		t.Fatalf("variants %v %v", vs, err)
	}
	got := map[string]bool{}
	for _, v := range vs {
		b, _ := os.ReadFile(filepath.Join(v, "solution", "files", "x.txt"))
		got[string(b)] = true
	}
	if !got["a\nb\nc\nd\ne\nF\ng\n"] || !got["a\nB\nc\nd\ne\nf\ng\n"] {
		t.Fatalf("partial references %v", got)
	}
}

func TestLeakProbe(t *testing.T) {
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	task := filepath.Join(t.TempDir(), "task")
	if _, err := runner.Export(l, rec, task, runner.Settings{}); err != nil {
		t.Fatal(err)
	}
	if r := leakProbe(task); r != "" {
		t.Fatalf("clean export: %s", r)
	}
	hidden, _ := os.ReadFile(filepath.Join(task, "tests", "hidden", "discount_test.go"))
	os.WriteFile(filepath.Join(task, "environment", "repo", "extra_test.go"), hidden, 0o644)
	if r := leakProbe(task); r != "solution_leak" {
		t.Fatalf("hidden test in the agent context: %q", r)
	}
	os.Remove(filepath.Join(task, "environment", "repo", "extra_test.go"))
	f, _ := os.OpenFile(filepath.Join(task, "instruction.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("func Total(price, discount int) int { return price - discount }\n")
	f.Close()
	if r := leakProbe(task); r != "solution_leak" {
		t.Fatalf("reference line in the instruction: %q", r)
	}
	os.MkdirAll(filepath.Join(task, "environment", "repo", ".git"), 0o755)
	if r := leakProbe(task); r != "future_history_leak" {
		t.Fatalf("git metadata: %q", r)
	}
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// G10-3, the reviewer's case: the request also asks for a panic on a negative discount; no
// test checks it and the reference does not panic. The eval must not be validated, and the
// uncovered requirement is named.
func TestUncheckedRequirementBlocksValidation(t *testing.T) {
	l := evaltest.Library(t)
	c := evaltest.PanicRequirement()
	b, err := evaltest.Package(t, l, c, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	rec := evaltest.MapChange(t, l, b.Record, c)
	r, err := Run(context.Background(), l, &fakeRunner{fixed: "price - discount"}, rec, Options{Work: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "needs_review" || !has(r.Reasons, "requirement_coverage_missing") {
		t.Fatalf("%+v", r)
	}
	var missing []string
	for _, q := range r.Requirements {
		if q.Status == "missing" {
			missing = append(missing, q.ReqID)
		}
	}
	if len(missing) != 1 || missing[0] != "R2" {
		t.Fatalf("requirements %+v", r.Requirements)
	}
}
