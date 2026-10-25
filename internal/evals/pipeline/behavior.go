package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/store"
)

var observedStartDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const behaviorCaptureLimit = 8 << 20 // The runner's complete stdout capture bound.

type BehaviorImport struct {
	State             string                       `json:"state"`
	Reason            string                       `json:"reason,omitempty"`
	RunRef            string                       `json:"run_ref"`
	ObservedStartTree string                       `json:"observed_start_tree,omitempty"`
	BaseTree          string                       `json:"base_tree,omitempty"`
	SourceID          string                       `json:"source_id,omitempty"`
	Diagnostics       *activity.CLIRootDiagnostics `json:"diagnostics,omitempty"`
}

// handOverBehavior imports only the runner-observed complete stdout of this actual trial.
// Its default resolver scans immutable BASE; study opt-in scans observed start instead.
// Neither path scans the current project or final agent tree.
func handOverBehavior(ctx context.Context, s *store.Store, l *library.Library, rec map[string]any, work string, m workunits.Manifest,
	trial, task, family string, runOK bool, observedStart bool, policy runner.StudyLaunchPolicy) BehaviorImport {
	result := BehaviorImport{State: "unknown", RunRef: activity.RunRef(s.Key(), trial)}
	fail := func(code string) BehaviorImport { result.Reason = code; return result }
	if !runOK {
		return fail("runner_error")
	}
	job, err := readJobResult(work)
	if err != nil {
		return fail("capture_metadata_unavailable")
	}
	if policy != "" && (!observedStart || !studyBehaviorReceipt(job, m, policy)) {
		return fail("study_launch_unusable")
	}
	if job.Container.AgentOutput["stdout"] != "complete" {
		return fail("capture_not_complete")
	}
	if job.Container.AgentBinary.Version != activity.CLIRootAgentVersion {
		return fail("agent_version_unsupported")
	}
	file := filepath.Join(work, "jobs", "trial", "trial__1", "agent", "opencode.txt")
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > behaviorCaptureLimit {
		return fail("capture_unavailable_or_oversize")
	}
	f, err := os.Open(file)
	if err != nil {
		return fail("capture_unavailable_or_oversize")
	}
	raw, err := io.ReadAll(io.LimitReader(f, behaviorCaptureLimit+1))
	f.Close()
	if err != nil || len(raw) > behaviorCaptureLimit {
		return fail("capture_unavailable_or_oversize")
	}
	cfg, err := ledgerConfiguration(s.Key(), m)
	if err != nil {
		return fail("configuration_unusable")
	}
	var resolve activity.Resolver
	var base, observed string
	var cleanup func()
	if observedStart {
		resolve, base, observed, cleanup, err = observedStartResolver(ctx, s, rec, work, job)
	} else {
		resolve, base, cleanup, err = behaviorResolver(ctx, s, l, rec, work)
	}
	if err != nil {
		if observedStart {
			return fail("observed_start_unusable")
		}
		return fail("baseline_unusable")
	}
	defer cleanup()
	result.BaseTree, result.ObservedStartTree = base, observed
	decoded, err := activity.DecodeCLIRoot(bytes.NewReader(raw), activity.CLIRootOptions{Key: s.Key(), AgentVersion: job.Container.AgentBinary.Version,
		Configuration: &cfg, TaskID: project.TaskID(s.Key(), task), TaskFamilyID: activity.FamilyRef(s.Key(), family), RunRef: result.RunRef,
		Cwd: "/app", Resolve: resolve, CaptureComplete: true, CapturedAt: time.Now().UTC()})
	if err != nil {
		var e *activity.CLIRootError
		if errors.As(err, &e) {
			return fail(e.Code)
		}
		return fail("decode_failed")
	}
	if err = project.WriteActivityBatch(s, decoded.Source, decoded.Observations); err != nil {
		return fail("spool_import_failed")
	}
	result.State = "imported"
	result.SourceID = decoded.Source.SourceID
	result.Diagnostics = &decoded.Diagnostics
	return result
}

func behaviorResolver(ctx context.Context, s *store.Store, l *library.Library, rec map[string]any, work string) (activity.Resolver, string, func(), error) {
	bad := errors.New("frozen baseline unavailable")
	repos, ok := rec["repositories"].([]any)
	if !ok || len(repos) != 1 {
		return nil, "", nil, bad
	}
	repo, ok := repos[0].(map[string]any)
	if !ok {
		return nil, "", nil, bad
	}
	repoID := ""
	for _, r := range s.Cfg.Repos {
		if r.Name == str(repo["repo"]) {
			if repoID != "" {
				return nil, "", nil, bad
			}
			repoID = s.RepoID(r)
		}
	}
	if repoID == "" {
		return nil, "", nil, bad
	}
	base := str(repo["base_tree"])
	tree, err := trees.Load(l, base)
	if err != nil {
		return nil, "", nil, err
	}
	temp, err := os.MkdirTemp(s.Path("cache"), "behavior-base-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() { os.RemoveAll(temp) }
	if err = trees.Materialize(l, base, temp); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	files := []string{}
	expected := map[string]bool{}
	exported := filepath.Join(work, "task", "environment", "repo")
	// Confirm this tree is also exactly the runner's exported starting input.
	for _, entry := range tree.Files {
		expected[entry.Path] = true
		name := filepath.Join(exported, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(name)
		if err != nil {
			cleanup()
			return nil, "", nil, bad
		}
		var content []byte
		if entry.Mode == "120000" {
			if info.Mode()&os.ModeSymlink == 0 {
				cleanup()
				return nil, "", nil, bad
			}
			target, e := os.Readlink(name)
			err = e
			content = []byte(target)
		} else {
			if !info.Mode().IsRegular() || (info.Mode().Perm()&0o111 != 0) != (entry.Mode == "100755") {
				cleanup()
				return nil, "", nil, bad
			}
			content, err = os.ReadFile(name)
			files = append(files, entry.Path)
		}
		if err != nil || library.Digest(content) != entry.Digest {
			cleanup()
			return nil, "", nil, bad
		}
	}
	err = filepath.WalkDir(exported, func(name string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(exported, name)
		if e != nil || !expected[filepath.ToSlash(rel)] {
			return bad
		}
		return nil
	})
	if err != nil {
		cleanup()
		return nil, "", nil, bad
	}
	scanned, err := scan.Scan(scan.Options{Ctx: ctx, Root: temp, RepoID: repoID, Key: s.Key(), Files: files})
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	ids := map[string]string{}
	for _, entity := range scanned.Entities {
		if entity.Kind == "file" {
			ids["/app/"+entity.Path] = entity.ID
		}
	}
	return func(name string) (string, bool) {
		if !strings.HasPrefix(name, "/app/") || path.Clean(name) != name {
			return "", false
		}
		id, ok := ids[name]
		return id, ok
	}, base, cleanup, nil
}

// observedStartResult owns one materialization and one scan. It is local factual
// input, not a public snapshot or capture authority. The graph is never rescanned.
type observedStartResult struct {
	resolve  activity.Resolver
	graph    *scan.Result
	repoID   string
	capture  runner.PrelaunchTreeCapture
	baseTree string
	cleanup  func()
	once     sync.Once
}

func (r *observedStartResult) close() { r.once.Do(r.cleanup) }

// observedStartResolver never substitutes BASE for an unavailable capture.
// It preserves the behavior-only interface and publishes no project snapshot.
func observedStartResolver(ctx context.Context, s *store.Store, rec map[string]any, work string, job jobResult) (activity.Resolver, string, string, func(), error) {
	r, err := resolveObservedStart(ctx, s, rec, work, job)
	if err != nil {
		return nil, "", "", nil, err
	}
	return r.resolve, r.baseTree, r.capture.Digest, r.close, nil
}

func resolveObservedStart(ctx context.Context, s *store.Store, rec map[string]any, work string, job jobResult) (*observedStartResult, error) {
	bad := errors.New("observed start unavailable")
	capture := job.Container.PrelaunchTree
	ledger := job.Container.Ledger
	if capture == nil || capture.State != "complete" || capture.Clock != "ledger_anchor_monotonic_ms" || capture.StartedMs < 0 || capture.FinishedMs < capture.StartedMs ||
		ledger == nil || ledger.AgentRun == nil || ledger.AgentRun.EnclosureStartMs == nil || capture.FinishedMs > *ledger.AgentRun.EnclosureStartMs ||
		!observedStartDigestPattern.MatchString(capture.ExecutionDigest) || capture.ExecutionDigest != ledger.AgentRun.ExecutionDigest || capture.ExecutionDigest != job.Container.AgentBinary.ExecutionDigest {
		return nil, bad
	}
	repos, ok := rec["repositories"].([]any)
	if !ok || len(repos) != 1 {
		return nil, bad
	}
	repo, ok := repos[0].(map[string]any)
	if !ok || str(repo["base_tree"]) == "" {
		return nil, bad
	}
	repoID := ""
	for _, configured := range s.Cfg.Repos {
		if configured.Name == str(repo["repo"]) {
			if repoID != "" {
				return nil, bad
			}
			repoID = s.RepoID(configured)
		}
	}
	if repoID == "" {
		return nil, bad
	}
	temp, err := os.MkdirTemp(s.Path("cache"), "behavior-observed-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(temp) }
	artifact := filepath.Join(work, "jobs", "trial", "trial__1", "agent", "prelaunch.tar")
	files, err := runner.MaterializePrelaunchTree(artifact, capture.Digest, temp)
	if err != nil {
		cleanup()
		return nil, err
	}
	if len(files) == 0 {
		cleanup()
		return nil, bad
	}
	scanned, err := scan.Scan(scan.Options{Ctx: ctx, Root: temp, RepoID: repoID, Key: s.Key(), Files: files})
	if err != nil {
		cleanup()
		return nil, err
	}
	ids := map[string]string{}
	for _, entity := range scanned.Entities {
		if entity.Kind == "file" {
			ids["/app/"+entity.Path] = entity.ID
		}
	}
	resolve := func(name string) (string, bool) {
		if !strings.HasPrefix(name, "/app/") || path.Clean(name) != name {
			return "", false
		}
		id, ok := ids[name]
		return id, ok
	}
	return &observedStartResult{resolve: resolve, graph: scanned, repoID: repoID, capture: *capture, baseTree: str(repo["base_tree"]), cleanup: cleanup}, nil
}

// Inspection-only receipts do not grant canonical/calibration eligibility. The separate
// real-binary acceptance is deliberately not enabled by this local behavior import.
func studyBehaviorReceipt(job jobResult, m workunits.Manifest, p runner.StudyLaunchPolicy) bool {
	r := job.Container.StudyLaunch
	c := job.Container.PrelaunchTree
	if p.Check() != nil || r == nil || c == nil || r.Policy != p || r.State != "prepared" || r.Clock != "ledger_anchor_monotonic_ms" || r.StartedMs == nil || r.FinishedMs == nil || *r.StartedMs < 0 || *r.FinishedMs < *r.StartedMs || *r.FinishedMs > c.StartedMs || r.BinaryValidation != "not_run" || r.Eligibility != "requires_real_binary_validation" || r.LaunchDigest != m.LaunchDigest || r.ExecutionDigest != c.ExecutionDigest || r.ExecutionDigest != job.Container.AgentBinary.ExecutionDigest || r.ImageID != job.Container.ImageID {
		return false
	}
	for _, d := range []string{r.LaunchDigest, r.ExecutionDigest, r.ImageID, r.LauncherDigest, r.InventoryDigest} {
		if !observedStartDigestPattern.MatchString(d) {
			return false
		}
	}
	return true
}
