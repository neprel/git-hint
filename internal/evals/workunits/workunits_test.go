package workunits

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func project(t *testing.T) (*store.Store, *library.Library, string) {
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
	v2["version"], v2["previous"], v2["state"], v2["family"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated", "refactor"
	ref, err := l.PutManifest("eval", v2)
	if err != nil {
		t.Fatal(err)
	}
	if digests[ref], err = l.ManifestDigest("eval", ref); err != nil {
		t.Fatal(err)
	}
	return s, l, ref
}

var launch = "sha256:" + strings.Repeat("a", 64)

// digests holds the immutable record digest of each eval version (set by project).
var digests = map[string]string{}

// planned is a trial input like the real producer writes it for the test manifest: run,
// schedule slot, launch, limits and the exact task binding (objective, baseline, digests).
func planned(ref string, attempt int) suites.TrialInput {
	obj, _, _ := strings.Cut(ref, "@")
	return suites.TrialInput{RunID: "jb-0000000000000001", Suite: "su-0000000000000001", Eval: ref, Attempt: attempt, Arm: "baseline", Agent: "opencode",
		AgentVersion: "2.0.26", Model: "m", RunnerVersion: "0.23.0", MaxSeconds: 900, MaxRequests: 40, ModelCheck: "verified",
		Started: time.Date(2026, 10, 18, 10, attempt, 0, 0, time.UTC), ObservedConfig: []map[string]string{{"kind": "setting_override", "digest": launch}},
		Objective: obj, Baseline: ref, BaselineDigest: digests[ref], EvalDigest: digests[ref]}
}

func manifest(ref string, attempts int) Manifest {
	obj, _, _ := strings.Cut(ref, "@")
	return Manifest{RunID: "jb-0000000000000001", Suite: "su-0000000000000001", Agent: "opencode", AgentVersion: "2.0.26", Arm: "baseline", Attempts: attempts,
		RunnerVersion: "0.23.0", ModelLabel: "m", LaunchDigest: launch, MaxRequests: 40, MaxSeconds: 900,
		Members: []Member{{Eval: ref, EvalDigest: digests[ref], Baseline: ref, BaselineDigest: digests[ref], Objective: obj, TaskFamily: "fam", Eligible: true},
			{Eval: "ev-x@1", Eligible: false}}}
}

func TestAPlannedUnitIsOpenBeforeAnyAttempt(t *testing.T) {
	s, l, ref := project(t)
	if err := Start(s, l, manifest(ref, 2), time.Now()); err != nil {
		t.Fatal(err)
	}
	revs, _ := Unreported(s)
	if len(revs) != 1 || revs[0].(map[string]any)["state"] != "open" || len(revs[0].(map[string]any)["attempts"].([]any)) != 0 {
		t.Fatalf("zero-start unit: %v", revs)
	}
	if err := Start(s, l, manifest(ref, 2), time.Now()); !errors.Is(err, ErrRunExists) {
		t.Fatalf("a restart rewrote the manifest: %v", err)
	}
}

func record(t *testing.T, s *store.Store, l *library.Library, ref string, attempt int, tr runner.Trial, check string) string {
	t.Helper()
	in := planned(ref, attempt)
	in.ModelCheck = check
	tr.Name = "t" + string(rune('0'+attempt))
	n, err := suites.RecordTrial(l, in, tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := Record(s, l, "jb-0000000000000001", ref, n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEveryTrialIsKeptOnceAndClosesTheUnit(t *testing.T) {
	s, l, ref := project(t)
	Start(s, l, manifest(ref, 2), time.Now())
	one := 1.0
	n := record(t, s, l, ref, 1, runner.Trial{Exception: "DockerDaemonError"}, "not_observed") // an infrastructure error
	if err := Record(s, l, "jb-0000000000000001", ref, n); err != nil {                        // the same trial again
		t.Fatal(err)
	}
	record(t, s, l, ref, 2, runner.Trial{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: ptr(10), OutputTokens: ptr(1)}, "verified")
	revs, _ := Unreported(s)
	if len(revs) != 3 {
		t.Fatalf("revisions %d", len(revs))
	}
	last := revs[2].(map[string]any)
	atts := last["attempts"].([]any)
	err0 := atts[0].(map[string]any)
	if last["state"] != "closed" || err0["closure"].(map[string]any)["reason"] != "trial_error" || err0["closure"].(map[string]any)["state"] != "unknown" {
		t.Fatalf("closed unit %v", last)
	}
	if atts[1].(map[string]any)["outcome"] != "completed" {
		t.Fatalf("the later success: %v", atts[1])
	}
	if err := Record(s, l, "jb-0000000000000001", ref, record3(t, s, l, ref)); err == nil {
		t.Fatal("more trials than planned attempts were accepted")
	}
}

func record3(t *testing.T, s *store.Store, l *library.Library, ref string) string {
	in := planned(ref, 3)
	n, err := suites.RecordTrial(l, in, runner.Trial{Name: "t3"})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ptr(v int64) *int64 { return &v }

// CU4: validated completion needs the full non-empty mandatory check set, all passed, the
// trial passed and a validated eval.
func TestValidatedCompletionRules(t *testing.T) {
	s, _, _ := project(t)
	ev := func(checks []string, state string) map[string]any {
		var cs []any
		for _, c := range checks {
			cs = append(cs, map[string]any{"check_id": c})
		}
		return map[string]any{"verifier": map[string]any{"checks": cs}, "state": state}
	}
	tr := func(outcome string, checks map[string]string) map[string]any {
		var cs []any
		for k, v := range checks {
			cs = append(cs, map[string]any{"check_id": k, "status": v})
		}
		return map[string]any{"outcome": outcome, "checks": cs, "attempt": 1.0, "counters": map[string]any{"input_tokens": nil, "output_tokens": 5.0},
			"agent": map[string]any{"model_check": "unverified"}}
	}
	all := map[string]string{"C1": "pass", "C2": "pass"}
	for _, c := range []struct {
		ev           map[string]any
		trial        map[string]any
		outcome, why string
	}{
		{ev([]string{"C1", "C2"}, "validated"), tr("passed", all), "completed", "checks_passed"},
		{ev(nil, "validated"), tr("passed", all), "failed", "incomplete_checks"},                                              // an empty mandatory set
		{ev([]string{"C1", "C2"}, "validated"), tr("passed", map[string]string{"C1": "pass"}), "failed", "incomplete_checks"}, // a missing check
		{ev([]string{"C1", "C2"}, "reproducible"), tr("passed", all), "failed", "not_qualified"},
		{ev([]string{"C1", "C2"}, "validated"), tr("timeout", all), "failed", "trial_failed"},
		{ev([]string{"C1", "C2"}, "validated"), tr("cancelled", all), "failed", "trial_error"},
	} {
		a := attempt(s, "run", "ev@1", "n", c.trial, c.ev)
		if a["outcome"] != c.outcome || a["closure"].(map[string]any)["reason"] != c.why {
			t.Fatalf("%v / %v: %v", c.ev, c.trial["outcome"], a)
		}
		if a["usage_provenance"] != "unknown" || a["model_check"] != "unverified" { // a null count stays null
			t.Fatalf("usage/model check: %v", a)
		}
	}
}

// C7BE-7: a trial that is not the planned one is refused before the journal changes; the
// intended trial still works afterwards; a second record for one planned slot is refused.
func TestOnlyThePlannedTrialEntersTheJournal(t *testing.T) {
	s, l, ref := project(t)
	if err := Start(s, l, manifest(ref, 2), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := Unreported(s)
	one := 1.0
	ok := runner.Trial{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: ptr(10), OutputTokens: ptr(1)}
	for i, change := range []func(*suites.TrialInput){
		func(in *suites.TrialInput) { in.Model = "different-model" }, // verified against another selection
		func(in *suites.TrialInput) {
			in.ObservedConfig = []map[string]string{{"kind": "setting_override", "digest": "sha256:" + strings.Repeat("b", 64)}}
		},
		func(in *suites.TrialInput) { in.MaxRequests = 80 },
		func(in *suites.TrialInput) { in.RunID = "jb-0000000000000002" },
		func(in *suites.TrialInput) { in.RunID = "" }, // no run binding
		func(in *suites.TrialInput) { in.Arm = "configuration" },
		func(in *suites.TrialInput) { in.AgentVersion = "2.0.27" },
		func(in *suites.TrialInput) { in.Attempt = 3 }, // not a planned slot
		func(in *suites.TrialInput) { in.Objective = "ev-0000000000000000" },
		func(in *suites.TrialInput) { in.Objective = "" }, // no objective: the trial record then carries no binding at all
		func(in *suites.TrialInput) { in.Baseline = "ev-0000000000000000@1" },
		func(in *suites.TrialInput) { in.BaselineDigest = "sha256:" + strings.Repeat("c", 64) },
		func(in *suites.TrialInput) { in.EvalDigest = "sha256:" + strings.Repeat("d", 64) },
	} {
		in := planned(ref, 1)
		change(&in)
		ok.Name = fmt.Sprintf("bad%d", i)
		n, err := suites.RecordTrial(l, in, ok)
		if err != nil {
			t.Fatal(err)
		}
		if err := Record(s, l, "jb-0000000000000001", ref, n); err == nil || !strings.Contains(err.Error(), "does not belong") {
			t.Fatalf("case %d accepted: %v", i, err)
		}
		var kept map[string]any
		if err := l.Manifest("trial", n, &kept); err != nil {
			t.Fatalf("case %d: the refused trial record was lost", i)
		}
	}
	if after, _ := Unreported(s); len(after) != len(before) {
		t.Fatal("a refused trial changed the journal")
	}
	record(t, s, l, ref, 1, ok, "verified") // the intended trial still works
	ok.Name = "dup"
	n, _ := suites.RecordTrial(l, planned(ref, 1), ok)
	if err := Record(s, l, "jb-0000000000000001", ref, n); err == nil || !strings.Contains(err.Error(), "already has a trial record") {
		t.Fatalf("a second record for slot 1: %v", err)
	}
	revs, _ := Unreported(s)
	last := revs[len(revs)-1].(map[string]any)
	if last["state"] != "open" || len(last["attempts"].([]any)) != 1 || last["attempts"].([]any)[0].(map[string]any)["ordinal"].(float64) != 1 {
		t.Fatalf("journal %v", last)
	}
}

// a member whose frozen digests do not match the immutable records is refused at start
func TestStartRefusesMemberDigestsThatDoNotMatch(t *testing.T) {
	s, l, ref := project(t)
	m := manifest(ref, 1)
	m.Members[0].BaselineDigest = "sha256:" + strings.Repeat("e", 64)
	if err := Start(s, l, m, time.Now()); err == nil || !strings.Contains(err.Error(), "baseline digest") {
		t.Fatalf("start: %v", err)
	}
	m = manifest(ref, 1)
	m.Members[0].EvalDigest = ""
	if err := Start(s, l, m, time.Now()); err == nil || !strings.Contains(err.Error(), "eval digest") {
		t.Fatalf("start: %v", err)
	}
}

// T1: the trial's plan binding must equal the member's frozen binding (bound or unbound)
func TestThePlanBindingOfATrialMustMatchTheMember(t *testing.T) {
	s, l, ref := project(t)
	m := manifest(ref, 2)
	bound := map[string]any{"recommendation_set_id": strings.Repeat("1", 32), "set_digest": "sha256:" + strings.Repeat("2", 64), "plan_id": strings.Repeat("3", 32),
		"unit_ref": strings.Repeat("4", 32), "revision": 3, "attempt_ref": strings.Repeat("5", 32), "after_snapshot_id": strings.Repeat("6", 32),
		"after_tree": "sha256:" + strings.Repeat("7", 64)}
	m.Members[0].Plan = bound
	if err := Start(s, l, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	one := 1.0
	ok := runner.Trial{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}, InputTokens: ptr(10), OutputTokens: ptr(1)}
	other := map[string]any{}
	for k, v := range bound {
		other[k] = v
	}
	other["plan_id"] = strings.Repeat("9", 32)
	for i, pb := range []map[string]any{nil, other} { // unbound trial, other binding
		in := planned(ref, 1)
		in.PlanBinding = pb
		ok.Name = fmt.Sprintf("pb%d", i)
		n, _ := suites.RecordTrial(l, in, ok)
		if err := Record(s, l, "jb-0000000000000001", ref, n); err == nil || !strings.Contains(err.Error(), "plan binding") {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
	in := planned(ref, 1)
	in.PlanBinding = bound
	ok.Name = "pbok"
	n, _ := suites.RecordTrial(l, in, ok)
	if err := Record(s, l, "jb-0000000000000001", ref, n); err != nil {
		t.Fatalf("the matching binding: %v", err)
	}
}

// C7BE-22: Harbor 0.23.0 + OpenCode counters are root-session only and coerce every missing step
// usage to 0 (07B-E §39): the attempt never claims complete task usage, and a 0 is unknown there;
// the trial keeps the raw counters.
func TestHarborOpenCodeCountersNeverCompleteTaskUsage(t *testing.T) {
	s, _, _ := project(t)
	ev := map[string]any{"verifier": map[string]any{"checks": []any{map[string]any{"check_id": "C1"}}}, "state": "validated"}
	tr := func(in, out, cache any) map[string]any {
		return map[string]any{"outcome": "passed", "checks": []any{map[string]any{"check_id": "C1", "status": "pass"}}, "attempt": 1.0,
			"counters": map[string]any{"requests": 3.0, "input_tokens": in, "output_tokens": out, "cache_tokens": cache},
			"agent":    map[string]any{"name": "opencode", "version": "2.0.26", "model_check": "verified"}, "runner": map[string]any{"name": "harbor", "version": "0.23.0"}}
	}
	for _, c := range []struct {
		name          string
		in, out, cach any
		wantIn        any
		wantCache     any
		prov          string
	}{
		{"positive full-looking aggregate", 1300.0, 200.0, 300.0, 1300.0, 300.0, "harness_reported"},
		{"cache zero is unknown, input kept", 1300.0, 200.0, 0.0, 1300.0, nil, "harness_reported"},
		{"all zero is not a measured zero", 0.0, 0.0, 0.0, nil, nil, "unknown"},
		{"absent stays absent", nil, nil, nil, nil, nil, "unknown"},
		{"reasoning-only output gives output 0", 100.0, 0.0, 0.0, 100.0, nil, "harness_reported"},
	} {
		trial := tr(c.in, c.out, c.cach)
		a := attempt(s, "run", "ev@1", "n-"+strings.ReplaceAll(c.name, " ", "-"), trial, ev)
		u := a["usage"].(map[string]any)
		cov := a["coverage"].(map[string]any)
		if cov["tasks_with_complete_usage"] != 0.0 && cov["tasks_with_complete_usage"] != 0 {
			t.Fatalf("%s: complete task usage claimed: %v", c.name, cov)
		}
		if u["input_tokens"] != c.wantIn || u["cache_tokens"] != c.wantCache || a["usage_provenance"] != c.prov {
			t.Fatalf("%s: usage %v provenance %v", c.name, u, a["usage_provenance"])
		}
		if trial["counters"].(map[string]any)["input_tokens"] != c.in { // the raw trial counters are untouched
			t.Fatalf("%s: raw counters changed", c.name)
		}
		if a["outcome"] != "completed" { // the outcome stays as it was
			t.Fatalf("%s: outcome %v", c.name, a["outcome"])
		}
	}
}
