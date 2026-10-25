package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
)

func TestObservedStartOwnedGraphAndResolverShareOneResult(t *testing.T) {
	s, l := behaviorProject(t)
	names, _ := l.Manifests("eval")
	var rec map[string]any
	if e := l.Manifest("eval", names[len(names)-1], &rec); e != nil {
		t.Fatal(e)
	}
	work := t.TempDir()
	dir := filepath.Join(work, "jobs", "trial", "trial__1", "agent")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	digest, e := writeObservedArchive(filepath.Join(dir, "prelaunch.tar"))
	if e != nil {
		t.Fatal(e)
	}
	execution := "sha256:" + strings.Repeat("c", 64)
	start := int64(4)
	var job jobResult
	job.Container.PrelaunchTree = &runner.PrelaunchTreeCapture{State: "complete", Digest: digest, Clock: "ledger_anchor_monotonic_ms", StartedMs: 1, FinishedMs: 3, ExecutionDigest: execution}
	job.Container.AgentBinary.ExecutionDigest = execution
	job.Container.Ledger = &runner.LedgerReport{AgentRun: &runner.ReportAgentRun{ExecutionDigest: execution, EnclosureStartMs: &start}}
	r, e := resolveObservedStart(context.Background(), s, rec, work, job)
	if e != nil {
		t.Fatal(e)
	}
	defer r.close()
	if r.repoID != s.RepoID(s.Cfg.Repos[0]) || r.graph == nil || r.capture.Digest != digest || r.capture.ExecutionDigest != execution {
		t.Fatal("owned scan bindings")
	}
	files := 0
	for _, entity := range r.graph.Entities {
		if entity.Kind == "file" {
			files++
			id, ok := r.resolve("/app/" + entity.Path)
			if !ok || id != entity.ID {
				t.Fatal("resolver did not use retained graph identity")
			}
		}
	}
	if files != 1 {
		t.Fatal("unexpected capture scope", files)
	}
	job.Container.PrelaunchTree.ExecutionDigest = "changed"
	if r.capture.ExecutionDigest != execution {
		t.Fatal("capture copy aliases job")
	}
	entries, e := os.ReadDir(s.Path("cache"))
	if e != nil {
		t.Fatal(e)
	}
	owned := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "behavior-observed-") {
			if owned != "" {
				t.Fatal("more than one materialization")
			}
			owned = s.Path("cache", entry.Name())
		}
	}
	if owned == "" {
		t.Fatal("owned materialization absent")
	}
	r.close()
	if _, e = os.Stat(owned); !os.IsNotExist(e) {
		t.Fatal("owned materialization retained")
	}
	if e = os.Mkdir(owned, 0700); e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(owned)
	if e = os.WriteFile(filepath.Join(owned, "foreign-after-close"), []byte("synthetic"), 0600); e != nil {
		t.Fatal(e)
	}
	r.close()
	if _, e = os.Stat(filepath.Join(owned, "foreign-after-close")); e != nil {
		t.Fatal("second cleanup removed replacement directory")
	}
}

// The genuine sole ledger writer receipt must bind to the native side of normal
// export. No spool search or caller mapping repairs a mismatched emission.
func TestBehaviorProjectionMatchesActualLedgerEmissionReceipt(t *testing.T) {
	s, in := receiptInput(t, false)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if _, e := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); e != nil {
		t.Fatal(e)
	}
	rawB := "sha256:" + strings.Repeat("b", 64)
	var cfg activity.Configuration
	cfg.Agent.Name, cfg.Agent.Version = "opencode", activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model = activity.ModelState{State: "known", Ref: activity.ModelRef(s.Key(), "synthetic-private-model"), Provenance: "launch_intent"}
	cfg.Harness.Kind, cfg.Harness.Version = "other", activity.VersionState{State: "known", Value: runner.DockerRunnerSemver}
	cfg.ContextPolicy = activity.ConfigPolicy{State: "disabled"}
	cfg.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{activity.ConfigRef(s.Key(), "setting_override", "synthetic-launch")}}
	cfg.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 8}, MaxSeconds: activity.Limit{State: "known", Value: 300}}
	in.Configuration = cfg
	zero := 0
	in.Report.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentExited, ExitCode: &zero, ExecutionDigest: rawB}
	_, native, e := WriteLedgerWithReceipt(s, in)
	if e != nil {
		t.Fatal(e)
	}
	_, r, e := project.BuildPackageWithReceipt(s, project.ExportOptions{All: true, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	rows := r.Ledgers()
	if len(rows) != 1 {
		t.Fatal("native occurrence missing")
	}
	x := rows[0]
	if x.NativeObservationID != native.ObservationID || x.NativeObservationDigest != native.ObservationDigest || x.NativeSourceID != native.SourceID || x.NativeSourceDigest != native.SourceDeclarationDigest || x.NativeLedgerDigest != native.LedgerDigest || x.Binding != native.Binding || x.LedgerRef != native.LedgerRef || x.Revision != native.Revision {
		t.Fatal("same-emission receipt mismatch")
	}
	if x.ExecutionProvenanceRef != hint.DeriveID(s.Key(), "execution-provenance", rawB) {
		t.Fatal("wrong keyed projection")
	}
	if x.State != "incomplete" || len(x.BackendImages) != 0 {
		t.Fatal("missing backend capture became complete")
	}
}
