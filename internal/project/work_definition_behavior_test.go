package project_test

// All captures, journals, traffic and launch facts below are SYNTHETIC fixtures.
// Actual local producers run; no engine, model, backend, listener or source capture runs.
// Successful input creation never asserts canonical usage or completion authority.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

func definitionCLI(t *testing.T, s *store.Store, cfg activity.Configuration, task, family, run string, now time.Time) activity.CLIRootImport {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join("..", "activity", "testdata", "cli-root", "p3.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := activity.DecodeCLIRoot(bytes.NewReader(raw), activity.CLIRootOptions{Key: s.Key(), AgentVersion: activity.CLIRootAgentVersion, Configuration: &cfg, TaskID: task, TaskFamilyID: family, RunRef: run, Cwd: "/app", CaptureComplete: true, CapturedAt: now, Resolve: func(string) (string, bool) { return "", false }})
	if e != nil || len(decoded.Observations) == 0 {
		t.Fatal("actual fixture decoder", e)
	}
	return decoded
}

// A legacy-shaped synthetic account through the real writer. No profile, capture
// or physical-enclosure authority is supplied; token quantities remain descriptive.
func definitionTraffic(now time.Time) runner.LedgerReport {
	return runner.LedgerReport{State: "complete", Counts: map[string]int{runner.EntryComplete: 1}, Components: map[string]int64{"prompt": 3, "completion": 2}, ComponentsN: map[string]int{"prompt": 1, "completion": 1}, ModelCheck: "verified", Bypass: runner.BypassEnforced, Scope: runner.ScopeIsolatedContainer, SealedAt: now.Format(time.RFC3339)}
}

func TestWorkDefinitionControlledBehaviorExactReceivedTuple(t *testing.T) {
	cases := []string{"no-ledger", "ledger", "failed-ledger", "wrong-run", "job-run", "wrong-family", "wrong-configuration", "unowned-task", "interrupted-before-attempt", "missing-run-journal", "wrong-ledger-attempt", "wrong-ledger-configuration", "wrong-ledger-family", "original-wrong-run-and-config"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s, l, request, now := project.DefinitionFixtureForExternalTest(t)
			mb := workunits.Member{Eval: request.QualifiedIO.EvalName, EvalDigest: request.QualifiedIO.ManifestDigest, Baseline: request.QualifiedIO.EvalName, BaselineDigest: request.QualifiedIO.ManifestDigest, Objective: strings.Split(request.QualifiedIO.EvalName, "@")[0], TaskFamily: "feature", Eligible: true}
			m := workunits.Manifest{RunID: "jb-0000000000000042", Suite: "su-0000000000000042", Agent: "opencode", AgentVersion: runner.DockerOpenCodeVersion, Arm: "baseline", Attempts: 1, RunnerVersion: runner.DockerRunnerSemver, Harness: "hint-docker", ModelLabel: "synthetic", LaunchDigest: "sha256:" + strings.Repeat("a", 64), MaxRequests: 8, MaxSeconds: 300, CreatedAt: now.Format(time.RFC3339), Members: []workunits.Member{mb}}
			if e := workunits.Start(s, l, m, now); e != nil {
				t.Fatal(e)
			}
			reward := 1.0
			if name == "failed-ledger" {
				reward = 0
			}
			input := suites.TrialInput{RunID: m.RunID, Suite: m.Suite, Eval: mb.Eval, Attempt: 1, Arm: m.Arm, Agent: m.Agent, AgentVersion: m.AgentVersion, Model: m.ModelLabel, RunnerVersion: m.RunnerVersion, RunnerName: "hint-docker", Started: now.Add(time.Second), MaxRequests: m.MaxRequests, MaxSeconds: m.MaxSeconds, ModelCheck: "not_observed", Objective: mb.Objective, Baseline: mb.Baseline, BaselineDigest: mb.BaselineDigest, EvalDigest: mb.EvalDigest, ObservedConfig: []map[string]string{{"kind": "setting_override", "digest": m.LaunchDigest}}}
			trial, e := suites.RecordTrial(l, input, runner.Trial{Name: "synthetic-terminal", Reward: &reward})
			if e != nil {
				t.Fatal(e)
			}
			if name != "interrupted-before-attempt" {
				if e = workunits.Record(s, l, m.RunID, mb.Eval, trial); e != nil {
					t.Fatal(e)
				}
			}
			var cfg activity.Configuration
			raw, _ := json.Marshal(workunits.ScopeConfiguration(s.Key(), m))
			if e = json.Unmarshal(raw, &cfg); e != nil {
				t.Fatal(e)
			}
			task, family, run := project.TaskID(s.Key(), mb.Objective), activity.FamilyRef(s.Key(), mb.TaskFamily), activity.RunRef(s.Key(), trial)
			ledgerCfg := cfg
			switch name {
			case "wrong-run":
				run = activity.RunRef(s.Key(), "different-trial")
			case "job-run":
				run = activity.RunRef(s.Key(), m.RunID)
			case "wrong-family":
				family = activity.FamilyRef(s.Key(), "different-family")
			case "wrong-configuration":
				cfg.Budget.MaxRequests.Value++
			case "unowned-task":
				task = project.TaskID(s.Key(), "unowned")
			case "original-wrong-run-and-config":
				cfg.Model = activity.ModelState{State: "unknown"}
				cfg.ConfigRefs.Refs = []string{}
				run = activity.RunRef(s.Key(), m.RunID+"-trial-1")
			}
			decoded := definitionCLI(t, s, cfg, task, family, run, now)
			if e = project.WriteActivityBatch(s, decoded.Source, decoded.Observations); e != nil {
				t.Fatal(e)
			}
			if name == "ledger" || name == "failed-ledger" || name == "wrong-ledger-attempt" || name == "wrong-ledger-configuration" || name == "wrong-ledger-family" {
				attempt := 1
				if name == "wrong-ledger-attempt" {
					attempt = 2
				}
				if name == "wrong-ledger-configuration" {
					ledgerCfg.Budget.MaxRequests.Value++
				}
				ledgerFamily := mb.TaskFamily
				if name == "wrong-ledger-family" {
					ledgerFamily = "different-family"
				}
				_, receipt, e := pipeline.WriteLedgerWithReceipt(s, pipeline.LedgerInput{Trial: trial, Run: m.RunID, Eval: mb.Eval, Attempt: attempt, Task: mb.Objective, Family: ledgerFamily, Agent: m.Agent, Configuration: ledgerCfg, SealedAt: now.Format(time.RFC3339), Report: definitionTraffic(now)})
				if e != nil || receipt == nil {
					t.Fatal("actual ledger writer", e)
				}
			}
			if name == "missing-run-journal" {
				if e = os.Remove(s.Path("state", "evals", "work", m.RunID+".json")); e != nil {
					t.Fatal(e)
				}
			}
			_, e = project.WriteWorkDefinitionInputs(context.Background(), s, request, now)
			positive := name == "no-ledger" || name == "ledger" || name == "failed-ledger"
			if positive {
				if e != nil {
					t.Fatal("exact received tuple refused", e)
				}
				return
			}
			if !errors.Is(e, project.ErrWorkDefinition) {
				t.Fatal("unowned or wrong received tuple admitted", e)
			}
			if _, e = os.Lstat(s.Path("state", "work-definitions")); !os.IsNotExist(e) {
				t.Fatal("reserved definition before refusal", e)
			}
		})
	}
}

func TestWorkDefinitionProjectMembershipBehaviorAndLedger(t *testing.T) {
	for _, name := range []string{"valid", "failed", "next-epoch", "forked-next-epoch", "reused-execution-next-epoch", "distinct-executions-same-task", "wrong-run", "wrong-family", "wrong-configuration", "missing-enrollment", "wrong-execution"} {
		t.Run(name, func(t *testing.T) {
			s, _, request, now := project.DefinitionFixtureForExternalTest(t)
			launch, execution := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
			cfg := request.DeclaredConfiguration
			cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(s.Key(), "synthetic"), Provenance: "launch_intent"}
			cfg.Harness.Kind, cfg.Harness.Version = "other", activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
			cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(s.Key(), "setting_override", launch)}}
			cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 8}, MaxSeconds: activity.Limit{State: "known", Value: 300}}
			if _, e := project.WorkStart(s, "unrelated-project", nil, "", "feature", &cfg, now); e != nil {
				t.Fatal(e)
			}
			policy := project.MembershipPolicy{Coverage: project.MembershipCoverage, Agent: "opencode", ConfigurationDigests: []string{cfg.Digest()}, LaunchDigests: []string{launch}, StudyLaunchPolicy: string(runner.StudyLaunchSingleRootV1), GatePolicyDigest: runner.GatePolicyDigest(), RunnerVersion: runner.DockerRunnerSemver, SchemaDigest: project.MembershipSchemaDigest()}
			epoch, e := project.EpochOpen(s, "unrelated-project", policy, now)
			if e != nil {
				t.Fatal(e)
			}
			x, e := epoch.Enroll(s, "project-task", "project-run", cfg, now)
			if e != nil {
				t.Fatal(e)
			}
			if e = epoch.LaunchHook(s, x)(runner.LaunchFacts{LaunchDigest: launch, ExecutionDigest: execution, GatePolicyDigest: runner.GatePolicyDigest(), Target: "linux-arm64", Model: "synthetic"}); e != nil {
				t.Fatal(e)
			}
			behaviorCfg := cfg
			family, run := activity.FamilyRef(s.Key(), "feature"), x.RunRef
			switch name {
			case "wrong-run":
				run = activity.RunRef(s.Key(), "different-project-run")
			case "wrong-family":
				family = activity.FamilyRef(s.Key(), "different-family")
			case "wrong-configuration":
				behaviorCfg.Budget.MaxRequests.Value++
			}
			decoded := definitionCLI(t, s, behaviorCfg, x.TaskID, family, run, now)
			if e = project.WriteActivityBatch(s, decoded.Source, decoded.Observations); e != nil {
				t.Fatal(e)
			}
			projectRun := &pipeline.ProjectRun{Label: "unrelated-project", Run: "project-run", AttemptRef: x.AttemptRef, ExecutionRef: x.ExecutionRef}
			if name == "wrong-execution" {
				projectRun.Run = "different-project-run"
				projectRun.ExecutionRef = hint.EvalRef(s.Key(), "execution", projectRun.Label, projectRun.Run)
			}
			report := definitionTraffic(now)
			zero, end, code := int64(0), int64(1000), 0
			outcome := runner.AgentExited
			if name == "failed" {
				code = 1
				outcome = runner.AgentExitedNonzero
			}
			prompt, completion, done := int64(3), int64(2), true
			report.Entries = []runner.ReportEntry{{Ordinal: 1, State: runner.EntryComplete, Termination: "response_ended", ResponseStarted: true, StreamDone: &done, StatusClass: "2xx", Model: "verified", AdmittedMs: 1, FinishedMs: 2, Components: map[string]*int64{"prompt": &prompt, "completion": &completion}}}
			report.AgentRun = &runner.ReportAgentRun{Outcome: outcome, ExitCode: &code, EnclosureStartMs: &zero, EnclosureEndMs: &end, ExecutionDigest: execution}
			report.AnchorUTC = now.Add(-time.Second).Format(time.RFC3339)
			report.SealedMs = &end
			report.FinalizedMs = &end
			report.ClockStepMs = &zero
			_, receipt, e := pipeline.WriteLedgerWithReceipt(s, pipeline.LedgerInput{Task: "project-task", Family: "feature", Agent: "opencode", Configuration: cfg, SealedAt: report.SealedAt, Report: report, Project: projectRun})
			if e != nil || receipt == nil {
				t.Fatal("actual project ledger writer", e)
			}
			var ledger *activity.ModelLedger
			obs, damage, e := project.ReadSpoolChecked(s)
			if e != nil || damage.Any() {
				t.Fatal(e, damage)
			}
			for _, o := range obs {
				if o.ModelLedger != nil && o.ModelLedger.LedgerRef == receipt.LedgerRef {
					ledger = o.ModelLedger
				}
			}
			if ledger == nil {
				t.Fatal("missing actual written ledger")
			}
			if name == "valid" || name == "failed" || name == "next-epoch" || name == "forked-next-epoch" || name == "reused-execution-next-epoch" || name == "distinct-executions-same-task" {
				if e = epoch.Finish(s, x, ledger); e != nil {
					t.Fatal(e)
				}
				if _, e = epoch.Seal(s, now); e != nil {
					t.Fatal(e)
				}
			}
			if name == "next-epoch" || name == "forked-next-epoch" || name == "reused-execution-next-epoch" || name == "distinct-executions-same-task" {
				next, e := project.EpochOpen(s, "unrelated-project", policy, now)
				if e != nil {
					t.Fatal(e)
				}
				task := "project-task-2"
				if name == "distinct-executions-same-task" {
					task = "project-task"
				}
				if _, e = next.Enroll(s, task, "project-run-2", cfg, now); e != nil {
					t.Fatal(e)
				}
				if name == "forked-next-epoch" || name == "reused-execution-next-epoch" {
					path := s.Path("state", "membership", hint.DeriveID(s.Key(), "membership-file", "unrelated-project"), "epoch-2.jsonl")
					raw, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					var rebuilt bytes.Buffer
					var previous any
					for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
						var event map[string]any
						if e = json.Unmarshal(line, &event); e != nil {
							t.Fatal(e)
						}
						if i == 0 && name == "forked-next-epoch" {
							event["predecessor_epoch_digest"] = "sha256:" + strings.Repeat("0", 64)
						}
						if name == "reused-execution-next-epoch" && event["kind"] == "enroll" {
							event["execution_ref"] = x.ExecutionRef
						}
						event["prev_digest"] = previous
						previous, e = hint.Digest(event)
						if e != nil {
							t.Fatal(e)
						}
						encoded, _ := json.Marshal(event)
						rebuilt.Write(encoded)
						rebuilt.WriteByte('\n')
					}
					if e = os.WriteFile(path, rebuilt.Bytes(), 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
			if name == "missing-enrollment" {
				if e = os.RemoveAll(s.Path("state", "membership")); e != nil {
					t.Fatal(e)
				}
			}
			_, e = project.WriteWorkDefinitionInputs(context.Background(), s, request, now)
			if name == "valid" || name == "failed" || name == "next-epoch" || name == "distinct-executions-same-task" {
				if e != nil {
					t.Fatal("genuine SDK project tuple refused", e)
				}
				return
			}
			if !errors.Is(e, project.ErrWorkDefinition) {
				t.Fatal("wrong project tuple admitted", e)
			}
		})
	}
}
