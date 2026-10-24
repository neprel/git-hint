package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

func terminalCapture() string {
	var lines []string
	for i, file := range []string{"/app/cart.go", "/app/discount_test.go"} {
		b, _ := json.Marshal(map[string]any{"type": "tool_use", "timestamp": 1760000010000 + i*10, "sessionID": "synthetic-root",
			"part": map[string]any{"type": "tool", "id": fmt.Sprint(i), "partID": fmt.Sprint("part-", i), "messageID": "synthetic-message", "sessionID": "synthetic-root", "tool": "read",
				"state": map[string]any{"status": "completed", "input": map[string]any{"filePath": file}, "metadata": map[string]any{},
					"time": map[string]any{"start": 1760000009000 + i*10, "end": 1760000010000 + i*10}, "output": "HINTCANARY RAW OUTPUT"}}})
		lines = append(lines, string(b))
	}
	return strings.Join(lines, "\n") + "\n"
}

type behaviorFakeRunner struct {
	liveRunner
	status, version, raw string
	captureRequested     bool
}

func (r *behaviorFakeRunner) Run(ctx context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	trials, err := r.liveRunner.Run(ctx, task, o)
	if err != nil {
		return trials, err
	}
	dir := filepath.Join(o.JobsDir, "trial", "trial__1")
	os.MkdirAll(filepath.Join(dir, "agent"), 0o700)
	os.WriteFile(filepath.Join(dir, "agent", "opencode.txt"), []byte(strings.ReplaceAll(r.raw, "synthetic-root", fmt.Sprint("synthetic-root-", r.n))), 0o600)
	r.captureRequested = o.CapturePrelaunchTree
	var prelaunch *runner.PrelaunchTreeCapture
	executionDigest := "sha256:" + strings.Repeat("d", 64)
	ledger := o.Counter.Seal(0, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	if o.CapturePrelaunchTree {
		digest, writeErr := writeObservedArchive(filepath.Join(dir, "agent", "prelaunch.tar"))
		if writeErr != nil {
			return nil, writeErr
		}
		zero := int64(0)
		end := *ledger.SealedMs
		code := 0
		ledger.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentExited, ExecutionDigest: executionDigest, EnclosureStartMs: &zero, EnclosureEndMs: &end, ExitCode: &code}
		prelaunch = &runner.PrelaunchTreeCapture{State: "complete", Digest: digest, Clock: "ledger_anchor_monotonic_ms", ExecutionDigest: executionDigest}
	}
	raw, _ := json.Marshal(map[string]any{"hint_container": map[string]any{"agent_binary": map[string]any{"version": r.version, "target": "linux-arm64", "execution_digest": executionDigest},
		"prelaunch_tree": prelaunch,
		"agent_output":   map[string]string{"stdout": r.status}, "ledger": ledger, "ledger_final": true, "ledger_sealed_at": o.Counter.SealedAt()}})
	os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o600)
	return trials, nil
}
func behaviorProject(t *testing.T) (*store.Store, *library.Library) {
	t.Helper()
	s, l := validatedProject(t)
	s.Cfg.Repos[0].Name = "shop"
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	names, _ := l.Manifests("eval")
	for _, name := range names {
		if strings.HasSuffix(name, "@2") {
			var rec map[string]any
			l.Manifest("eval", name, &rec)
			rec["family"], rec["version"], rec["previous"] = "refactor", 3, name
			if _, err := l.PutManifest("eval", rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The current checkout contains a file absent from the real frozen BASE.
	os.WriteFile(filepath.Join(s.Root, "cart.go"), []byte(evaltest.GoFix), 0o600)
	os.WriteFile(filepath.Join(s.Root, "discount_test.go"), []byte(evaltest.GoAdded), 0o600)
	if _, err := project.Collect(s, project.CollectOptions{Full: true, Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	return s, l
}
func TestRunSuiteImportsAtomicCLIRootWithExactLedgerAndJournalBinding(t *testing.T) {
	s, l := behaviorProject(t)
	live, stop := liveProject(t)
	defer stop()
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	p.Model = &evals.Model{Use: "lan", BaseURL: "http://192.168.1.2/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY"}
	s.Cfg.Evals = &p
	one := 1.0
	r := &behaviorFakeRunner{liveRunner: liveRunner{model: "local-coder", results: []runner.Trial{{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}}}}, status: "complete", version: activity.CLIRootAgentVersion, raw: terminalCapture()}
	rep, err := RunSuite(context.Background(), s, r, runner.DockerRunnerSemver, SuiteOptions{Agent: "opencode", Attempts: 1, Live: live}, time.Now())
	if err != nil || rep.BehaviorImportProblems != 0 || rep.LedgersNotWritten != 0 || r.captureRequested {
		names, _ := l.Manifests("trial")
		for _, name := range names {
			var diagnostic BehaviorImport
			l.Index("behavior-import/"+name, &diagnostic)
			t.Logf("diagnostic %+v", diagnostic)
		}
		t.Fatalf("run %+v %v", rep, err)
	}
	if len(rep.LedgerReceipts) != rep.Trials || rep.Trials == 0 {
		t.Fatal("suite did not retain each original ledger emission")
	}
	for i := range rep.LedgerReceipts {
		checkPersistedReceipt(t, s, &rep.LedgerReceipts[i])
	}
	withReceipts, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	withoutReceipts := rep
	withoutReceipts.LedgerReceipts = nil
	withoutJSON, err := json.Marshal(withoutReceipts)
	if err != nil || !bytes.Equal(withReceipts, withoutJSON) {
		t.Fatal("internal receipts changed public suite report JSON", err)
	}
	obs, damage, err := project.ReadSpoolChecked(s)
	if err != nil || damage.Any() {
		t.Fatal(damage, err)
	}
	var ledger *activity.ModelLedger
	var behavior []activity.Observation
	for _, o := range obs {
		if o.ModelLedger != nil {
			ledger = o.ModelLedger
		}
		if o.Source == "log" {
			behavior = append(behavior, o)
		}
	}
	if ledger == nil || len(behavior) != 2 {
		t.Fatalf("ledger %v behavior %d", ledger, len(behavior))
	}
	sources, err := project.Sources(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range behavior {
		if o.Correlation["task_id"] != ledger.Binding.TaskID || o.Correlation["task_family_id"] != ledger.Binding.TaskFamilyID || o.Correlation["run_ref"] != ledger.Binding.RunRef || sources[o.SourceID].Configuration.Digest() != ledger.Binding.ConfigurationDigest {
			t.Fatal("behavior/ledger binding mismatch")
		}
		if o.Op == "SESSION" || o.Collection != nil || o.Usage != nil {
			t.Fatal("invented closure/usage")
		}
	}
	if len(behavior[0].Targets) != 1 || behavior[0].Targets[0].EntityID != hint.FileID(s.Key(), s.RepoID(s.Cfg.Repos[0]), "cart.go") {
		t.Fatal("BASE file not resolved")
	}
	if len(behavior[1].Targets) != 0 {
		t.Fatal("resolver used current checkout's added file")
	}
	revisions, err := workunits.Unreported(s)
	if err != nil {
		t.Fatal(err)
	}
	last := revisions[len(revisions)-1].(map[string]any)
	attempt := last["attempts"].([]any)[0].(map[string]any)
	tasks := attempt["task_ids"].([]any)
	if len(tasks) != 1 || tasks[0] != ledger.Binding.TaskID || attempt["attempt_ref"] != ledger.Binding.AttemptRef || attempt["trial_ref"] != ledger.Binding.RunRef || last["unit_ref"] != ledger.Binding.UnitRef {
		t.Fatalf("journal binding %v", attempt)
	}
	// Original source_task_id remains absent for this synthetic eval without a source episode.
	raw, _, err := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace, WorkUnits: revisions}.Build(l, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "HINTCANARY") {
		t.Fatal("raw capture in report")
	}
	var reported map[string]any
	if err := json.Unmarshal(raw, &reported); err != nil {
		t.Fatal(err)
	}
	for _, item := range reported["trials"].([]any) {
		if trial := item.(map[string]any); trial["source_task_id"] != nil {
			t.Fatal("controlled run task replaced original episode provenance")
		}
	}

	pack, err := project.BuildPackage(s, project.ExportOptions{All: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range pack.Docs {
		if strings.Contains(string(doc.Raw), "HINTCANARY") || strings.Contains(string(doc.Raw), "/app/") {
			t.Fatal("raw content escaped export")
		}
	}
	trials, _ := l.Manifests("trial")
	var localTrial map[string]any
	if err := l.Manifest("trial", trials[0], &localTrial); err != nil {
		t.Fatal(err)
	}
	if project.TaskID(s.Key(), str(localTrial["objective"])) != ledger.Binding.TaskID {
		t.Fatal("actual declared run task mismatch")
	}
	var diag BehaviorImport
	if err := l.Index("behavior-import/"+trials[0], &diag); err != nil || diag.State != "imported" || diag.Diagnostics.Scope != "root_session_terminal_tools" {
		t.Fatalf("diagnostics %+v %v", diag, err)
	}
}
func TestBehaviorCaptureFailurePublishesNoPartialSourceOrEvents(t *testing.T) {
	for _, c := range []struct {
		name, status, version, raw string
		runOK                      bool
	}{
		{"truncated", "truncated", "2.0.26", terminalCapture(), true}, {"unsupported", "complete", "2.0.27", terminalCapture(), true},
		{"malformed", "complete", "2.0.26", terminalCapture() + "not json\n", true}, {"runner_error", "complete", "2.0.26", terminalCapture(), false},
		{"oversize", "complete", "2.0.26", strings.Repeat("x", behaviorCaptureLimit+1), true},
		{"baseline_mismatch", "complete", "2.0.26", terminalCapture(), true},
		{"missing_observed_start", "complete", "2.0.26", terminalCapture(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, l := behaviorProject(t)
			names, _ := l.Manifests("eval")
			var rec map[string]any
			l.Manifest("eval", names[len(names)-1], &rec)
			work := t.TempDir()
			dir := filepath.Join(work, "jobs", "trial", "trial__1")
			os.MkdirAll(filepath.Join(dir, "agent"), 0o700)
			os.WriteFile(filepath.Join(dir, "agent", "opencode.txt"), []byte(c.raw), 0o600)
			raw, _ := json.Marshal(map[string]any{"hint_container": map[string]any{"agent_binary": map[string]any{"version": c.version}, "agent_output": map[string]string{"stdout": c.status}}})
			os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o600)
			// Only the complete/valid path needs the BASE input; malformed input still decodes atomically.
			if _, err := runner.Export(l, rec, filepath.Join(work, "task"), runner.DefaultSettings); err != nil {
				t.Fatal(err)
			}
			if c.name == "baseline_mismatch" {
				os.WriteFile(filepath.Join(work, "task", "environment", "repo", "cart.go"), []byte("wrong baseline"), 0o600)
			}
			manifest := workunits.Manifest{Agent: "opencode", AgentVersion: "2.0.26", Harness: "hint-docker", RunnerVersion: runner.DockerRunnerSemver, ModelLabel: "synthetic", MaxRequests: 5, MaxSeconds: 30}
			result := handOverBehavior(context.Background(), s, l, rec, work, manifest, "trial", "task", "family", c.runOK, c.name == "missing_observed_start", "")
			if c.name == "missing_observed_start" && result.Reason != "observed_start_unusable" {
				t.Fatalf("missing capture diagnostic %+v", result)
			}
			if result.State != "unknown" || result.Reason == "" {
				t.Fatalf("result %+v", result)
			}
			sources, err := project.Sources(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range sources {
				if source.Adapter.Name == hint.CLIRootAdapter {
					t.Fatal("partial source committed")
				}
			}
			obs, _, err := project.ReadSpoolChecked(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range obs {
				if o.Source == "log" {
					t.Fatal("partial behavior committed")
				}
			}
		})
	}
}

// This is a recorded-style engine artifact, not output from an agent.
func writeObservedArchive(name string) (string, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		return "", err
	}
	body := []byte("package shop\nfunc ObservedOnly() {}\n")
	if err := tw.WriteHeader(&tar.Header{Name: "app/observed_only.go", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}); err != nil {
		return "", err
	}
	if _, err := tw.Write(body); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := os.WriteFile(name, b.Bytes(), 0600); err != nil {
		return "", err
	}
	hash := sha256.Sum256(b.Bytes())
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

func TestRunSuiteStudyUsesObservedStartWithoutChangingProjectSnapshot(t *testing.T) {
	s, l := behaviorProject(t)
	snapshots := s.Path("state", "snapshots")
	before := map[string]string{}
	filepath.WalkDir(snapshots, func(name string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(name)
			before[name] = string(b)
		}
		return err
	})
	live, stop := liveProject(t)
	defer stop()
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	p.Model = &evals.Model{Use: "lan", BaseURL: "http://192.168.1.2/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY"}
	s.Cfg.Evals = &p
	one := 1.0
	r := &behaviorFakeRunner{liveRunner: liveRunner{model: "local-coder", results: []runner.Trial{{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}}}}, status: "complete", version: activity.CLIRootAgentVersion, raw: strings.ReplaceAll(terminalCapture(), "discount_test.go", "observed_only.go")}
	rep, err := RunSuite(context.Background(), s, r, runner.DockerRunnerSemver, SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, CapturePrelaunchTree: true}, time.Now())
	if err != nil || rep.BehaviorImportProblems != 0 || !r.captureRequested {
		t.Fatalf("result %+v requested %v error %v", rep, r.captureRequested, err)
	}
	observations, damage, err := project.ReadSpoolChecked(s)
	if err != nil || damage.Any() {
		t.Fatal(damage, err)
	}
	var behavior []activity.Observation
	for _, o := range observations {
		if o.Source == "log" {
			behavior = append(behavior, o)
		}
	}
	if len(behavior) != 2 || len(behavior[0].Targets) != 0 || len(behavior[1].Targets) != 1 || behavior[1].Targets[0].EntityID != hint.FileID(s.Key(), s.RepoID(s.Cfg.Repos[0]), "observed_only.go") {
		t.Fatalf("observed targets %+v", behavior)
	}
	trials, _ := l.Manifests("trial")
	var diagnostic BehaviorImport
	if err := l.Index("behavior-import/"+trials[0], &diagnostic); err != nil || diagnostic.BaseTree == "" || diagnostic.ObservedStartTree == "" || diagnostic.BaseTree == diagnostic.ObservedStartTree {
		t.Fatalf("separate provenance %+v %v", diagnostic, err)
	}
	after := map[string]string{}
	filepath.WalkDir(snapshots, func(name string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(name)
			after[name] = string(b)
		}
		return err
	})
	if len(before) != len(after) {
		t.Fatal("project snapshot count changed")
	}
	for name, body := range before {
		if after[name] != body {
			t.Fatal("project snapshot changed")
		}
	}
	pack, err := project.BuildPackage(s, project.ExportOptions{All: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range pack.Docs {
		for _, raw := range []string{"HINTCANARY", "observed_only.go", "prelaunch_tree", "observed_start_tree", "/app/"} {
			if strings.Contains(string(doc.Raw), raw) {
				t.Fatalf("raw study metadata escaped: %s", raw)
			}
		}
	}
}

func TestObservedStartRejectsMissingConflictingOrLateEvidenceWithoutBASEFallback(t *testing.T) {
	s, l := behaviorProject(t)
	names, _ := l.Manifests("eval")
	var rec map[string]any
	if err := l.Manifest("eval", names[len(names)-1], &rec); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if _, err := runner.Export(l, rec, filepath.Join(work, "task"), runner.DefaultSettings); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "jobs", "trial", "trial__1", "agent")
	os.MkdirAll(dir, 0700)
	artifact := filepath.Join(dir, "prelaunch.tar")
	digest, err := writeObservedArchive(artifact)
	if err != nil {
		t.Fatal(err)
	}
	execution := "sha256:" + strings.Repeat("c", 64)
	makeJob := func() jobResult {
		var job jobResult
		start := int64(4)
		job.Container.PrelaunchTree = &runner.PrelaunchTreeCapture{State: "complete", Digest: digest, Clock: "ledger_anchor_monotonic_ms", StartedMs: 1, FinishedMs: 3, ExecutionDigest: execution}
		job.Container.AgentBinary.ExecutionDigest = execution
		job.Container.Ledger = &runner.LedgerReport{AgentRun: &runner.ReportAgentRun{ExecutionDigest: execution, EnclosureStartMs: &start}}
		return job
	}
	for name, mutate := range map[string]func(*jobResult){
		"missing capture":   func(j *jobResult) { j.Container.PrelaunchTree = nil },
		"failed capture":    func(j *jobResult) { j.Container.PrelaunchTree.State = "failed" },
		"wall clock":        func(j *jobResult) { j.Container.PrelaunchTree.Clock = "wall" },
		"negative start":    func(j *jobResult) { j.Container.PrelaunchTree.StartedMs = -1 },
		"reversed interval": func(j *jobResult) { j.Container.PrelaunchTree.StartedMs = 5 },
		"late capture":      func(j *jobResult) { j.Container.PrelaunchTree.FinishedMs = 5 },
		"missing ledger":    func(j *jobResult) { j.Container.Ledger = nil },
		"missing enclosure": func(j *jobResult) { j.Container.Ledger.AgentRun.EnclosureStartMs = nil },
		"other execution":   func(j *jobResult) { j.Container.Ledger.AgentRun.ExecutionDigest = "sha256:" + strings.Repeat("d", 64) },
		"other binary":      func(j *jobResult) { j.Container.AgentBinary.ExecutionDigest = "sha256:" + strings.Repeat("d", 64) },
		"malformed execution": func(j *jobResult) {
			j.Container.PrelaunchTree.ExecutionDigest = "x"
			j.Container.AgentBinary.ExecutionDigest = "x"
			j.Container.Ledger.AgentRun.ExecutionDigest = "x"
		},
		"wrong digest": func(j *jobResult) { j.Container.PrelaunchTree.Digest = "sha256:" + strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			job := makeJob()
			mutate(&job)
			resolve, _, _, cleanup, err := observedStartResolver(context.Background(), s, rec, work, job)
			if cleanup != nil {
				defer cleanup()
			}
			if err == nil || resolve != nil {
				t.Fatal("invalid capture fell back to declared BASE")
			}
		})
	}
	// Complete metadata cannot replace a missing or corrupt archive, even when BASE exists.
	for _, body := range [][]byte{nil, []byte("not a tar")} {
		if body == nil {
			os.Remove(artifact)
		} else {
			os.WriteFile(artifact, body, 0600)
		}
		resolve, _, _, cleanup, err := observedStartResolver(context.Background(), s, rec, work, makeJob())
		if cleanup != nil {
			defer cleanup()
		}
		if err == nil || resolve != nil {
			t.Fatal("missing or corrupt artifact fell back")
		}
	}
}
