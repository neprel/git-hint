package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// projectLedgerSetup is a store with one open work unit whose open attempt has the task "t1".
func projectLedgerSetup(t *testing.T) (*store.Store, LedgerInput, string) {
	t.Helper()
	s, _ := e2eProject(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	cfg := activity.Configuration{}
	if _, err := project.WorkStart(s, "unit-a", nil, "", "fam", &cfg, now); err != nil {
		t.Fatal(err)
	}
	task := project.TaskID(s.Key(), "t1")
	if err := project.WorkJoin(s, "unit-a", task, now); err != nil {
		t.Fatal(err)
	}
	u, err := project.LoadWork(s, "unit-a")
	if err != nil {
		t.Fatal(err)
	}
	_, c, url := proxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	send(context.Background(), url)
	r := c.Seal(time.Second, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	attempt := u.Attempts[0].Ref
	in := LedgerInput{Task: "t1", Family: "fam", Agent: "opencode", Configuration: cfg, SealedAt: r.SealedAt, Report: *r,
		Project: &ProjectRun{Label: "unit-a", Run: "run-1", AttemptRef: attempt, ExecutionRef: hint.EvalRef(s.Key(), "execution", "unit-a", "run-1")}}
	return s, in, attempt
}

func writtenLedger(t *testing.T, s *store.Store) *activity.ModelLedger {
	t.Helper()
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.ModelLedger != nil {
			return o.ModelLedger
		}
	}
	t.Fatal("no ledger written")
	return nil
}

func TestProjectLedgerHasTheProjectBinding(t *testing.T) {
	s, in, attempt := projectLedgerSetup(t)
	if _, err := WriteLedger(s, in); err != nil {
		t.Fatal(err)
	}
	b := writtenLedger(t, s).Binding
	key := s.Key()
	want := activity.LedgerBinding{RunRef: activity.RunRef(key, "run-1"), TaskID: project.TaskID(key, "t1"), TaskFamilyID: activity.FamilyRef(key, "fam"),
		ConfigurationDigest: in.Configuration.Digest(), UnitRef: hint.EvalRef(key, "work-unit", "unit-a"), AttemptRef: attempt,
		Origin: "project", ExecutionRef: hint.EvalRef(key, "execution", "unit-a", "run-1")}
	if b != want {
		t.Fatalf("binding %+v, want %+v", b, want)
	}
	if st, err := WriteLedger(s, in); err != nil || st != "revision already written" {
		t.Fatalf("replay: %q %v", st, err)
	}
}

func TestProjectLedgerRefusesAMismatchedClaim(t *testing.T) {
	cases := map[string]func(in *LedgerInput){
		"other execution":   func(in *LedgerInput) { in.Project.ExecutionRef = strings.Repeat("0", 32) },
		"other attempt":     func(in *LedgerInput) { in.Project.AttemptRef = strings.Repeat("0", 32) },
		"task not a member": func(in *LedgerInput) { in.Task = "t2" },
		"unknown unit":      func(in *LedgerInput) { in.Project.Label = "unit-b" },
		"also a trial":      func(in *LedgerInput) { in.Trial = "tr-0123456789abcdef" },
		"also an eval":      func(in *LedgerInput) { in.Eval = "ev-0123456789abcdef" },
		"also an attempt":   func(in *LedgerInput) { in.Attempt = 1 },
		"unusable run name": func(in *LedgerInput) { in.Project.Run = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, in, _ := projectLedgerSetup(t)
			p := *in.Project
			in.Project = &p
			change(&in)
			if _, err := WriteLedger(s, in); !errors.Is(err, ErrLedger) {
				t.Fatalf("written: %v", err)
			}
			if obs, _, _ := project.ReadSpool(s); len(obs) != 0 {
				for _, o := range obs {
					if o.ModelLedger != nil {
						t.Fatal("a refused project ledger was written")
					}
				}
			}
		})
	}
}

// The adapter ledger input has no JSON form for a project binding.
func TestLedgerInputJSONCannotClaimAProject(t *testing.T) {
	b, _ := json.Marshal(LedgerInput{Project: &ProjectRun{Label: "u", Run: "r"}})
	if strings.Contains(string(b), "project") || strings.Contains(string(b), `"u"`) {
		t.Fatalf("the input encodes the project claim: %s", b)
	}
	var in LedgerInput
	dec := json.NewDecoder(strings.NewReader(`{"project":{"Label":"u"}}`))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) == nil || in.Project != nil {
		t.Fatal("a JSON project claim was accepted")
	}
}

func TestBindingOriginRules(t *testing.T) {
	_, in, _ := projectLedgerSetup(t)
	m := ledgerRecord(in.Report)
	m.LedgerRef = strings.Repeat("1", 32)
	m.Binding = activity.LedgerBinding{RunRef: strings.Repeat("2", 32), TaskID: strings.Repeat("3", 32), TaskFamilyID: strings.Repeat("4", 32),
		ConfigurationDigest: "sha256:" + strings.Repeat("5", 64), UnitRef: strings.Repeat("6", 32), AttemptRef: strings.Repeat("7", 32)}
	if p := activity.LedgerProblems(m); len(p) != 0 {
		t.Fatalf("controlled binding: %v", p)
	}
	for name, b := range map[string][2]string{"execution without origin": {"", strings.Repeat("8", 32)}, "project without execution": {"project", ""},
		"other origin": {"controlled", strings.Repeat("8", 32)}, "opaque check": {"project", "not-opaque"}} {
		mm := m
		mm.Binding.Origin, mm.Binding.ExecutionRef = b[0], b[1]
		if len(activity.LedgerProblems(mm)) == 0 {
			t.Fatalf("%s accepted", name)
		}
	}
	mm := m
	mm.Binding.Origin, mm.Binding.ExecutionRef = "project", strings.Repeat("8", 32)
	if p := activity.LedgerProblems(mm); len(p) != 0 {
		t.Fatalf("project binding: %v", p)
	}
	raw, _ := json.Marshal(m.Binding)
	if strings.Contains(string(raw), "origin") || strings.Contains(string(raw), "execution_ref") {
		t.Fatalf("a controlled binding changed its bytes: %s", raw)
	}
}
