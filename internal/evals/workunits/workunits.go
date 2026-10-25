// Package workunits journals controlled work units of eval runs (eval report v0.2 work_units,
// origin controlled): the run manifest is written before the first attempt, every planned
// eligible objective gets an open unit at once (also one that is never reached), and every
// terminal trial record appends one immutable revision. Reports serialize the journal; they
// never regenerate it. Only live agents with a refactor-family eval form cost units; nop and
// oracle runs are controls.
package workunits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runjournal"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	projectstate "github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// Neutral aliases preserve the existing producer API and JSON bytes.
type Member = runjournal.Member
type Manifest = runjournal.Manifest
type Unit = runjournal.Unit
type Run = runjournal.Run

var ErrRunExists = errors.New("the run manifest exists already")

// runIDPattern is the form of every run ID ("jb-" and 16 hex digits); nothing else names a
// stored run file.
var runIDPattern = regexp.MustCompile(`^jb-[0-9a-f]{16}$`)

// ReadManifest returns the stored manifest of a run (written once by Start): the run ID must
// have the run form, and the stored file must be a run whose manifest names that run.
func ReadManifest(s *store.Store, runID string) (Manifest, error) {
	if !runIDPattern.MatchString(runID) {
		return Manifest{}, errors.New("not a run ID")
	}
	var r Run
	if err := s.ReadJSON(&r, "state", "evals", "work", runFile(runID)); err != nil {
		return Manifest{}, err
	}
	if r.Manifest.RunID != runID || r.Manifest.Agent == "" || r.Manifest.RunnerVersion == "" || r.Manifest.CreatedAt == "" {
		return Manifest{}, errors.New("the stored run is not a usable manifest")
	}
	return r.Manifest, nil
}

func runFile(id string) string { return id + ".json" }

// Start stores the manifest and opens a unit for every eligible member (revision 1, no
// attempt). A manifest is written once.
func Start(s *store.Store, l *library.Library, m Manifest, now time.Time) error {
	var old Run
	if err := s.ReadJSON(&old, "state", "evals", "work", runFile(m.RunID)); err == nil {
		return ErrRunExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	r := &Run{Manifest: m}
	for _, mb := range m.Members {
		if !mb.Eligible {
			continue
		}
		var ev map[string]any
		if err := l.Manifest("eval", mb.Eval, &ev); err != nil {
			return err
		}
		// the frozen versions: the member's digests must be those of the immutable records
		if d, err := l.ManifestDigest("eval", mb.Eval); err != nil || d != mb.EvalDigest || mb.EvalDigest == "" {
			return fmt.Errorf("member %s: the eval digest does not match its record", mb.Eval)
		}
		if d, err := l.ManifestDigest("eval", mb.Baseline); err != nil || d != mb.BaselineDigest || mb.BaselineDigest == "" {
			return fmt.Errorf("member %s: the baseline digest does not match its record", mb.Eval)
		}
		ref := func(kind string, parts ...string) string { return hint.EvalRef(s.Key(), kind, parts...) }
		cfg := ScopeConfiguration(s.Key(), m)
		scope := map[string]any{"declared_at": now.UTC().Truncate(time.Hour).Format(time.RFC3339), "plan": nil,
			"objective_ref": ref("objective", mb.Objective), "base_snapshots": []any{}, "family_ref": ref("family", mb.TaskFamily),
			"configuration": cfg, "environment_ref": ref("environment", report.EnvironmentIdentity(ev)...), "run_ref": ref("run", m.RunID),
			"eval_ref": ref("eval", mb.Eval), "baseline_ref": ref("eval", mb.Baseline), "arm": m.Arm, "planned_attempts": m.Attempts}
		u := &Unit{Eval: mb.Eval, UnitRef: ref("work-unit", "controlled", m.RunID, mb.Eval), Scope: plain(scope).(map[string]any), State: "open",
			Attempts: []map[string]any{}, Trials: []string{}}
		if err := u.Revise(); err != nil {
			return err
		}
		r.Units = append(r.Units, u)
	}
	return s.WriteJSON(r, "state", "evals", "work", runFile(m.RunID))
}

// Record appends the terminal trial record `name` of member `eval` as an attempt (once).
func Record(s *store.Store, l *library.Library, runID, eval, name string) error {
	var r Run
	if err := s.ReadJSON(&r, "state", "evals", "work", runFile(runID)); err != nil {
		return err
	}
	var u *Unit
	for _, x := range r.Units {
		if x.Eval == eval {
			u = x
		}
	}
	if u == nil {
		return nil // not an eligible member: no cost unit
	}
	if slices.Contains(u.Trials, name) {
		return nil // the same trial record again
	}
	var t, ev map[string]any
	if err := l.Manifest("trial", name, &t); err != nil {
		return err
	}
	if err := l.Manifest("eval", eval, &ev); err != nil {
		return err
	}
	// the trial must be the one this run planned for this member (CU3, C7BE-7): checked before
	// the journal changes; a refused trial record stays in the library with its spent usage and
	// the planned slot stays open
	var member *Member
	for i := range r.Manifest.Members {
		if r.Manifest.Members[i].Eval == eval {
			member = &r.Manifest.Members[i]
		}
	}
	if member == nil {
		return fmt.Errorf("trial %s: %s is not a member of this run", name, eval)
	}
	if err := bound(r.Manifest, *member, t); err != nil {
		return fmt.Errorf("trial %s does not belong to this run's plan: %w", name, err)
	}
	ord := int(num(t["attempt"]))
	for _, a := range u.Attempts {
		if int(num(a["ordinal"])) == ord {
			return fmt.Errorf("trial %s: planned attempt %d already has a trial record", name, ord)
		}
	}
	u.Attempts = append(u.Attempts, attempt(s, r.Manifest.RunID, eval, name, t, ev))
	u.Trials = append(u.Trials, name)
	if len(u.Attempts) == r.Manifest.Attempts {
		u.State = "closed" // every planned attempt has a terminal record
	}
	if err := u.Revise(); err != nil {
		return err
	}
	return s.WriteJSON(r, "state", "evals", "work", runFile(runID))
}

// ScopeConfiguration is the canonical configuration of a controlled run (the scope of each of
// its units, and the configuration its model ledgers are bound to).
func ScopeConfiguration(key []byte, m Manifest) map[string]any {
	return runjournal.ScopeConfiguration(key, m)
}

// attempt is the report form of one terminal trial (CU3/CU4).
func attempt(s *store.Store, run, eval, name string, t, ev map[string]any) map[string]any {
	ref := func(kind string, parts ...string) string { return hint.EvalRef(s.Key(), kind, parts...) }
	outcome, _ := t["outcome"].(string)
	var mandatory []string
	if v, _ := ev["verifier"].(map[string]any); v != nil {
		for _, c := range list(v["checks"]) {
			if m, _ := c.(map[string]any); m != nil {
				mandatory = append(mandatory, str(m["check_id"]))
			}
		}
	}
	status := map[string]string{}
	for _, c := range list(t["checks"]) {
		if m, _ := c.(map[string]any); m != nil {
			status[str(m["check_id"])] = str(m["status"])
		}
	}
	present, passed := 0, 0
	for _, c := range mandatory {
		if st, ok := status[c]; ok {
			present++
			if st == "pass" {
				passed++
			}
		}
	}
	qual := str(ev["state"])
	tr := ref("trial", name)
	closure := map[string]any{"state": "not_validated", "reason": "trial_failed"}
	out := "failed"
	switch {
	case outcome == "passed" && len(mandatory) > 0 && present == len(mandatory) && passed == len(mandatory) && qual == "validated":
		out, closure = "completed", map[string]any{"state": "validated", "validation_ref": tr, "reason": "checks_passed"}
	case outcome == "passed" && qual != "validated":
		closure["reason"] = "not_qualified"
	case outcome == "passed":
		closure["reason"] = "incomplete_checks"
	case outcome == "infrastructure_error" || outcome == "verifier_error" || outcome == "cancelled":
		closure = map[string]any{"state": "unknown", "reason": "trial_error"} // kept; coverage incomplete
	}
	// the trial's runner trust capped by the qualification's (07B-J §11.4(8)); absent is
	// unknown and nothing raises it
	q, _ := ev["qualification"].(map[string]any)
	closure["result_trust"] = runner.MinTrust(str(t["result_trust"]), str(q["result_trust"]))
	c, _ := t["counters"].(map[string]any)
	usage := map[string]any{"requests": c["requests"], "input_tokens": c["input_tokens"], "output_tokens": c["output_tokens"], "cache_tokens": c["cache_tokens"]}
	prov, complete := "unknown", 0
	if ag, _ := t["agent"].(map[string]any); ag != nil && str(ag["name"]) == "opencode" {
		// Harbor 0.23.0 + OpenCode counters (harbor-0.23.0+opencode:n-tokens-root-v1, 07B-E §39,
		// C7BE-22): root session only (children, title and auxiliary calls are never counted) and
		// every missing step usage becomes 0. The trial keeps the raw counters; the attempt never
		// claims complete task usage, and a 0 here is unknown, not a measured zero.
		for _, k := range []string{"input_tokens", "output_tokens", "cache_tokens"} {
			if v, ok := c[k].(float64); ok && v == 0 {
				usage[k] = nil
			}
		}
		if _, ok := usage["input_tokens"].(float64); ok {
			prov = "harness_reported"
		} else if _, ok := usage["output_tokens"].(float64); ok {
			prov = "harness_reported"
		}
	} else {
		_, okIn := c["input_tokens"].(float64)
		_, okOut := c["output_tokens"].(float64)
		if okIn && okOut { // the harness's token counts; null stays null
			prov, complete = "harness_reported", 1
		}
	}
	check := "not_observed"
	if ag, _ := t["agent"].(map[string]any); ag != nil && str(ag["model_check"]) != "" {
		check = str(ag["model_check"])
	}
	n := fmt.Sprint(int(num(t["attempt"])))
	return plain(map[string]any{"attempt_ref": ref("attempt", "controlled", run, eval, n), "task_ids": controlledTaskIDs(s, t), "outcome": out, "closure": closure,
		"application_change_set": nil, "usage": usage, "usage_provenance": prov, "coverage": map[string]any{"tasks": 1, "tasks_with_complete_usage": complete},
		"trial_ref": tr, "trial_outcome": outcome, "model_check": check, "ordinal": int(num(t["attempt"])),
		"checks": map[string]any{"mandatory": len(mandatory), "present": present, "passed": passed}, "qualification": nullable(qual)}).(map[string]any)
}

// Unreported returns the journaled revisions not yet sent, in run and unit order.
func Unreported(s *store.Store) ([]any, error) {
	runs, err := runs(s)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, r := range runs {
		for _, u := range r.Units {
			for _, rv := range u.Revisions {
				if d, _ := rv["revision_digest"].(string); !slices.Contains(u.Reported, d) {
					out = append(out, rv)
				}
			}
		}
	}
	return out, nil
}

// MarkReported records sent revisions.
func MarkReported(s *store.Store, revisions []any) error {
	runs, err := runs(s)
	if err != nil {
		return err
	}
	for _, r := range runs {
		changed := false
		for _, u := range r.Units {
			for _, x := range revisions {
				m, _ := x.(map[string]any)
				d, _ := m["revision_digest"].(string)
				if m["unit_ref"] == u.UnitRef && !slices.Contains(u.Reported, d) {
					u.Reported = append(u.Reported, d)
					changed = true
				}
			}
		}
		if changed {
			if err := s.WriteJSON(r, "state", "evals", "work", runFile(r.Manifest.RunID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func runs(s *store.Store) ([]*Run, error) {
	files, err := filepath.Glob(filepath.Join(s.Path("state", "evals", "work"), "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*Run
	for _, f := range files {
		var r Run
		if err := s.ReadJSON(&r, "state", "evals", "work", filepath.Base(f)); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, nil
}

func version(v string) map[string]any {
	if v == "" {
		return map[string]any{"state": "unknown"}
	}
	return map[string]any{"state": "known", "value": v}
}

func limit(v int) map[string]any {
	if v < 1 {
		return map[string]any{"state": "unknown"}
	}
	return map[string]any{"state": "known", "value": v}
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func plain(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func list(v any) []any { l, _ := v.([]any); return l }

func str(v any) string { s, _ := v.(string); return s }

// bound checks that a trial record is the planned one: run, suite, member, arm, agent and
// version, model, runner version, limits, the launch configuration digest and an ordinal of
// the schedule.
func bound(m Manifest, mb Member, t map[string]any) error {
	eval := mb.Eval
	ag, _ := t["agent"].(map[string]any)
	bind, _ := t["binding"].(map[string]any)
	run, _ := t["runner"].(map[string]any)
	bud, _ := t["budget"].(map[string]any)
	launch := ""
	for _, c := range list(t["observed_config"]) {
		if x, _ := c.(map[string]any); x != nil && str(x["kind"]) == "setting_override" {
			launch = str(x["digest"])
		}
	}
	ord := num(t["attempt"])
	checks := []struct {
		what      string
		got, want any
	}{
		{"run", str(t["run_id"]), m.RunID}, {"suite", str(t["suite_id"]), m.Suite}, {"eval", str(t["eval"]), eval}, {"arm", str(t["arm"]), m.Arm},
		{"agent", str(ag["name"]), m.Agent}, {"agent version", str(ag["version"]), m.AgentVersion}, {"model", str(ag["model"]), m.ModelLabel},
		{"runner version", str(run["version"]), m.RunnerVersion}, {"time limit", num(bud["max_seconds"]), float64(m.MaxSeconds)},
		{"request limit", num(bud["max_requests"]), float64(m.MaxRequests)}, {"launch configuration", launch, m.LaunchDigest},
		// the exact task: objective, baseline version and both digests (a missing value is a
		// refusal, never filled in from the plan)
		{"objective", str(t["objective"]), mb.Objective}, {"baseline", str(bind["baseline"]), mb.Baseline},
		{"baseline digest", str(bind["baseline_digest"]), mb.BaselineDigest}, {"eval digest", str(bind["eval_digest"]), mb.EvalDigest},
		{"plan binding", canon(t["plan_binding"]), canon(mb.Plan)},
	}
	for _, c := range checks {
		if c.got != c.want || c.got == "" {
			return fmt.Errorf("%s differs", c.what)
		}
	}
	if ord < 1 || int(ord) > m.Attempts || ord != float64(int(ord)) {
		return fmt.Errorf("attempt %v is not a planned slot", t["attempt"])
	}
	return nil
}

func num(v any) float64 { f, _ := v.(float64); return f }

// canon is the canonical JSON of a plan binding ("unbound" when absent).
func canon(v any) string {
	if m, _ := v.(map[string]any); len(m) == 0 {
		return "unbound"
	}
	b, _ := json.Marshal(plain(v))
	return string(b)
}

// controlledTaskIDs uses the run's declared objective task, exactly as its ledger/behavior.
// It is not the eval report's optional original source-session task provenance.
func controlledTaskIDs(s *store.Store, t map[string]any) []any {
	objective := str(t["objective"])
	if objective == "" {
		return []any{}
	}
	return []any{projectstate.TaskID(s.Key(), objective)}
}
