package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

// A work unit is an explicitly declared piece of work (for example a refactoring) whose cost
// HINT attributes: its scope is frozen at start, its member tasks are the agent runs declared
// with HINT_WORK_UNIT (and HINT_TASK_ID), and every change of state is a new immutable revision
// (eval report v0.2 work_units). It declares work; it never claims a correct refactoring or an
// effect. The raw label stays local.

// WorkAttempt is one attempt of a work unit.
type WorkAttempt struct {
	Ref        string   `json:"attempt_ref"`
	Tasks      []string `json:"task_ids"`
	Outcome    string   `json:"outcome"` // open, completed, failed, abandoned
	Closure    string   `json:"closure"` // validated, not_validated, unknown
	Validation string   `json:"validation_task_id,omitempty"`
	ClosedAt   string   `json:"closed_at,omitempty"`
	Late       []string `json:"late_tasks,omitempty"`
}

// WorkUnit is the local state of one unit.
type WorkUnit struct {
	Label     string           `json:"label"` // raw, local only
	UnitRef   string           `json:"unit_ref"`
	Scope     map[string]any   `json:"scope"`
	PlanRepo  string           `json:"plan_repo,omitempty"`
	PlanSnap  string           `json:"plan_snapshot,omitempty"`
	PlanID    string           `json:"plan_id,omitempty"`
	State     string           `json:"state"` // open, closed
	Attempts  []WorkAttempt    `json:"attempts"`
	Revisions []map[string]any `json:"revisions"` // immutable, in order
	Reported  []string         `json:"reported,omitempty"`
}

var (
	ErrWorkUnknown = errors.New("unknown work unit")
	ErrWorkExists  = errors.New("the work unit exists already")
)

func workFile(s *store.Store, label string) string {
	return hint.DeriveID(s.Key(), "work-file", label) + ".json"
}

// LoadWork reads one unit (ErrWorkUnknown when it does not exist).
func LoadWork(s *store.Store, label string) (*WorkUnit, error) {
	var u WorkUnit
	if err := s.ReadJSON(&u, "state", "work", workFile(s, label)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrWorkUnknown
		}
		return nil, err
	}
	return &u, nil
}

// WorkUnits returns every local unit, sorted by unit reference.
func WorkUnits(s *store.Store) ([]*WorkUnit, error) {
	var out []*WorkUnit
	files, err := filepath.Glob(filepath.Join(s.Path("state", "work"), "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		f = filepath.Base(f)
		var u WorkUnit
		if err := s.ReadJSON(&u, "state", "work", f); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UnitRef < out[j].UnitRef })
	return out, nil
}

func saveWork(s *store.Store, u *WorkUnit) error {
	return s.WriteJSON(u, "state", "work", workFile(s, u.Label))
}

// WorkStart declares a unit with its frozen scope: the base snapshots (the current latest of
// every repository), the plan (checked by the existing importer; the work starts at the plan's
// snapshot), the declared family and the configuration of the run. Revision 1 is open.
func WorkStart(s *store.Store, label string, setRaw []byte, planID, family string, cfg *activity.Configuration, now time.Time) (*WorkUnit, error) {
	if !ValidTaskID(label) || !ValidTaskID(family) {
		return nil, errors.New("the work unit label and the family must be 1-200 bytes of printable text")
	}
	if _, err := LoadWork(s, label); err == nil {
		return nil, ErrWorkExists
	} else if !errors.Is(err, ErrWorkUnknown) {
		return nil, err
	}
	u := &WorkUnit{Label: label, UnitRef: hint.EvalRef(s.Key(), "work-unit", label), State: "open", Attempts: []WorkAttempt{}}
	var bases []any
	latest := map[string]string{}
	for _, r := range s.Cfg.Repos {
		l, err := LoadLatest(s, s.RepoID(r))
		if err != nil {
			return nil, err
		}
		if l == nil {
			return nil, fmt.Errorf("repository %q has no snapshot; run `git hint collect` first", r.Name)
		}
		latest[l.Record.RepoID] = l.Record.SnapshotID
		bases = append(bases, l.Record.SnapshotID)
	}
	var plan any
	if planID != "" {
		pr, err := resolvePlan(s, setRaw, planID, now)
		if err != nil {
			return nil, err
		}
		if latest[pr.RepoID] != pr.SnapshotID {
			return nil, fmt.Errorf("%s: the plan was made for another snapshot than the current one; collect or fetch a new plan first", pr.RepoName)
		}
		plan = map[string]any{"recommendation_set_id": pr.SetID, "plan_id": pr.PlanID}
		u.PlanRepo, u.PlanSnap, u.PlanID = pr.RepoID, pr.SnapshotID, pr.PlanID
	}
	var c map[string]any
	b, _ := json.Marshal(cfg)
	_ = json.Unmarshal(b, &c)
	u.Scope = map[string]any{"declared_at": now.UTC().Truncate(time.Hour).Format(time.RFC3339), "plan": plan, "objective_ref": nil,
		"base_snapshots": bases, "family_ref": activity.FamilyRef(s.Key(), family), "configuration": c,
		"environment_ref": nil, "run_ref": nil, "eval_ref": nil, "baseline_ref": nil, "arm": nil, "planned_attempts": nil}
	if err := u.revise(s, now); err != nil {
		return nil, err
	}
	return u, saveWork(s, u)
}

// WorkJoin adds a member task (an agent run with HINT_WORK_UNIT and HINT_TASK_ID); called by
// the adapter before it writes. An open unit gets the task in its open attempt (a new attempt
// after a failed one); a closed unit records it as late activity (the unit becomes stale for
// cost, nothing is dropped). A task of another unit is refused.
func WorkJoin(s *store.Store, label, taskID string, now time.Time) error {
	u, err := LoadWork(s, label)
	if err != nil {
		return err
	}
	all, err := WorkUnits(s)
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.UnitRef == u.UnitRef {
			continue
		}
		for _, a := range o.Attempts {
			if slices.Contains(a.Tasks, taskID) {
				return errors.New("the task belongs to another work unit")
			}
		}
	}
	if u.State == "closed" {
		last := &u.Attempts[len(u.Attempts)-1]
		if slices.Contains(last.Late, taskID) {
			return nil
		}
		last.Late = append(last.Late, taskID)
	} else {
		if len(u.Attempts) == 0 || u.Attempts[len(u.Attempts)-1].Outcome != "open" {
			u.Attempts = append(u.Attempts, WorkAttempt{Ref: hint.EvalRef(s.Key(), "attempt", label, strconv.Itoa(len(u.Attempts)+1)),
				Tasks: []string{}, Outcome: "open", Closure: "unknown"})
		}
		a := &u.Attempts[len(u.Attempts)-1]
		if slices.Contains(a.Tasks, taskID) {
			return nil
		}
		a.Tasks = append(a.Tasks, taskID)
	}
	if err := u.revise(s, now); err != nil {
		return err
	}
	return saveWork(s, u)
}

// WorkClose ends the open attempt. completed needs a passed test of a member task after the
// last edit of every member task (the bound validation) and closes the unit; failed keeps the
// unit open for another attempt; abandoned closes it.
func WorkClose(s *store.Store, label, outcome string, now time.Time) (*WorkUnit, error) {
	u, err := LoadWork(s, label)
	if err != nil {
		return nil, err
	}
	if u.State != "open" || len(u.Attempts) == 0 || u.Attempts[len(u.Attempts)-1].Outcome != "open" {
		return nil, errors.New("the work unit has no open attempt")
	}
	a := &u.Attempts[len(u.Attempts)-1]
	if len(a.Tasks) == 0 {
		return nil, errors.New("the open attempt has no member task (run the agent with HINT_WORK_UNIT and HINT_TASK_ID)")
	}
	switch outcome {
	case "completed":
		v, err := validation(s, a.Tasks)
		if err != nil {
			return nil, err
		}
		if v == "" {
			return nil, errors.New("no passed test of a member task after the last edit of every member task; the attempt is not completed")
		}
		a.Closure, a.Validation, u.State = "validated", v, "closed"
	case "failed":
		a.Closure = "not_validated"
	case "abandoned":
		a.Closure, u.State = "unknown", "closed"
	default:
		return nil, errors.New("the outcome must be completed, failed or abandoned")
	}
	a.Outcome, a.ClosedAt = outcome, now.UTC().Format(time.RFC3339Nano)
	if err := u.revise(s, now); err != nil {
		return nil, err
	}
	return u, saveWork(s, u)
}

// validation returns the member task of a passed test after the last edit of every member
// task, or "".
func validation(s *store.Store, tasks []string) (string, error) {
	obs, _, err := ReadSpool(s)
	if err != nil {
		return "", err
	}
	var lastEdit time.Time
	type test struct {
		at   time.Time
		task string
	}
	var tests []test
	for _, o := range obs {
		task := o.Correlation["task_id"]
		if !slices.Contains(tasks, task) || o.Time.Start == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, *o.Time.Start)
		if err != nil {
			continue
		}
		switch {
		case o.Op == "EDIT_INTENT" || o.Op == "FILE_MUTATION":
			if at.After(lastEdit) {
				lastEdit = at
			}
		case o.Op == "TEST" && o.Outcome.Status == "success":
			tests = append(tests, test{at, task})
		}
	}
	for _, t := range tests {
		if t.at.After(lastEdit) {
			return t.task, nil
		}
	}
	return "", nil
}

// revise appends a new immutable revision when the reported form changed (usage and late
// activity of member tasks are read from the activity spool).
func (u *WorkUnit) revise(s *store.Store, now time.Time) error {
	attempts, err := u.reportAttempts(s)
	if err != nil {
		return err
	}
	scopeDigest, err := hint.Digest(plainOf(u.Scope))
	if err != nil {
		return err
	}
	var prev any
	if n := len(u.Revisions); n > 0 {
		last := u.Revisions[n-1]
		if last["state"] == u.State && equalJSON(last["attempts"], attempts) {
			return nil // nothing changed
		}
		prev = last["revision_digest"]
	}
	r := map[string]any{"unit_ref": u.UnitRef, "origin": "project", "scope": u.Scope, "scope_digest": scopeDigest, "revision": len(u.Revisions) + 1,
		"predecessor": prev, "state": u.State, "attempts": attempts, "declared_by": "user"}
	d, err := hint.Digest(plainOf(r))
	if err != nil {
		return err
	}
	r["revision_digest"] = d
	u.Revisions = append(u.Revisions, plainOf(r).(map[string]any))
	return nil
}

// Revise updates late activity (member task records after closure) and appends a revision
// when something changed; used before a report.
func (u *WorkUnit) Revise(s *store.Store, now time.Time) error {
	if u.State == "closed" {
		obs, _, err := ReadSpool(s)
		if err != nil {
			return err
		}
		last := &u.Attempts[len(u.Attempts)-1]
		closed, _ := time.Parse(time.RFC3339Nano, last.ClosedAt)
		for _, a := range u.Attempts {
			for _, o := range obs {
				task := o.Correlation["task_id"]
				if slices.Contains(a.Tasks, task) && o.Time.Start != nil && !slices.Contains(last.Late, task) {
					if at, err := time.Parse(time.RFC3339Nano, *o.Time.Start); err == nil && at.After(closed) {
						last.Late = append(last.Late, task)
					}
				}
			}
		}
	}
	if err := u.revise(s, now); err != nil {
		return err
	}
	return saveWork(s, u)
}

// reportAttempts is the eval report form of the attempts. Usage is the sum over the member
// tasks only when every member task has complete model-call coverage under one usage
// normalization (activity.UsageCoverage, 07B-E §33); otherwise the counts stay unknown (null,
// never zero or a partial sum). The model-request count stays unknown: steps and physical
// requests are not proven one-to-one (retries).
func (u *WorkUnit) reportAttempts(s *store.Store) ([]any, error) {
	obs, _, err := ReadSpool(s)
	if err != nil {
		return nil, err
	}
	srcs, err := Sources(s)
	if err != nil {
		return nil, err
	}
	byTask := map[string][]activity.Observation{}
	for _, o := range obs {
		if t := o.Correlation["task_id"]; t != "" {
			byTask[t] = append(byTask[t], o)
		}
	}
	res := []any{}
	for _, a := range u.Attempts {
		closure := map[string]any{"state": a.Closure}
		switch {
		case a.Validation != "":
			closure["validation_task_id"], closure["reason"] = a.Validation, "validation_task_passed"
		case a.Closure == "not_validated":
			closure["reason"] = "not_validated"
		default:
			closure["reason"] = "no_validation"
		}
		complete, norms := 0, map[string]bool{}
		var in, out, cache float64
		cacheKnown := true
		for _, t := range a.Tasks {
			c := activity.UsageCoverage(byTask[t], srcs)
			if c.State != "complete" {
				continue
			}
			complete++
			norms[c.Normalization] = true
			in, out = in+c.Input, out+c.Output
			if c.Cache == nil {
				cacheKnown = false
			} else {
				cache += *c.Cache
			}
		}
		usage := map[string]any{"requests": nil, "input_tokens": nil, "output_tokens": nil, "cache_tokens": nil}
		prov, norm := "unknown", any(nil)
		if len(a.Tasks) > 0 && complete == len(a.Tasks) && len(norms) == 1 {
			usage["input_tokens"], usage["output_tokens"] = in, out
			if cacheKnown {
				usage["cache_tokens"] = cache
			}
			prov = "activity_usage"
			for n := range norms {
				norm = n
			}
		}
		ra := map[string]any{"attempt_ref": a.Ref, "task_ids": a.Tasks, "outcome": a.Outcome, "closure": closure,
			"application_change_set": nil, "usage": usage, "usage_provenance": prov, "usage_normalization": norm,
			"coverage": map[string]any{"tasks": len(a.Tasks), "tasks_with_complete_usage": complete}}
		if len(a.Late) > 0 {
			ra["late_tasks"] = a.Late
		}
		res = append(res, ra)
	}
	return plainOf(res).([]any), nil
}

func plainOf(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// ClosedAttempt returns the revision number and attempt of the closed successful attempt.
func (u *WorkUnit) ClosedAttempt() (int, WorkAttempt, bool) {
	if u.State != "closed" || len(u.Attempts) == 0 {
		return 0, WorkAttempt{}, false
	}
	a := u.Attempts[len(u.Attempts)-1]
	if a.Outcome != "completed" || a.Closure != "validated" || len(a.Late) > 0 { // late activity: the closure no longer holds
		return 0, WorkAttempt{}, false
	}
	for i, r := range u.Revisions { // the revision that closed it
		if r["state"] == "closed" {
			return i + 1, a, true
		}
	}
	return 0, WorkAttempt{}, false
}

// MarkWorkReported records sent revisions (the next report sends only newer ones).
func MarkWorkReported(s *store.Store, revisions []any) error {
	units, err := WorkUnits(s)
	if err != nil {
		return err
	}
	for _, u := range units {
		changed := false
		for _, r := range revisions {
			m, _ := r.(map[string]any)
			d, _ := m["revision_digest"].(string)
			if m["unit_ref"] == u.UnitRef && !slices.Contains(u.Reported, d) {
				u.Reported = append(u.Reported, d)
				changed = true
			}
		}
		if changed {
			if err := saveWork(s, u); err != nil {
				return err
			}
		}
	}
	return nil
}
