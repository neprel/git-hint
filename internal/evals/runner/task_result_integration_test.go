//go:build unix

package runner_test

// Real project producers and runner callbacks with existing synthetic SDKs only.
// No candidate command, model or external engine executes.
import (
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func terminalGitNodes(t *testing.T, root string) []runner.SelectedMappingTestNode {
	t.Helper()
	var out []runner.SelectedMappingTestNode
	e := filepath.WalkDir(filepath.Join(root, ".git"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		s, e := d.Info()
		if e != nil {
			return e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		out = append(out, runner.SelectedMappingTestNode{Path: filepath.ToSlash(rel), Body: string(b), Mode: int64(s.Mode().Perm())})
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestSelectedResultActualProducerAndFrozenB1(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	f := newMappingFixture(t)
	preparePolicy, verifyPolicy := runner.SelectedResultTestPolicies(t)
	prepared, e := project.PrepareFinalReference(ctx, f.s, project.PrepareReferenceRequest{Definition: f.source.Identity().DefinitionRef, Label: "selected-mapping-unit", OperationID: "actual-terminal-reference", Policy: preparePolicy}, time.Now())
	if e != nil {
		t.Fatal("actual prepare", e)
	}
	h, e := project.LoadPreparedFinalReference(ctx, f.s, *prepared.Prepared())
	if e != nil {
		t.Fatal(e)
	}
	d := f.source.Identity().Definition
	setBytes, e := f.l.GetObject(d.RawSetObject)
	if e != nil {
		t.Fatal(e)
	}
	baselineRef := f.baseline.Binding().Observation
	foreignDef, e := project.WriteWorkDefinitionInputs(ctx, f.s, project.DefinitionRequest{Label: "foreign-terminal-unit", Family: "synthetic-fix", ExactSetBytes: setBytes, PlanID: d.PlanBinding.PlanID, DeclaredConfiguration: d.DeclaredConfiguration, QualifiedIO: d.QualifiedInputRequest, Baseline: &baselineRef}, time.Now())
	if e != nil {
		t.Fatal("foreign definition control", e)
	}
	foreignPrepared, e := project.PrepareFinalReference(ctx, f.s, project.PrepareReferenceRequest{Definition: foreignDef, Label: "foreign-terminal-unit", OperationID: "foreign-terminal-reference", Policy: preparePolicy}, time.Now())
	if e != nil {
		t.Fatal("foreign reference control", e)
	}
	foreignHandle, e := project.LoadPreparedFinalReference(ctx, f.s, *foreignPrepared.Prepared())
	if e != nil {
		t.Fatal(e)
	}
	live, file := runner.SelectedMappingTestLive(t)
	r := &runner.SelectedMappingTestRunner{T: t, Nodes: terminalGitNodes(t, f.s.Root), FinalNodes: []runner.SelectedMappingTestNode{{Path: "main.go", Body: "package main\nfunc Value()int{return 2}\n", Mode: 0644}}}
	var mapping *pipeline.SelectedProjectMapping
	var result *pipeline.SelectedProjectResult
	var observed *pipeline.ObservedProjectResult
	var callbackErr error
	_, e = pipeline.RunSuite(ctx, f.s, r, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file,
		PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
			mapping, callbackErr = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
			return callbackErr
		},
		ResultCheck: func(ctx context.Context, v *pipeline.ObservedProjectResult) error {
			observed = v
			result, callbackErr = pipeline.CheckSelectedProjectResult(ctx, mapping, v)
			if result != nil && result.Ready() {
				t.Fatal("pending result usable")
			}
			return callbackErr
		}}, time.Now())
	if e != nil || callbackErr != nil || result == nil || !result.Ready() {
		t.Fatalf("actual result missing e=%v callback=%v result=%v starts=%d problem=%s", e, callbackErr, result != nil, r.Starts, r.Problem)
		t.Log(r.Problem)
	}
	_, rid := result.Identity()
	if rid.Artifact.Digest == mapping.Identity().BaseArchive || rid.Artifact.Digest == h.Identity().Audit.FinalTAR.Digest {
		t.Fatal("artifact domains substituted")
	}
	selected, e := result.ResolveSelectedInputs(ctx, trees.Limits{})
	if e != nil {
		t.Fatal("terminal strict pair", e)
	}
	sid := selected.Identity()
	if sid.Selected.TreeDigest == rid.Artifact.Digest || sid.State != "terminal_selected_correspondence_observed" || sid.Authority != "unknown" {
		t.Fatal("domain/authority")
	}
	changed := false
	for _, n := range sid.Compared {
		if n.Path == "main.go" {
			changed = true
		}
	}
	if !changed {
		t.Fatal("terminal bytes not selected")
	}
	snap := selected.Snapshot()
	if snap == nil || snap.Record.RepoID != f.baseline.Binding().RepoID {
		t.Fatal("terminal graph")
	}
	if b, e := json.Marshal(selected); e == nil {
		t.Fatalf("raw serialization %s", b)
	}
	verified, e := pipeline.VerifySelectedProjectResult(ctx, result, h, verifyPolicy)
	if e != nil {
		t.Fatal("frozen B1", e)
	}
	id := verified.Observation().Identity()
	if id.State != "finite_equivalence_observed" || id.Authority != "unknown" || len(id.Cases) != 3 || id.Candidate.Digest != rid.Artifact.Digest || id.Reference.Digest != h.Identity().Audit.FinalTAR.Digest || id.OracleOutputsDigest != h.Identity().Audit.OracleOutputsDigest {
		t.Fatal("actual frozen bindings", id.State, len(id.Cases))
	}
	for i, c := range id.Cases {
		role := "reference_case"
		rep := i
		if i == 2 {
			role = "candidate_case"
			rep = 0
		}
		if c.Role != role || c.Repetition != rep || !c.Complete || !c.Phase.Removed || !c.Phase.StreamsJoined || !c.Phase.NetworkNone {
			t.Fatal("case/audit order", i, c)
		}
	}
	if _, e = pipeline.VerifySelectedProjectResult(ctx, result, h, runner.FrozenVerificationPolicyRef{PolicyID: "missing", PolicyDigest: "sha256:" + strings.Repeat("a", 64)}); e == nil {
		t.Fatal("default policy accepted")
	}
	if _, e := pipeline.VerifySelectedProjectResult(ctx, result, foreignHandle, verifyPolicy); e == nil {
		t.Fatal("foreign prepared definition accepted")
	}
	second := &runner.SelectedMappingTestRunner{T: t, Nodes: terminalGitNodes(t, f.s.Root)}
	var foreignMapping *pipeline.SelectedProjectMapping
	_, e = pipeline.RunSuite(ctx, f.s, second, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file, PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
		var e error
		foreignMapping, e = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
		return e
	}}, time.Now())
	if e != nil || foreignMapping == nil || second.Starts != 1 {
		t.Fatal("foreign actual occurrence control", e, second.Problem)
	}
	if _, e := pipeline.CheckSelectedProjectResult(ctx, foreignMapping, observed); e == nil {
		t.Fatal("foreign actual occurrence accepted")
	}

}

func TestSelectedResultActualCallbackRefusals(t *testing.T) {
	for _, mode := range []string{"panic", "panic_after_identity", "error", "cancel", "close", "remove", "missing_git"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			f := newMappingFixture(t)
			live, file := runner.SelectedMappingTestLive(t)
			r := &runner.SelectedMappingTestRunner{T: t, FailResultClose: mode == "close", FailResultRemove: mode == "remove"}
			if mode != "missing_git" {
				r.Nodes = terminalGitNodes(t, f.s.Root)
			}
			var m *pipeline.SelectedProjectMapping
			var result *pipeline.SelectedProjectResult
			callbacks := 0
			_, _ = pipeline.RunSuite(ctx, f.s, r, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file,
				PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
					var e error
					m, e = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
					return e
				},
				ResultCheck: func(ctx context.Context, v *pipeline.ObservedProjectResult) error {
					callbacks++
					var e error
					result, e = pipeline.CheckSelectedProjectResult(ctx, m, v)
					if e != nil {
						return e
					}
					switch mode {
					case "panic":
						panic("private callback text")
					case "panic_after_identity":
						_, _ = result.Identity()
						panic("private identity callback")
					case "error":
						return context.Canceled
					case "cancel":
						cancel()
					}
					return nil
				}}, time.Now())
			if mode == "missing_git" {
				if result == nil || !result.Ready() {
					t.Fatal("full TAR fact missing")
				}
				if _, e := result.ResolveSelectedInputs(context.Background(), trees.Limits{}); e == nil {
					t.Fatal("borrowed Git")
				}
				return
			}
			if mode == "close" || mode == "remove" {
				if callbacks != 0 {
					t.Fatal("callback after failed cleanup", callbacks)
				}
			} else if callbacks != 1 {
				t.Fatal("callback never reached", r.Problem)
			}
			if result != nil && result.Ready() {
				t.Fatal("failed delivery usable")
			}
			if strings.Contains(r.Problem, "private") {
				t.Fatal("panic leak")
			}
		})
	}
}
