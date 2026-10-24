package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/project"
)

type studySuiteFake struct {
	behaviorFakeRunner
	policy      runner.StudyLaunchPolicy
	planned     string
	omitReceipt bool
}

func (r *studySuiteFake) SupportsStudyLaunchPolicy(p runner.StudyLaunchPolicy) bool {
	return p == runner.StudyLaunchSingleRootV1
}
func (r *studySuiteFake) Run(ctx context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	r.policy, r.planned = o.StudyLaunchPolicy, o.ExpectLaunchDigest
	trials, err := r.behaviorFakeRunner.Run(ctx, task, o)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(o.JobsDir, "trial", "trial__1", "result.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	container := doc["hint_container"].(map[string]any)
	image := "sha256:" + strings.Repeat("c", 64)
	container["agent_image"] = image
	if !r.omitReceipt {
		zero := int64(0)
		container["study_launch"] = &runner.StudyLaunchReceipt{Policy: o.StudyLaunchPolicy, State: "prepared", Clock: "ledger_anchor_monotonic_ms", StartedMs: &zero, FinishedMs: &zero, LaunchDigest: o.ExpectLaunchDigest, ExecutionDigest: "sha256:" + strings.Repeat("d", 64), ImageID: image, LauncherDigest: "sha256:" + strings.Repeat("a", 64), InventoryDigest: "sha256:" + strings.Repeat("b", 64), BinaryValidation: "not_run", Eligibility: "requires_real_binary_validation"}
	}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		return nil, err
	}
	return trials, nil
}
func TestRunSuiteSingleRootPolicyBindsPlanAndKeepsReceiptLocal(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "missing receipt"}[missing], func(t *testing.T) {
			s, l := behaviorProject(t)
			live, stop := liveProject(t)
			defer stop()
			p := evals.Enabled(nil)
			p.Run = &evals.Run{Use: "docker", Runner: "docker"}
			p.Model = &evals.Model{Use: "lan", BaseURL: "http://192.168.1.2/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY"}
			s.Cfg.Evals = &p
			one := 1.0
			r := &studySuiteFake{behaviorFakeRunner: behaviorFakeRunner{liveRunner: liveRunner{model: "local-coder", results: []runner.Trial{{Reward: &one, Checks: map[string]string{"C1": "pass", "C2": "pass"}}}}, status: "complete", version: activity.CLIRootAgentVersion, raw: strings.ReplaceAll(terminalCapture(), "discount_test.go", "observed_only.go")}, omitReceipt: missing}
			rep, err := RunSuite(context.Background(), s, r, runner.DockerRunnerSemver, SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, CapturePrelaunchTree: true, StudyLaunchPolicy: runner.StudyLaunchSingleRootV1}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			want, _ := runner.DockerLaunchDigestWithPolicy(runner.DockerOpenCodeVersion, "local-coder", "linux-arm64", runner.StudyLaunchSingleRootV1)
			if r.policy != runner.StudyLaunchSingleRootV1 || r.planned != want {
				t.Fatal("planned A did not reach actual runner options")
			}
			observations, damage, err := project.ReadSpoolChecked(s)
			if err != nil || damage.Any() {
				t.Fatal(damage, err)
			}
			count := 0
			for _, o := range observations {
				if o.Source == "log" {
					count++
				}
			}
			if missing {
				if count != 0 || rep.BehaviorImportProblems == 0 {
					t.Fatal("missing receipt imported behavior")
				}
				names, _ := l.Manifests("trial")
				var diag BehaviorImport
				l.Index("behavior-import/"+names[0], &diag)
				if diag.Reason != "study_launch_unusable" {
					t.Fatal(diag)
				}
			} else if count != 2 || rep.BehaviorImportProblems != 0 {
				t.Fatalf("behavior %d report %+v", count, rep)
			}
			pack, err := project.BuildPackage(s, project.ExportOptions{All: true, Now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			for _, doc := range pack.Docs {
				for _, value := range []string{"study_launch", "single-root-v1", "launcher_digest", "inventory_digest", "requires_real_binary_validation", "observed_only.go"} {
					if strings.Contains(string(doc.Raw), value) {
						t.Fatal("local study fields escaped export", value)
					}
				}
			}
		})
	}
}
func TestSuiteStudyRejectsUnsupportedRunnerAndMissingCapture(t *testing.T) {
	s, _ := behaviorProject(t)
	plain := &behaviorFakeRunner{}
	supporting := &studySuiteFake{}
	for _, test := range []struct {
		policy  runner.StudyLaunchPolicy
		capture bool
		support bool
	}{{"unknown", true, true}, {runner.StudyLaunchSingleRootV1, true, false}, {runner.StudyLaunchSingleRootV1, false, true}} {
		options := SuiteOptions{Agent: "opencode", CapturePrelaunchTree: test.capture, StudyLaunchPolicy: test.policy}
		var err error
		if test.support {
			_, err = RunSuite(context.Background(), s, supporting, runner.DockerRunnerSemver, options, time.Now())
		} else {
			_, err = RunSuite(context.Background(), s, plain, runner.DockerRunnerSemver, options, time.Now())
		}
		if err == nil || plain.n != 0 || supporting.n != 0 {
			t.Fatal("invalid study reached runner")
		}
	}
}
func TestStudyReceiptRejectsMismatchedOrOverstatedProof(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	image := "sha256:" + strings.Repeat("c", 64)
	a := "sha256:" + strings.Repeat("a", 64)
	makeJob := func() jobResult {
		start, end := int64(1), int64(3)
		var j jobResult
		j.Container.ImageID = image
		j.Container.AgentBinary.ExecutionDigest = digest
		j.Container.PrelaunchTree = &runner.PrelaunchTreeCapture{StartedMs: 4, ExecutionDigest: digest}
		j.Container.StudyLaunch = &runner.StudyLaunchReceipt{Policy: runner.StudyLaunchSingleRootV1, State: "prepared", Clock: "ledger_anchor_monotonic_ms", StartedMs: &start, FinishedMs: &end, LaunchDigest: a, ExecutionDigest: digest, ImageID: image, LauncherDigest: a, InventoryDigest: a, BinaryValidation: "not_run", Eligibility: "requires_real_binary_validation"}
		return j
	}
	m := workunits.Manifest{LaunchDigest: a}
	if !studyBehaviorReceipt(makeJob(), m, runner.StudyLaunchSingleRootV1) {
		t.Fatal("valid local inspection refused")
	}
	for name, mutate := range map[string]func(*jobResult){"missing": func(j *jobResult) { j.Container.StudyLaunch = nil },
		"missing interval": func(j *jobResult) { j.Container.StudyLaunch.StartedMs = nil }, "other policy": func(j *jobResult) { j.Container.StudyLaunch.Policy = "" }, "failed": func(j *jobResult) { j.Container.StudyLaunch.State = "failed" }, "late": func(j *jobResult) { late := int64(5); j.Container.StudyLaunch.FinishedMs = &late }, "wall clock": func(j *jobResult) { j.Container.StudyLaunch.Clock = "wall" }, "other plan": func(j *jobResult) { j.Container.StudyLaunch.LaunchDigest = image }, "other image": func(j *jobResult) { j.Container.ImageID = a }, "other execution": func(j *jobResult) { j.Container.StudyLaunch.ExecutionDigest = image }, "missing launcher": func(j *jobResult) { j.Container.StudyLaunch.LauncherDigest = "" }, "false eligibility": func(j *jobResult) { j.Container.StudyLaunch.Eligibility = "eligible" }, "unsupported binary claim": func(j *jobResult) { j.Container.StudyLaunch.BinaryValidation = "verified" }} {
		t.Run(name, func(t *testing.T) {
			j := makeJob()
			mutate(&j)
			if studyBehaviorReceipt(j, m, runner.StudyLaunchSingleRootV1) {
				t.Fatal("accepted conflicting or overstated receipt")
			}
		})
	}
}
