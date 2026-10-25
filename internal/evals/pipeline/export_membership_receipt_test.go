package pipeline

// Actual normal writers over synthetic local input only. No proxy, agent, engine,
// network, imported admission metadata or physical model traffic exists here.
import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// This existing-writer composition can also be called by a private test overlay
// to retain fixture bytes. It supplies no production authority or export helper.
func membershipProjectionWriterFixture(t *testing.T) (*store.Store, *project.Package, *project.LocalProjectionReceipt, []byte) {
	t.Helper()
	s, base := receiptInput(t, false)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, e := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); e != nil {
		t.Fatal(e)
	}
	key := s.Key()
	launch := "sha256:" + strings.Repeat("a", 64)
	execution := "sha256:" + strings.Repeat("b", 64)
	var cfg activity.Configuration
	cfg.Agent.Name = "opencode"
	cfg.Agent.Version = activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(key, "synthetic-membership-model"), Provenance: "launch_intent"}
	cfg.Harness.Kind = "other"
	cfg.Harness.Version = activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
	cfg.ContextPolicy = activity.ConfigPolicy{State: "disabled"}
	cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(key, "setting_override", launch)}}
	cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 5}, MaxSeconds: activity.Limit{State: "known", Value: 60}}
	if _, e := project.WorkStart(s, "unit", nil, "", "family", &cfg, now); e != nil {
		t.Fatal(e)
	}
	policy := project.MembershipPolicy{Coverage: project.MembershipCoverage, Agent: "opencode", ConfigurationDigests: []string{cfg.Digest()}, LaunchDigests: []string{launch}, StudyLaunchPolicy: string(runner.StudyLaunchSingleRootV1), GatePolicyDigest: runner.GatePolicyDigest(), RunnerVersion: runner.DockerRunnerSemver, SchemaDigest: project.MembershipSchemaDigest()}
	raw, e := os.ReadFile(filepath.Join("..", "..", "activity", "testdata", "cli-root", "p3.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 2; i++ {
		epoch, e := project.EpochOpen(s, "unit", policy, now)
		if e != nil {
			t.Fatal(e)
		}
		suffix := []string{"one", "two"}[i-1]
		task, run := "task-"+suffix, "run-"+suffix
		x, e := epoch.Enroll(s, task, run, cfg, now)
		if e != nil {
			t.Fatal(e)
		}
		if e = epoch.LaunchHook(s, x)(runner.LaunchFacts{LaunchDigest: launch, ExecutionDigest: execution, GatePolicyDigest: runner.GatePolicyDigest(), Target: "linux-arm64", Model: "synthetic-membership-model"}); e != nil {
			t.Fatal(e)
		}
		captured := bytes.ReplaceAll(raw, []byte("synthetic-session-1"), []byte("synthetic-session-"+suffix))
		decoded, e := activity.DecodeCLIRoot(bytes.NewReader(captured), activity.CLIRootOptions{Key: key, AgentVersion: activity.CLIRootAgentVersion, Configuration: &cfg, TaskID: x.TaskID, TaskFamilyID: activity.FamilyRef(key, "family"), RunRef: x.RunRef, Cwd: "/app", Resolve: func(string) (string, bool) { return "", false }, CaptureComplete: true, CapturedAt: now})
		if e != nil {
			t.Fatal(e)
		}
		if e = project.WriteActivityBatch(s, decoded.Source, decoded.Observations); e != nil {
			t.Fatal(e)
		}
		in := base
		in.Trial, in.Run, in.Eval, in.Attempt = "", "", "", 0
		in.Task, in.Family, in.Agent = task, "family", "opencode"
		in.Configuration = cfg
		in.Project = &ProjectRun{Label: "unit", Run: run, AttemptRef: x.AttemptRef, ExecutionRef: x.ExecutionRef}
		code := 0
		outcome := runner.AgentExited
		if i == 1 {
			code = 1
			outcome = runner.AgentExitedNonzero
		}
		in.Report.AgentRun = &runner.ReportAgentRun{Outcome: outcome, ExitCode: &code, ExecutionDigest: execution}
		_, written, e := WriteLedgerWithReceipt(s, in)
		if e != nil {
			t.Fatal(e)
		}
		observations, _, e := project.ReadSpoolChecked(s)
		if e != nil {
			t.Fatal(e)
		}
		var ledger *activity.ModelLedger
		for _, o := range observations {
			if o.ObservationID == written.ObservationID {
				ledger = o.ModelLedger
			}
		}
		if ledger == nil {
			t.Fatal("actual written ledger unavailable")
		}
		if e = epoch.Finish(s, x, ledger); e != nil {
			t.Fatal(e)
		}
		if _, e = epoch.Seal(s, now); e != nil {
			t.Fatal(e)
		}
		// Replay the same sealed epoch through its real API; no second occurrence appears.
		if _, e = epoch.Seal(s, now); e != nil {
			t.Fatal(e)
		}
	}
	ordinary, e := project.BuildPackage(s, project.ExportOptions{All: true, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	p, r, e := project.BuildPackageWithReceipt(s, project.ExportOptions{All: true, Now: now})
	if e != nil || !reflect.DeepEqual(ordinary, p) {
		t.Fatal("normal package parity", e)
	}
	units, e := project.WorkUnits(s)
	if e != nil {
		t.Fatal(e)
	}
	revisions := []any{}
	for _, unit := range units {
		for _, revision := range unit.Revisions {
			revisions = append(revisions, revision)
		}
	}
	reportBytes, _, e := (report.Builder{Key: key, Namespace: s.Cfg.ProjectNamespace, WorkUnits: revisions}).Build(library.Open(s, 0), nil, now)
	if e != nil {
		t.Fatal(e)
	}
	return s, p, r, reportBytes
}
func TestMembershipProjectionActualWriterAndCLIRoot(t *testing.T) {
	_, p, r, reportBytes := membershipProjectionWriterFixture(t)
	state, reason := r.MembershipState()
	if state != "complete" || reason != "" || len(r.Memberships()) != 2 {
		t.Fatal("membership projection", state, reason, len(r.Memberships()))
	}
	if len(r.Ledgers()) != 2 {
		t.Fatal("actual ledger receipts missing")
	}
	launches := 0
	for _, m := range r.Memberships() {
		for _, event := range m.Events {
			if event.Kind == "launch" {
				launches++
				found := false
				for _, ledger := range r.Ledgers() {
					if ledger.Binding.ExecutionRef == event.Binding.ExecutionRef && ledger.Binding.RunRef == event.Binding.RunRef && ledger.Binding.AttemptRef == event.Binding.AttemptRef && ledger.Binding.TaskID == event.Binding.TaskID && ledger.Binding.ConfigurationDigest == event.Binding.PlannedConfigurationDigest {
						found = true
					}
				}
				if !found {
					t.Fatal("actual producer binding lost")
				}
			}
		}
	}
	if launches != 2 {
		t.Fatal("same-epoch replay duplicated launch", launches)
	}
	var doc map[string]any
	if json.Unmarshal(reportBytes, &doc) != nil || doc["report_version"] != "0.2.1" {
		t.Fatal("ordinary report version changed")
	}
	if len(doc["work_units"].([]any)) == 0 {
		t.Fatal("normal project revisions missing")
	}
	if p.Version != "0.3.0" {
		t.Fatal("privacy export version")
	}
}
