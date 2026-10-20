// Package qualify runs the automatic qualification of a packaged eval version under the
// named policy q3 and returns the new state with every check result.
//
// Policy q3 (all trials run through the runner; nothing runs on the host):
//   - base_fails: the nop agent fails check C1 (the hidden tests);
//   - reference_passes and repeatable: the oracle passes C1 and C2 in every one of
//     ReferenceRuns clean trials;
//   - near_miss_fails: partial references (one changed file or one change hunk left out)
//     all fail; when the reference is a single hunk, the only partial reference is the
//     empty change, which is the base trial: the check takes the base result;
//   - alternative_passes: every existing alternative solution (Options.Alternatives)
//     passes C1 and C2; missing when there is none (no model is started for one); a
//     claimed alternative that fails gives needs_review (the claim or the verifier is
//     wrong; the result does not say which); independent only when someone claims it,
//     never because the files differ; not required for promotion;
//   - leak_probe: the agent build context holds no hidden test, no reference-only line
//     and no Git metadata;
//   - requirement_coverage: every requirement is linked to tests (a request or correction
//     that neither the local model nor the user linked is missing evidence); the linked
//     tests pass in every reference trial; for requested behavior at least one linked
//     test fails on the base. Per-test results come from the verifier's TEST lines;
//   - forgery_probe and isolation: a solution that tries the network, writes reward
//     files (also from code under test) and adds test harness code that exits early
//     with success (TestMain, conftest.py) must get reward 0 and must see no network;
//     the verifier container, which runs the agent's code with the hidden tests, must
//     record no network either.
//
// State: validated when every required check passes; needs_review when a requirement
// lacks coverage; reproducible when base and reference checks pass but other evidence is
// missing; needs_review or quarantined with reasons otherwise. A missing result never
// counts as a pass. q2 replaced q1, which took the hidden tests as evidence for every
// requirement. q3 (07B-J) keeps every q2 check and adds result_trust: the runner's trust of
// the trials, never raised; a validated q3 state with same_process or unknown trust does
// not meet Q7 and is not authoritative evidence.
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
const Policy = "q3"

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
	Name        string   `json:"name"`
	Status      string   `json:"status"` // pass, fail, missing, not_applicable
	Runs        int      `json:"runs"`
	Independent *bool    `json:"independent,omitempty"`
	ClaimedBy   string   `json:"claimed_by,omitempty"` // who claims the independence
	Evidence    []string `json:"evidence,omitempty"`   // the alternative trees that ran
}

// Alternative is an existing solution that someone states is correct. It is never
// generated here.
type Alternative struct {
	Tree      string // the full result tree (library digest)
	Source    string // provenance, for example "user" or "episode:<id>"
	ClaimedBy string // who claims it is independent of the reference; empty: nobody
}

// Requirement is the coverage result of one requirement.
type Requirement struct {
	ReqID  string `json:"req_id"`
	Status string `json:"status"` // pass, fail, missing
	Reason string `json:"reason,omitempty"`
}

// Result of a qualification.
type Result struct {
	State        string        `json:"state"`
	Reasons      []string      `json:"reasons"`
	Checks       []Check       `json:"checks"`
	Requirements []Requirement `json:"requirements"`
	Trials       int           `json:"trials"`
	// ResultTrust is the minimum runner trust of every trial that ran, each over the exact
	// required check set (07B-J §11-§12); unknown when no trial ran or any is unknown.
	ResultTrust string `json:"result_trust"`
}

// Options of one qualification.
type Options struct {
	Work         string // directory for task copies and job outputs
	Settings     runner.Settings
	Alternatives []Alternative
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
	required := runner.RequiredChecks(rec)
	var trust []string
	run := func(name, agent, taskDir string, attempts int) ([]runner.Trial, error) {
		ts, err := r.Run(ctx, taskDir, runner.RunOptions{Agent: agent, Attempts: attempts, JobsDir: jobs, JobName: name})
		res.Trials += len(ts)
		for _, t := range ts {
			trust = append(trust, t.TrustOver(required))
		}
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
	// a variant without a verified reference (a refactored base where the original change
	// does not apply): base and C2 are observed; reference evidence is missing
	if ref, _ := rec["reference"].(string); ref == "" {
		out, err := noReference(res, rec, nop, baseFails, task, run)
		out.ResultTrust = runner.MinTrust(trust...)
		return out, err
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
	// requirement coverage, from the per-test results of the base and reference trials
	var baseTrial *runner.Trial
	if len(nop) == 1 && !infra(nop[0]) {
		baseTrial = &nop[0]
	}
	cov, reqs, rs := coverage(rec, baseTrial, or, refOK)
	add(cov)
	res.Requirements = reqs
	reason(rs...)
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
	// existing alternative solutions; none is generated
	alt := Check{Name: "alternative_passes", Status: "missing"}
	indep := len(o.Alternatives) > 0
	var claims []string
	passed, failed, missing := 0, 0, 0
	for i, a := range o.Alternatives {
		indep = indep && a.ClaimedBy != ""
		if a.ClaimedBy != "" {
			claims = append(claims, a.ClaimedBy)
		}
		dir, err := alternativeTask(l, task, a.Tree, i+1)
		if err != nil {
			return res, err
		}
		alt.Evidence = append(alt.Evidence, a.Tree)
		ts, err := run(fmt.Sprintf("alternative-%d", i+1), "oracle", dir, 1)
		switch {
		case err != nil || len(ts) != 1 || infra(ts[0]):
			missing++
		case ts[0].Outcome() == "passed" && ts[0].Checks["C1"] == "pass" && ts[0].Checks["C2"] == "pass":
			passed++
		default:
			failed++
		}
	}
	alt.Runs = passed + failed
	switch {
	case failed > 0:
		alt.Status = "fail"
		reason("alternative_fails")
	case missing > 0:
		// not required for promotion: the check shows missing, no reason is added
	case passed > 0:
		alt.Status = "pass"
	}
	alt.Independent, alt.ClaimedBy = &indep, strings.Join(uniq(claims), ",")
	add(alt)
	// forgery and isolation probe
	probe, err := probeTask(task)
	if err != nil {
		return res, err
	}
	pt, err := run("probe", "oracle", probe, 1)
	// the probe is judged by its reward value: an inconsistent reward is a forgery here,
	// not an infrastructure error
	if err != nil || len(pt) != 1 || pt[0].Exception != "" {
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
		// the agent container and the verifier container must both have no network
		net, vnet := netSeen(filepath.Join(jobs, "probe")), verifierNetSeen(filepath.Join(jobs, "probe"))
		switch {
		case net != "blocked" || vnet == "reachable":
			add(Check{Name: "isolation", Status: "fail", Runs: 1})
			reason("isolation_failed")
		case vnet == "":
			add(Check{Name: "isolation", Status: "missing", Runs: 1}) // a verifier without the network record
		default:
			add(Check{Name: "isolation", Status: "pass", Runs: 1})
		}
	}
	res.State = decide(res)
	res.Reasons = uniq(res.Reasons)
	res.ResultTrust = runner.MinTrust(trust...)
	return res, nil
}

// noReference finishes the qualification of a record without a reference: the base
// trial gives base_fails and the kept behavior (C2) of preserve requirements; every check
// that needs the reference is missing; the state is needs_review (reference_missing).
func noReference(res Result, rec map[string]any, nop []runner.Trial, baseFails bool, task string, run func(string, string, string, int) ([]runner.Trial, error)) (Result, error) {
	add := func(c Check) { res.Checks = append(res.Checks, c) }
	for _, n := range []string{"reference_passes", "repeatable", "near_miss_fails"} {
		add(Check{Name: n, Status: "missing"})
	}
	var base *runner.Trial
	if len(nop) == 1 && !infra(nop[0]) {
		base = &nop[0]
	}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		q := Requirement{ReqID: m["req_id"].(string), Status: "missing", Reason: "reference_missing"}
		if m["kind"] == "preserve" && base != nil {
			// kept behavior is observed on the base itself
			q.Status, q.Reason = status(base.Checks["C2"] == "pass"), ""
		}
		res.Requirements = append(res.Requirements, q)
	}
	add(Check{Name: "requirement_coverage", Status: "missing"})
	no := false
	add(Check{Name: "alternative_passes", Status: "missing", Independent: &no})
	probe, err := probeTask(task)
	if err != nil {
		return res, err
	}
	pt, err := run("probe", "oracle", probe, 1)
	switch {
	case err != nil || len(pt) != 1 || pt[0].Exception != "":
		add(Check{Name: "forgery_probe", Status: "missing", Runs: len(pt)})
		add(Check{Name: "isolation", Status: "missing", Runs: len(pt)})
	default:
		forged := pt[0].Reward != nil && *pt[0].Reward > 0
		switch {
		case !baseFails:
			add(Check{Name: "forgery_probe", Status: "not_applicable", Runs: 1})
		case forged:
			add(Check{Name: "forgery_probe", Status: "fail", Runs: 1})
			res.Reasons = append(res.Reasons, "verifier_forgeable")
		default:
			add(Check{Name: "forgery_probe", Status: "pass", Runs: 1})
		}
		jobs := filepath.Join(filepath.Dir(task), "jobs", "probe")
		net, vnet := netSeen(jobs), verifierNetSeen(jobs)
		switch {
		case net != "blocked" || vnet == "reachable":
			add(Check{Name: "isolation", Status: "fail", Runs: 1})
			res.Reasons = append(res.Reasons, "isolation_failed")
		case vnet == "":
			add(Check{Name: "isolation", Status: "missing", Runs: 1})
		default:
			add(Check{Name: "isolation", Status: "pass", Runs: 1})
		}
	}
	res.Reasons = append(res.Reasons, "reference_missing")
	res.State = decide(res)
	if res.State != "quarantined" {
		res.State = "needs_review" // never reproducible or validated without a reference
	}
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
	if st["base_fails"] == "fail" || st["repeatable"] == "fail" || st["near_miss_fails"] == "fail" || st["alternative_passes"] == "fail" {
		return "needs_review"
	}
	if st["requirement_coverage"] == "fail" || st["requirement_coverage"] == "missing" && st["reference_passes"] == "pass" {
		return "needs_review"
	}
	required := []string{"leak_probe", "base_fails", "reference_passes", "repeatable", "near_miss_fails", "requirement_coverage", "forgery_probe", "isolation"}
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

// coverage checks the requirement-to-test matrix of rec against the base trial and the
// reference trials. Tests are named "C1 file::Name" in the trial results.
func coverage(rec map[string]any, base *runner.Trial, refs []runner.Trial, refOK bool) (Check, []Requirement, []string) {
	var out []Requirement
	var reasons []string
	missing, failed := false, false
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		id, _ := m["req_id"].(string)
		var checks, tests []string
		for _, c := range anyList(m["checks"]) {
			checks = append(checks, c)
		}
		for _, t := range anyList(m["tests"]) {
			tests = append(tests, t)
		}
		res := Requirement{ReqID: id, Status: "pass"}
		switch {
		case m["mapping"] == "none" || len(checks) == 0:
			res.Status, res.Reason = "missing", "requirement_coverage_missing"
		case !refOK || base == nil:
			res.Status, res.Reason = "missing", "runner_unavailable"
		default:
			// every linked check (or test) passes in every reference trial
			for _, t := range refs {
				for _, c := range checks {
					if t.Checks[c] != "pass" {
						res.Status, res.Reason = "fail", "reference_fails"
					}
				}
				for _, x := range tests {
					if t.Tests[checks[0]+" "+x] != "pass" {
						res.Status, res.Reason = "fail", "reference_fails"
					}
				}
			}
			// requested behavior: at least one linked check (or test) fails on the base
			if res.Status == "pass" && m["kind"] != "preserve" {
				disc := false
				if len(tests) > 0 {
					for _, x := range tests {
						disc = disc || base.Tests[checks[0]+" "+x] == "fail"
					}
				} else {
					for _, c := range checks {
						disc = disc || base.Checks[c] == "fail"
					}
				}
				if !disc {
					res.Status, res.Reason = "fail", "requirement_not_discriminating"
				}
			}
		}
		switch res.Status {
		case "missing":
			missing = true
		case "fail":
			failed = true
		}
		if res.Reason != "" {
			reasons = append(reasons, res.Reason)
		}
		out = append(out, res)
	}
	c := Check{Name: "requirement_coverage", Status: "pass", Runs: len(refs)}
	switch {
	case failed:
		c.Status = "fail"
	case missing:
		c.Status = "missing"
	}
	return c, out, reasons
}

func anyList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
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
