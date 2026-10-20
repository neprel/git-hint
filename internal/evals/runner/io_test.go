package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ioTaskDir writes a synthetic B1 task: the base tool and the reference tool are POSIX sh
// scripts run as "sh tool.sh" with the case input on stdin.
func ioTaskDir(t *testing.T, base, reference string, caseSeconds, verifierSeconds int, spec map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	w := func(rel, s string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	s := DefaultSettings
	s.AgentSeconds, s.VerifierSeconds = 120, verifierSeconds
	w("task.toml", taskTOML("hint/io-v1", 1, "io", s), 0o644)
	w("environment/Dockerfile", "FROM "+baseImage+"\nWORKDIR /app\nCOPY repo/ /app/\n", 0o644)
	w("environment/repo/tool.sh", base, 0o644)
	w("tests/Dockerfile", "FROM "+baseImage+"\n", 0o644)
	w("solution/solve.sh", solveFixed, 0o755)
	w("solution/files/tool.sh", reference, 0o644)
	if spec == nil {
		spec = map[string]any{"interface": IOInterface, "entry": []string{"sh", "tool.sh"}, "cases": []string{"lower", "mixed"}, "case_seconds": caseSeconds, "compare": "exact"}
	}
	b, _ := json.Marshal(spec)
	w("tests/io/spec.json", string(b), 0o644)
	w("tests/io/inputs/lower.in", "hello\n", 0o644)
	w("tests/io/inputs/mixed.in", "Ab c\nx", 0o644)
	return dir
}

const upper = "tr a-z A-Z\n"

// The declaration must be exactly valid (07B-J §15 condition 1).
func TestReadIOTask(t *testing.T) {
	if iot, err := readIOTask(t.TempDir()); iot != nil || err != nil {
		t.Fatalf("a task without a declaration: %v %v", iot, err)
	}
	ok := ioTaskDir(t, upper, upper, 5, 60, nil)
	iot, err := readIOTask(ok)
	if err != nil || len(iot.inputs) != 2 || string(iot.inputs["mixed"]) != "Ab c\nx" || !strings.HasPrefix(iot.inputsDigest, "sha256:") {
		t.Fatalf("valid declaration: %+v %v", iot, err)
	}
	spec := func(edit func(m map[string]any)) map[string]any {
		m := map[string]any{"interface": IOInterface, "entry": []string{"sh", "tool.sh"}, "cases": []string{"lower", "mixed"}, "case_seconds": 5, "compare": "exact"}
		edit(m)
		return m
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	for name, s := range map[string]map[string]any{
		"no cases":         spec(func(m map[string]any) { m["cases"] = []string{} }),
		"65 cases":         spec(func(m map[string]any) { m["cases"] = many }),
		"unsafe ID":        spec(func(m map[string]any) { m["cases"] = []string{"../x"} }),
		"duplicate ID":     spec(func(m map[string]any) { m["cases"] = []string{"lower", "lower"} }),
		"unknown field":    spec(func(m map[string]any) { m["shell"] = true }),
		"other interface":  spec(func(m map[string]any) { m["interface"] = "cli-file-v2" }),
		"other comparison": spec(func(m map[string]any) { m["compare"] = "trimmed" }),
		"empty entry":      spec(func(m map[string]any) { m["entry"] = []string{} }),
		"long timeout":     spec(func(m map[string]any) { m["case_seconds"] = 31 }),
		"missing input":    spec(func(m map[string]any) { m["cases"] = []string{"lower", "absent"} }),
	} {
		if _, err := readIOTask(ioTaskDir(t, upper, upper, 5, 60, s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	extra := ioTaskDir(t, upper, upper, 5, 60, nil)
	os.WriteFile(filepath.Join(extra, "tests", "io", "inputs", "other.in"), []byte("x"), 0o644)
	if _, err := readIOTask(extra); err == nil {
		t.Error("an unexpected input file is accepted")
	}
	link := ioTaskDir(t, upper, upper, 5, 60, nil)
	p := filepath.Join(link, "tests", "io", "inputs", "lower.in")
	os.Remove(p)
	os.Symlink("/etc/hostname", p)
	if _, err := readIOTask(link); err == nil {
		t.Error("a symlinked input is accepted")
	}
	big := ioTaskDir(t, upper, upper, 5, 60, nil)
	os.WriteFile(filepath.Join(big, "tests", "io", "inputs", "lower.in"), make([]byte, maxIOBytes+1), 0o644)
	if _, err := readIOTask(big); err == nil {
		t.Error("an oversized input is accepted")
	}
}

func ioRun(t *testing.T, d *Docker, task, agent string) Trial {
	t.Helper()
	jobs := t.TempDir()
	ts, err := d.Run(context.Background(), task, RunOptions{Agent: agent, JobsDir: jobs, JobName: "io"})
	if err != nil || len(ts) != 1 {
		t.Fatalf("run: %v %v", ts, err)
	}
	return ts[0]
}

// B1 on the local engine: the reference passes; the base fails; a candidate that matches
// the reference bytes with stderr noise passes; every run is process-separated.
func TestDockerRunnerIOVerdicts(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	ref := ioRun(t, d, ioTaskDir(t, "cat\n", upper, 5, 120, nil), "oracle")
	if ref.Outcome() != "passed" || ref.Checks["B1"] != "pass" || ref.Tests["B1 cases/lower::output"] != "pass" || ref.TrustOver([]string{"B1"}) != TrustProcessSeparated {
		t.Fatalf("reference: %+v", ref)
	}
	jobs := t.TempDir() // provenance of the verdict, sealed by the runner
	if _, err := d.Run(context.Background(), ioTaskDir(t, "cat\n", upper, 5, 120, nil), RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "p"}); err != nil {
		t.Fatal(err)
	}
	var res trialResult
	b, _ := os.ReadFile(filepath.Join(jobs, "p", "p__1", "result.json"))
	json.Unmarshal(b, &res)
	prov, _ := res.Container["io"].(map[string]any)
	for _, k := range []string{"reference_tree", "spec", "inputs", "oracle_outputs", "candidate_outputs"} {
		if v, _ := prov[k].(string); !strings.HasPrefix(v, "sha256:") {
			t.Fatalf("provenance %s missing: %v", k, prov)
		}
	}
	if prov["verdict_scope"] != "equivalence_to_reference_on_declared_cases" || prov["oracle_runs"] != float64(OracleRuns) {
		t.Fatalf("provenance %v", prov)
	}
	base := ioRun(t, d, ioTaskDir(t, "cat\n", upper, 5, 120, nil), "nop")
	if base.Outcome() != "failed" || base.Checks["B1"] != "fail" || base.TrustOver([]string{"B1"}) != TrustProcessSeparated {
		t.Fatalf("base: %+v", base)
	}
	noise := ioRun(t, d, ioTaskDir(t, upper+"echo noise >&2\n", upper, 5, 120, nil), "nop")
	if noise.Outcome() != "passed" {
		t.Fatalf("stderr is not part of the result: %+v", noise)
	}
}

// Candidate behavior never forges a pass: right bytes with another exit status, printed
// CHECK/TEST lines, a timeout and a flood all fail the case.
func TestDockerRunnerIOCandidateNegatives(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	for name, base := range map[string]string{
		"right-bytes-exit-3": upper + "exit 3\n",
		"printed-verdict":    "printf 'CHECK B1 PASS\\nTEST B1 cases/lower::output PASS\\n'\n",
		"timeout":            "sleep 30\n",
		"flood":              "head -c 2000000 /dev/zero\n",
	} {
		t.Run(name, func(t *testing.T) {
			jobs := t.TempDir()
			ts, err := d.Run(context.Background(), ioTaskDir(t, base, upper, 2, 120, nil), RunOptions{Agent: "nop", JobsDir: jobs, JobName: "io"})
			if err != nil || len(ts) != 1 || ts[0].Outcome() != "failed" || ts[0].Checks["B1"] != "fail" || ts[0].Tampered {
				t.Fatalf("%+v %v", ts, err)
			}
			out, _ := os.ReadFile(filepath.Join(jobs, "io", "io__1", "verifier", "test-stdout.txt"))
			if strings.Count(string(out), "CHECK B1") != 1 {
				t.Fatalf("verifier lines: %q", out)
			}
		})
	}
}

// An unusable reference gives no verdict (verifier_error, unknown trust), never a candidate
// result: a nondeterministic reference and a reference that times out. The shared deadline
// covers all cases: slow cases end the trial as a timeout instead of resetting per case.
func TestDockerRunnerIOOracleUnknownAndDeadline(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	for name, reference := range map[string]string{
		"nondeterministic": "od -An -N8 -tx8 /dev/urandom\n",
		"oracle-timeout":   "sleep 30\n",
	} {
		t.Run(name, func(t *testing.T) {
			tr := ioRun(t, d, ioTaskDir(t, upper, reference, 2, 120, nil), "oracle")
			if tr.Outcome() != "verifier_error" || tr.ResultTrust != TrustUnknown || tr.TrustOver([]string{"B1"}) != TrustUnknown {
				t.Fatalf("%+v", tr)
			}
		})
	}
	slow := ioRun(t, d, ioTaskDir(t, upper, "sleep 3; "+upper, 5, 8, nil), "nop")
	if slow.Outcome() != "timeout" || slow.TrustOver([]string{"B1"}) != TrustUnknown {
		t.Fatalf("shared deadline: %+v", slow)
	}
}
