package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// LedgerInput is one sealed model ledger of an agent run as the runner hands it over: the raw
// local names of the run, its declared task and family, its canonical configuration, the
// revision and seal time, the runner's account, and whether the account is final.
type LedgerInput struct {
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
	// NotFinal marks an account whose attempt was not torn down completely (07B-J §23): the
	// record is incomplete with teardown_incomplete whatever the account says
	NotFinal bool `json:"not_final,omitempty"`
}

// ErrLedger is returned for a ledger that is not written (nothing was written).
var ErrLedger = errors.New("the ledger is not written")

// WriteLedger writes the USAGE record of one ledger revision (draft v0.2) to the activity
// spool. It refuses an inconsistent account, a revision that is not the next one, a second
// body for a written revision, a changed binding or seal time, and a revision that rewrites
// or heals the sealed ledger; a replay of a written revision writes nothing. It returns a
// short status line.
func WriteLedger(s *store.Store, in LedgerInput) (string, error) {
	for _, v := range []string{in.Trial, in.Run, in.Eval, in.Task, in.Family} {
		if !project.ValidTaskID(v) {
			return "", fmt.Errorf("%w: the input names an unusable trial, run, eval, task or family", ErrLedger)
		}
	}
	key := s.Key()
	runRef := activity.RunRef(key, in.Trial)
	m := ledgerRecord(in.Report)
	if in.NotFinal && !slices.Contains(m.Reasons, "teardown_incomplete") {
		m.State, m.Reasons = "incomplete", append(m.Reasons, "teardown_incomplete")
	}
	m.LedgerRef, m.Revision, m.SealedAt = activity.LedgerRef(key, runRef), in.Revision, in.SealedAt
	if in.Report.SealedAt != "" && in.Report.SealedAt != in.SealedAt {
		return "", fmt.Errorf("%w: the account's seal time is not the handed-over seal time", ErrLedger)
	}
	ledgerEntries(&m, key, in.Report)
	m.Binding = activity.LedgerBinding{RunRef: runRef, TaskID: project.TaskID(key, in.Task), TaskFamilyID: activity.FamilyRef(key, in.Family),
		ConfigurationDigest: in.Configuration.Digest(), UnitRef: hint.EvalRef(key, "work-unit", "controlled", in.Run, in.Eval),
		AttemptRef: hint.EvalRef(key, "attempt", "controlled", in.Run, in.Eval, fmt.Sprint(in.Attempt))}
	if p := activity.LedgerProblems(m); len(p) > 0 {
		return "", fmt.Errorf("%w: the ledger is inconsistent (%v)", ErrLedger, p)
	}
	src := activity.LedgerSource(key, in.Agent, in.Configuration)
	o := activity.LedgerObservation(key, src, m)
	lock, err := s.LockState(true)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		return "", err
	}
	next := 0
	var prev *activity.ModelLedger
	for _, x := range obs {
		l := x.ModelLedger
		if l == nil || l.LedgerRef != m.LedgerRef {
			continue
		}
		if l.Binding != m.Binding || l.SealedAt != m.SealedAt {
			return "", fmt.Errorf("%w: the ledger changes its binding or seal time", ErrLedger)
		}
		if l.Revision == m.Revision {
			if !sameJSON(x, o) {
				return "", fmt.Errorf("%w: a different body for a written ledger revision", ErrLedger)
			}
			return "revision already written", nil
		}
		if l.Revision+1 > next {
			next, prev = l.Revision+1, l
		}
	}
	if m.Revision != next {
		return "", fmt.Errorf("%w: the ledger revision must be %d", ErrLedger, next)
	}
	if prev != nil && activity.LedgerLifecycleProblem(*prev, m) {
		return "", fmt.Errorf("%w: the revision rewrites or heals the sealed ledger (only late traffic can be added)", ErrLedger)
	}
	if err := project.RegisterSource(s, src); err != nil {
		return "", err
	}
	if err := project.AppendObservation(s, o); err != nil {
		return "", err
	}
	return fmt.Sprintf("revision %d written (%s)", m.Revision, m.State), nil
}

// ledgerRecord maps the runner's account to the wire form: every count present (the runner's
// account enumerates its entries, so an absent state count is a known zero), the components
// with their coverage, the requested-model label, the agent's launch/exit enclosure, the backend
// captures.
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
	if a := r.AgentRun; a != nil { // the runner-observed enclosure (draft v0.2), copied as recorded
		m.AgentRun = &activity.LedgerAgentRun{Outcome: a.Outcome, ExitCode: a.ExitCode, EnclosureStartMs: a.EnclosureStartMs, EnclosureEndMs: a.EnclosureEndMs,
			ExecutionDigest: a.ExecutionDigest, EngineStartedAt: a.EngineStartedAt, EngineFinishedAt: a.EngineFinishedAt}
	}
	if b := r.BackendCapture; b != nil { // the assembled backend captures (draft v0.2), copied as recorded
		m.BackendCapture = &activity.LedgerBackendCapture{Method: b.Method, Collector: activity.LedgerCollector{Name: b.Collector["name"], Version: b.Collector["version"]},
			Start: ledgerCapture(b.Start), End: ledgerCapture(b.End)}
	}
	return m
}

// ledgerCapture copies one capture into its own values (no memory shared with the account).
func ledgerCapture(c runner.ReportCapture) activity.LedgerCapture {
	x := activity.LedgerCapture{Status: c.Status, SendMs: clonePtr(c.SendMs), RecvMs: clonePtr(c.RecvMs)}
	if s := c.Snapshot; s != nil {
		f := s.Flags
		x.Snapshot = &activity.LedgerCaptureSnapshot{ImageID: s.ImageID, HostRef: s.HostRef, ContainerRef: s.ContainerRef, StartedAt: clonePtr(s.StartedAt),
			RestartCount: clonePtr(s.RestartCount), Flags: activity.LedgerFlags{PromptTokensDetails: clonePtr(f.PromptTokensDetails), PrefixCaching: clonePtr(f.PrefixCaching),
				MaxModelLen: clonePtr(f.MaxModelLen), ReasoningParser: clonePtr(f.ReasoningParser), ToolCallParser: clonePtr(f.ToolCallParser), ServedModelRef: clonePtr(f.ServedModelRef)}}
	}
	return x
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ledgerEntries copies the account's per-entry evidence and timing to the record (draft v0.2),
// with each entry's opaque ID derived from the project key and the ledger reference. An account
// without entries (no request, or over the bound) stays aggregate-only: no entries and no timing.
func ledgerEntries(m *activity.ModelLedger, key []byte, r runner.LedgerReport) {
	if len(r.Entries) == 0 {
		return
	}
	model := map[string]string{"verified": "requested_model_observed", "mismatch": "mismatch"}
	for _, e := range r.Entries {
		mc := model[e.Model]
		if mc == "" {
			mc = "unverified" // a request without a readable model field
		}
		comps := map[string]*int64{}
		for _, k := range activity.LedgerComponents {
			comps[k] = e.Components[k]
		}
		m.Entries = append(m.Entries, activity.LedgerEntry{Ordinal: e.Ordinal, EntryID: activity.LedgerEntryRef(key, m.LedgerRef, e.Ordinal),
			State: e.State, Termination: e.Termination, ResponseStarted: e.ResponseStarted, StreamDone: e.StreamDone, StatusClass: e.StatusClass,
			ModelCheck: mc, AdmittedMs: e.AdmittedMs, FinishedMs: e.FinishedMs, Components: comps})
	}
	m.AnchorUTC, m.SealedMs, m.FinalizedMs, m.ClockStepMs = r.AnchorUTC, r.SealedMs, r.FinalizedMs, r.ClockStepMs
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
