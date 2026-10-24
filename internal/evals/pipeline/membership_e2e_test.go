package pipeline

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/project"
)

// One project execution end to end, offline: enrollment, the launch hook, the project ledger
// through the shared writer, the finish from that ledger and the published seal. The execution's
// own ledger and its own published record are never foreign owners.
func TestMembershipProjectExecutionEndToEnd(t *testing.T) {
	s, _ := e2eProject(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	key := s.Key()
	launch, execDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	var cfg activity.Configuration
	cfg.Agent.Name, cfg.Agent.Version = "opencode", activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(key, "m"), Provenance: "launch_intent"}
	cfg.Harness.Kind, cfg.Harness.Version = "other", activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
	cfg.ContextPolicy = activity.ConfigPolicy{State: "disabled"}
	cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(key, "setting_override", launch)}}
	cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 5}, MaxSeconds: activity.Limit{State: "known", Value: 60}}
	if _, err := project.WorkStart(s, "unit-a", nil, "", "fam", &cfg, now); err != nil {
		t.Fatal(err)
	}
	policy := project.MembershipPolicy{Coverage: project.MembershipCoverage, Agent: "opencode", ConfigurationDigests: []string{cfg.Digest()},
		LaunchDigests: []string{launch}, StudyLaunchPolicy: string(runner.StudyLaunchSingleRootV1), GatePolicyDigest: runner.GatePolicyDigest(),
		RunnerVersion: runner.DockerRunnerSemver, SchemaDigest: project.MembershipSchemaDigest()}
	e, err := project.EpochOpen(s, "unit-a", policy, now)
	if err != nil {
		t.Fatal(err)
	}
	x, err := e.Enroll(s, "t1", "run-1", cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchHook(s, x)(runner.LaunchFacts{LaunchDigest: launch, ExecutionDigest: execDigest, GatePolicyDigest: runner.GatePolicyDigest(), Target: "linux-arm64", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	_, c, url := proxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	send(context.Background(), url)
	r := c.Seal(time.Second, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	zero, code := int64(0), 0
	r.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentExited, ExitCode: &code, EnclosureStartMs: &zero, EnclosureEndMs: r.SealedMs, ExecutionDigest: execDigest}
	in := LedgerInput{Task: "t1", Family: "fam", Agent: "opencode", Configuration: cfg, SealedAt: r.SealedAt, Report: *r,
		Project: &ProjectRun{Label: "unit-a", Run: "run-1", AttemptRef: x.AttemptRef, ExecutionRef: x.ExecutionRef}}
	if _, err := WriteLedger(s, in); err != nil {
		t.Fatal(err)
	}
	ledger := writtenLedger(t, s)
	if err := e.Finish(s, x, ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(s, now); err != nil {
		t.Fatal(err)
	}
	var finish map[string]any
	obs, _, _ := project.ReadSpool(s)
	for _, o := range obs {
		if o.Membership != nil {
			for _, ev := range o.Membership.Events {
				if ev["kind"] == "finish" {
					finish = ev
				}
			}
		}
	}
	if finish["finish_state"] != runner.AgentExited || finish["ledger_ref"] != ledger.LedgerRef {
		t.Fatalf("finish %v", finish)
	}
	// a replay of the seal and a next epoch with another run both work beside the own records
	if _, err := e.Seal(s, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	e2, err := project.EpochOpen(s, "unit-a", policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Enroll(s, "t2", "run-2", cfg, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Enroll(s, "t3", "run-1", cfg, now); err == nil {
		t.Fatal("a run of a published epoch was enrolled again")
	}
}
