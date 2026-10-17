// Package qualify runs the automatic qualification of a packaged eval version under the
// named policy q1 and returns the new state with every check result.
//
// Policy q1 (all trials run through the runner; nothing runs on the host):
//   - base_fails: the nop agent fails check C1 (the hidden tests);
//   - reference_passes and repeatable: the oracle passes C1 and C2 in every one of
//     ReferenceRuns clean trials;
//   - near_miss_fails: partial references (one changed file or one change hunk left out)
//     all fail; when the reference is a single hunk, the only partial reference is the
//     empty change, which is the base trial: the check takes the base result;
//   - alternative_passes: missing unless an independent solution exists (generated
//     solutions are not independent); not required for promotion;
//   - leak_probe: the agent build context holds no hidden test, no reference-only line
//     and no Git metadata;
//   - forgery_probe and isolation: a solution that tries the network and writes reward
//     files must get reward 0 and must see no network.
//
// State: validated when every required check passes; reproducible when base and
// reference checks pass but other evidence is missing; needs_review or quarantined with
// reasons otherwise. A missing result never counts as a pass.
package qualify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
)

// Policy is the qualification policy name.
const Policy = "q1"

// ReferenceRuns is the number of clean oracle trials.
const ReferenceRuns = 3

// MaxNearMisses bounds the partial-reference trials.
const MaxNearMisses = 3

// Runner runs a task directory (runner.Harbor in production).
type Runner interface {
	Run(ctx context.Context, taskDir string, o runner.RunOptions) ([]runner.Trial, error)
}

// Check is one qualification result.
type Check struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // pass, fail, missing, not_applicable
	Runs        int    `json:"runs"`
	Independent *bool  `json:"independent,omitempty"`
}

// Result of a qualification.
type Result struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`
	Checks  []Check  `json:"checks"`
	Trials  int      `json:"trials"`
}

// Options of one qualification.
type Options struct {
	Work     string // directory for task copies and job outputs
	Settings runner.Settings
}

// Run qualifies one eval record. It does not store anything; the caller writes the new
// eval version with the result.
func Run(ctx context.Context, l *library.Library, r Runner, rec map[string]any, o Options) (Result, error) {
	res := Result{}
	task := filepath.Join(o.Work, "task")
	if _, err := runner.Export(l, rec, task, o.Settings); err != nil {
		return res, err
	}
	jobs := filepath.Join(o.Work, "jobs")
	run := func(name, agent, taskDir string, attempts int) ([]runner.Trial, error) {
		ts, err := r.Run(ctx, taskDir, runner.RunOptions{Agent: agent, Attempts: attempts, JobsDir: jobs, JobName: name})
		res.Trials += len(ts)
		return ts, err
	}
	add := func(c Check) { res.Checks = append(res.Checks, c) }
	reason := func(rs ...string) { res.Reasons = append(res.Reasons, rs...) }

	// leak probe (static, before any trial)
	leak := leakProbe(task)
	add(Check{Name: "leak_probe", Status: status(leak == ""), Runs: 1})
	if leak != "" {
		reason(leak)
	}
	// base
	nop, err := run("base", "nop", task, 1)
	baseFails := err == nil && len(nop) == 1 && nop[0].Outcome() == "failed" && nop[0].Checks["C1"] == "fail"
	switch {
	case err != nil || len(nop) != 1 || infra(nop[0]):
		add(Check{Name: "base_fails", Status: "missing", Runs: len(nop)})
		reason("runner_unavailable")
	case baseFails:
		add(Check{Name: "base_fails", Status: "pass", Runs: 1})
	default:
		add(Check{Name: "base_fails", Status: "fail", Runs: 1})
		reason("base_passes")
	}
	// reference, repeated
	or, err := run("reference", "oracle", task, ReferenceRuns)
	passes, infraErr := 0, err != nil
	for _, t := range or {
		if infra(t) {
			infraErr = true
		}
		if t.Outcome() == "passed" && t.Checks["C1"] == "pass" && t.Checks["C2"] == "pass" {
			passes++
		}
	}
	refOK := passes == ReferenceRuns && len(or) == ReferenceRuns
	switch {
	case refOK:
		add(Check{Name: "reference_passes", Status: "pass", Runs: len(or)})
		add(Check{Name: "repeatable", Status: "pass", Runs: len(or)})
	case passes == 0 && !infraErr:
		add(Check{Name: "reference_passes", Status: "fail", Runs: len(or)})
		add(Check{Name: "repeatable", Status: "not_applicable", Runs: len(or)})
		reason("reference_fails")
	case infraErr:
		add(Check{Name: "reference_passes", Status: "missing", Runs: len(or)})
		add(Check{Name: "repeatable", Status: "missing", Runs: len(or)})
		reason("runner_unavailable")
	default:
		add(Check{Name: "reference_passes", Status: "pass", Runs: len(or)})
		add(Check{Name: "repeatable", Status: "fail", Runs: len(or)})
		reason("not_repeatable")
	}
	// near misses
	variants, err := nearMisses(task, MaxNearMisses)
	if err != nil {
		return res, err
	}
	if len(variants) == 0 {
		// a single-hunk reference: the empty change (the base trial) is the near miss
		switch {
		case baseFails:
			add(Check{Name: "near_miss_fails", Status: "pass", Runs: 1})
		case len(nop) == 1 && !infra(nop[0]):
			add(Check{Name: "near_miss_fails", Status: "fail", Runs: 1})
			reason("verifier_not_discriminating")
		default:
			add(Check{Name: "near_miss_fails", Status: "missing", Runs: 0})
			reason("near_miss_missing")
		}
	} else {
		failed, runs, missing := 0, 0, false
		for i, v := range variants {
			ts, err := run(fmt.Sprintf("near-miss-%d", i+1), "oracle", v, 1)
			if err != nil || len(ts) != 1 || infra(ts[0]) {
				missing = true
				continue
			}
			runs++
			if ts[0].Outcome() == "failed" {
				failed++
			}
		}
		switch {
		case missing:
			add(Check{Name: "near_miss_fails", Status: "missing", Runs: runs})
			reason("runner_unavailable")
		case failed == len(variants):
			add(Check{Name: "near_miss_fails", Status: "pass", Runs: runs})
		default:
			add(Check{Name: "near_miss_fails", Status: "fail", Runs: runs})
			reason("verifier_not_discriminating")
		}
	}
	// independent alternative: none can be constructed without a model
	no := false
	add(Check{Name: "alternative_passes", Status: "missing", Runs: 0, Independent: &no})
	// forgery and isolation probe
	probe, err := probeTask(task)
	if err != nil {
		return res, err
	}
	pt, err := run("probe", "oracle", probe, 1)
	if err != nil || len(pt) != 1 || infra(pt[0]) {
		add(Check{Name: "forgery_probe", Status: "missing", Runs: len(pt)})
		add(Check{Name: "isolation", Status: "missing", Runs: len(pt)})
		reason("runner_unavailable")
	} else {
		// the probe leaves the base state: a reward is forged only when the base fails
		forged := pt[0].Reward != nil && *pt[0].Reward > 0
		switch {
		case !baseFails:
			add(Check{Name: "forgery_probe", Status: "not_applicable", Runs: 1})
		case forged:
			add(Check{Name: "forgery_probe", Status: "fail", Runs: 1})
			reason("verifier_forgeable")
		default:
			add(Check{Name: "forgery_probe", Status: "pass", Runs: 1})
		}
		net := netSeen(filepath.Join(jobs, "probe"))
		add(Check{Name: "isolation", Status: status(net == "blocked"), Runs: 1})
		if net != "blocked" {
			reason("isolation_failed")
		}
	}
	res.State = decide(res)
	res.Reasons = uniq(res.Reasons)
	return res, nil
}

func status(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func infra(t runner.Trial) bool {
	o := t.Outcome()
	return o == "infrastructure_error" || o == "timeout" || o == "verifier_error"
}

func decide(r Result) string {
	st := map[string]string{}
	for _, c := range r.Checks {
		st[c.Name] = c.Status
	}
	for _, q := range []string{"leak_probe", "forgery_probe", "isolation"} {
		if st[q] == "fail" {
			return "quarantined"
		}
	}
	if st["reference_passes"] == "fail" {
		return "quarantined"
	}
	if st["base_fails"] == "fail" || st["repeatable"] == "fail" || st["near_miss_fails"] == "fail" {
		return "needs_review"
	}
	required := []string{"leak_probe", "base_fails", "reference_passes", "repeatable", "near_miss_fails", "forgery_probe", "isolation"}
	all := true
	for _, q := range required {
		if st[q] != "pass" {
			all = false
		}
	}
	if all {
		return "validated"
	}
	if st["base_fails"] == "pass" && st["reference_passes"] == "pass" && st["repeatable"] == "pass" {
		return "reproducible"
	}
	return "packaged"
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// leakProbe checks the agent build context and the instruction. It returns a reason code
// or "":
//   - Git metadata in the build context (future history);
//   - a hidden test file's content inside the build context, unless the base holds the
//     same file at the same path;
//   - evaluator files (tests/test.sh, solution/solve.sh) inside the build context;
//   - a line that only the reference or a hidden test holds, quoted in the instruction.
func leakProbe(task string) string {
	env := filepath.Join(task, "environment")
	repo := filepath.Join(env, "repo")
	envText := map[string]string{}
	git := false
	filepath.Walk(env, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() && fi.Name() == ".git" {
			git = true
		}
		if fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(env, p)
			b, _ := os.ReadFile(p)
			envText[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	if git {
		return "future_history_leak"
	}
	for rel := range envText {
		base := filepath.Base(rel)
		if base == "test.sh" && strings.Contains(envText[rel], "/logs/verifier/reward") || base == "solve.sh" && strings.Contains(envText[rel], "/solution/") {
			return "solution_leak"
		}
	}
	instr, _ := os.ReadFile(filepath.Join(task, "instruction.md"))
	distinctive := func(dir string) (lines []string, files map[string]string) {
		files = map[string]string{}
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			rel = filepath.ToSlash(rel)
			b, _ := os.ReadFile(p)
			files[rel] = string(b)
			baseLines := map[string]bool{}
			if old, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel))); err == nil {
				for _, ln := range strings.Split(string(old), "\n") {
					baseLines[strings.TrimSpace(ln)] = true
				}
			}
			for _, ln := range strings.Split(string(b), "\n") {
				if ln = strings.TrimSpace(ln); len(ln) >= 12 && !baseLines[ln] {
					lines = append(lines, ln)
				}
			}
			return nil
		})
		return lines, files
	}
	refLines, _ := distinctive(filepath.Join(task, "solution", "files"))
	testLines, hidden := distinctive(filepath.Join(task, "tests", "hidden"))
	for rel, content := range hidden {
		for envRel, text := range envText {
			if envRel == "repo/"+rel && text == content {
				continue // the base already holds this exact file
			}
			if len(content) > 0 && strings.Contains(text, content) {
				return "solution_leak"
			}
		}
	}
	for _, ln := range append(refLines, testLines...) {
		if strings.Contains(string(instr), ln) {
			return "solution_leak"
		}
	}
	return ""
}
