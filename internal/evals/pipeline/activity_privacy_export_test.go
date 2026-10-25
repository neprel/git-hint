package pipeline

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/version"
)

// Synthetic runner account only: actual normal persistence+package paths, zero processes,
// proxy/backend, engine, source capture, or model. No project membership/journal is required.
func TestActivityPrivacyControlledWriteLedgerExport(t *testing.T) {
	s, in := receiptInput(t, false)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, err := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	rawA := "sha256:" + strings.Repeat("a", 64)
	rawB := "sha256:" + strings.Repeat("b", 64)
	var cfg activity.Configuration
	cfg.Agent.Name, cfg.Agent.Version = "opencode", activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(s.Key(), "synthetic-private-model"), Provenance: "launch_intent"}
	cfg.Harness.Kind, cfg.Harness.Version = "other", activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
	cfg.ContextPolicy = activity.ConfigPolicy{State: "disabled"}
	cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(s.Key(), "setting_override", rawA)}}
	cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 8}, MaxSeconds: activity.Limit{State: "known", Value: 300}}
	in.Configuration = cfg
	zero := 0
	in.Report.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentExited, ExitCode: &zero, ExecutionDigest: rawB}
	if _, err := WriteLedger(s, in); err != nil {
		t.Fatal(err)
	}
	// An unrelated malformed project journal proves controlled projection never opens it.
	if err := os.MkdirAll(s.Path("state", "work"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path("state", "work", "synthetic-unread-journal.json"), []byte("not a journal"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := project.BuildPackage(s, project.ExportOptions{All: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Version != version.ActivityPrivacyVersion {
		t.Fatal("wrong normal private wire version")
	}
	after, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if !bytes.Equal(b1, b2) || before[0].ModelLedger.AgentRun.ExecutionDigest != rawB {
		t.Fatal("local B/spool changed")
	}
	found := false
	for _, d := range pkg.Docs {
		if bytes.Contains(d.Raw, []byte(rawB)) || bytes.Contains(d.Raw, []byte(rawA)) || bytes.Contains(d.Raw, []byte("synthetic-private-model")) {
			t.Fatal("private identity escaped package")
		}
		if d.Type == "activity_stream" {
			var m map[string]any
			json.Unmarshal(d.Raw, &m)
			obs := m["payload"].(map[string]any)["observations"].([]any)
			for _, x := range obs {
				if l, ok := x.(map[string]any)["model_ledger"].(map[string]any); ok {
					found = true
					if l["agent_run"].(map[string]any)["execution_provenance_ref"] != hint.DeriveID(s.Key(), "execution-provenance", rawB) {
						t.Fatal("normal B projection")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("no actual written ledger exported")
	}
}
