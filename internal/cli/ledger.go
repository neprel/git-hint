package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
)

// ledgerInput is one sealed model ledger of an agent run, as the runner hands it over: the raw
// local names of the run, its declared task and family, its canonical configuration, the
// revision and seal time, and the runner's account.
type ledgerInput struct {
	Trial         string                 `json:"trial"`
	Run           string                 `json:"run"`
	Eval          string                 `json:"eval"`
	Attempt       int                    `json:"attempt"`
	Task          string                 `json:"task"`
	Family        string                 `json:"family"`
	Agent         string                 `json:"agent"`
	Configuration activity.Configuration `json:"configuration"`
	Revision      int                    `json:"revision"`
	SealedAt      string                 `json:"sealed_at"`
	Report        runner.LedgerReport    `json:"report"`
}

// adapterLedger writes the USAGE record of one ledger revision (draft v0.2) to the activity
// spool. It refuses an inconsistent account, a revision that is not the next one, a second body
// for a written revision, and a changed binding or seal time; a replay of a written revision
// writes nothing.
func adapterLedger(e *Env, args []string) int {
	fs := flags(e, "adapter ledger", "reads one sealed model ledger on stdin; writes local state; no network")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	b, err := io.ReadAll(io.LimitReader(e.Stdin, 1<<20))
	if err != nil {
		return fail(e, err)
	}
	var in ledgerInput
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fail(e, fmt.Errorf("the ledger input is not readable: %w; nothing was written", err))
	}
	for _, v := range []string{in.Trial, in.Run, in.Eval, in.Task, in.Family} {
		if !project.ValidTaskID(v) {
			return fail(e, errors.New("the ledger input names an unusable trial, run, eval, task or family; nothing was written"))
		}
	}
	key := s.Key()
	runRef := activity.RunRef(key, in.Trial)
	m := ledgerRecord(in.Report)
	m.LedgerRef, m.Revision, m.SealedAt = activity.LedgerRef(key, runRef), in.Revision, in.SealedAt
	m.Binding = activity.LedgerBinding{RunRef: runRef, TaskID: project.TaskID(key, in.Task), TaskFamilyID: activity.FamilyRef(key, in.Family),
		ConfigurationDigest: in.Configuration.Digest(), UnitRef: hint.EvalRef(key, "work-unit", "controlled", in.Run, in.Eval),
		AttemptRef: hint.EvalRef(key, "attempt", "controlled", in.Run, in.Eval, fmt.Sprint(in.Attempt))}
	if p := activity.LedgerProblems(m); len(p) > 0 {
		return fail(e, fmt.Errorf("the ledger is inconsistent (%s); nothing was written", strings.Join(p, "; ")))
	}
	src := activity.LedgerSource(key, in.Agent, in.Configuration)
	o := activity.LedgerObservation(key, src, m)
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		return fail(e, err)
	}
	next := 0
	var prev *activity.ModelLedger
	for _, x := range obs {
		l := x.ModelLedger
		if l == nil || l.LedgerRef != m.LedgerRef {
			continue
		}
		if l.Binding != m.Binding || l.SealedAt != m.SealedAt {
			return fail(e, errors.New("the ledger changes its binding or seal time; nothing was written"))
		}
		if l.Revision == m.Revision {
			if !sameJSON(x, o) {
				return fail(e, errors.New("a different body for a written ledger revision; nothing was written"))
			}
			fmt.Fprintln(e.Stdout, "adapter ledger: revision already written")
			return 0
		}
		if l.Revision+1 > next {
			next, prev = l.Revision+1, l
		}
	}
	if m.Revision != next {
		return fail(e, fmt.Errorf("the ledger revision must be %d; nothing was written", next))
	}
	if prev != nil && activity.LedgerLifecycleProblem(*prev, m) {
		return fail(e, errors.New("the revision rewrites or heals the sealed ledger (only late traffic can be added); nothing was written"))
	}
	if err := project.RegisterSource(s, src); err != nil {
		return fail(e, err)
	}
	if err := project.AppendObservation(s, o); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "adapter ledger: revision %d written (%s)\n", m.Revision, m.State)
	return 0
}

// ledgerRecord maps the runner's account to the wire form: every count present (the runner's
// account enumerates its entries, so an absent state count is a known zero), the components
// with their coverage, the requested-model label.
func ledgerRecord(r runner.LedgerReport) activity.ModelLedger {
	m := activity.ModelLedger{State: r.State, Reasons: slices.Clone(r.Reasons), Counts: map[string]int{}, Components: map[string]*int64{},
		ComponentCoverage: map[string]int{}, Bypass: r.Bypass, Scope: r.Scope}
	if m.Reasons == nil {
		m.Reasons = []string{}
	}
	admitted := 0
	for _, k := range activity.LedgerEntryStates {
		m.Counts[k] = r.Counts[k]
		admitted += r.Counts[k]
	}
	m.Counts["admitted"], m.Counts["late"], m.Counts["rejected_limit"] = admitted, r.Counts["late"], r.Counts["rejected_limit"]
	for _, k := range activity.LedgerComponents {
		m.ComponentCoverage[k] = r.ComponentsN[k]
		if r.ComponentsN[k] > 0 {
			v := r.Components[k]
			m.Components[k] = &v
		} else {
			m.Components[k] = nil
		}
	}
	m.ModelCheck = map[string]string{"verified": "requested_model_observed"}[r.ModelCheck]
	if m.ModelCheck == "" {
		m.ModelCheck = r.ModelCheck
	}
	return m
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
