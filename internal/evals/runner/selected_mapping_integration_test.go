//go:build unix

package runner_test

// SYNTHETIC controls only. The real qualifier derives its state from a fake
// trial stream/audit. The normal project, exporter and fake SDK capture paths run.
// No engine, OpenCode, shell candidate, model or external endpoint executes.
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func mappingWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func mappingTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	names := []string{}
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := files[n]
		if e := w.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(v))}); e != nil {
			t.Fatal(e)
		}
		if _, e := w.Write(v); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

type mappingQualificationRunner struct {
	t     *testing.T
	calls int
}

func (r *mappingQualificationRunner) Run(ctx context.Context, task string, o runner.RunOptions) ([]runner.Trial, error) {
	r.calls++
	spec, e := os.ReadFile(filepath.Join(task, "tests/io/spec.json"))
	if e != nil {
		return nil, e
	}
	decl, e := iodecl.Parse(spec, map[string][]byte{"lower": []byte("hello\n")})
	if e != nil {
		return nil, e
	}
	solution, _ := os.ReadFile(filepath.Join(task, "solution/solve.sh"))
	program, _ := os.ReadFile(filepath.Join(task, "solution/files/tool.sh"))
	reference := o.Agent == "oracle" && !strings.Contains(string(solution), "HINT_PROBE") && strings.Contains(string(program), "tr a-z")
	out := []runner.Trial{}
	for i := 1; i <= max(o.Attempts, 1); i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		name := fmt.Sprintf("%s__%d", o.JobName, i)
		root := filepath.Join(o.JobsDir, o.JobName, name)
		reward := 0.0
		status := "fail"
		if reference {
			reward = 1
			status = "pass"
		}
		artifact := mappingTar(r.t, map[string][]byte{"app/tool.sh": []byte("tr a-z A-Z\n")})
		raw := map[string]any{"trial_name": name, "verifier_result": map[string]any{"rewards": map[string]any{"reward": reward}}, "hint_container": map[string]any{"runner": runner.DockerRunnerVersion, "result_trust": runner.TrustProcessSeparated, "check_trust": map[string]string{"B1": runner.TrustProcessSeparated}, "io": map[string]any{"interface": iodecl.Interface, "spec": decl.SpecDigest, "inputs": decl.InputsDigest, "oracle_runs": runner.OracleRuns, "verdict_scope": "equivalence_to_reference_on_declared_cases", "network": "engine_inspect:blocked", "reference_tree": library.Digest(artifact), "oracle_outputs": library.Digest([]byte("HELLO\n")), "candidate_outputs": library.Digest([]byte(status))}}}
		data, _ := json.Marshal(raw)
		mappingWrite(r.t, filepath.Join(root, "result.json"), data)
		mappingWrite(r.t, filepath.Join(root, "oracle/final.tar"), artifact)
		mappingWrite(r.t, filepath.Join(root, "verifier/test-stdout.txt"), []byte("CHECK B1 "+strings.ToUpper(status)+"\nTEST B1 cases/lower::output "+strings.ToUpper(status)+"\n"))
		mappingWrite(r.t, filepath.Join(root, "verifier/network.txt"), []byte("HINT_VERIFIER_NET=blocked\n"))
		if o.JobName == "probe" {
			mappingWrite(r.t, filepath.Join(root, "agent/oracle.txt"), []byte("HINT_PROBE_NET=blocked\n"))
		}
		out = append(out, runner.Trial{Name: name, Reward: &reward, Checks: map[string]string{"B1": status}, Tests: map[string]string{"B1 cases/lower::output": status}, ResultTrust: runner.TrustProcessSeparated, CheckTrust: map[string]string{"B1": runner.TrustProcessSeparated}})
	}
	return out, nil
}

type mappingFixture struct {
	s        *store.Store
	l        *library.Library
	baseline *project.BaselineInputs
	source   *project.CaptureSourceInputs
	eval     string
}

func newMappingFixture(t *testing.T, edits ...func(*library.Library, map[string]any)) mappingFixture {
	t.Helper()
	cwd, _ := os.Getwd()
	cwd, _ = filepath.EvalSymlinks(cwd)
	domain, e := os.MkdirTemp(cwd, ".mapping-source-fixture-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := os.RemoveAll(domain); e != nil {
			t.Error(e)
		}
	})
	for _, n := range []string{"root", "home", "xdg", "keys"} {
		if e = os.Mkdir(filepath.Join(domain, n), 0700); e != nil {
			t.Fatal(e)
		}
	}
	git, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	git, e = filepath.EvalSymlinks(git)
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range os.Environ() {
		k, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(strings.ToUpper(k), "GIT_") || strings.HasPrefix(strings.ToUpper(k), "DYLD_") || strings.HasPrefix(strings.ToUpper(k), "LD_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	t.Setenv("PATH", filepath.Dir(git)+":/usr/bin:/bin")
	t.Setenv("HOME", filepath.Join(domain, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(domain, "xdg"))
	t.Setenv("HINT_KEY_DIR", filepath.Join(domain, "keys"))
	root := filepath.Join(domain, "root")
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "go.mod", "module example\n\ngo 1.26\n")
	testkit.WriteFile(t, root, "tool.sh", "cat\n")
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value()int{return 1}\n")
	testkit.WriteFile(t, root, "missing.txt", "tracked then absent\n")
	testkit.Git(t, root, "add", "go.mod", "tool.sh", "main.go", "missing.txt")
	testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "-qm", "baseline")
	if e = os.Remove(filepath.Join(root, "missing.txt")); e != nil {
		t.Fatal(e)
	}
	s, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	p.Model = &evals.Model{Use: "lan", BaseURL: "http://192.168.1.2/v1", Model: "m", KeyEnv: "SYNTHETIC_UNUSED"}
	s.Cfg.Evals = &p
	if e = s.SaveConfig(); e != nil {
		t.Fatal(e)
	}
	l := library.Open(s, 0)
	col, e := project.CollectVerifiedBaseline(context.Background(), s, project.BaselineCollectRequest{RepoID: s.RepoID(s.Cfg.Repos[0])})
	if e != nil {
		t.Fatal(e)
	}
	baseline, e := project.LoadBaselineObservation(context.Background(), s, col.Ref())
	if e != nil {
		t.Fatal(e)
	}
	result := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n", "main.go": "package main\nfunc Value()int{return 1}\n"})
	built, e := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Repo: s.Cfg.Repos[0].Name, Base: baseline.Binding().Physical.TreeDigest, Result: result, Request: "Uppercase stdin.", RequestAt: map[string]any{"source": library.Digest([]byte("synthetic declared request")), "record": "request1"}, Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}}, CaseSeconds: 5, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	for _, edit := range edits {
		edit(l, built.Record)
	}
	name, e := l.PutManifest("eval", built.Record)
	if e != nil {
		t.Fatal(e)
	}
	draft, e := pipeline.MapDraft(l, name)
	if e != nil {
		t.Fatal(e)
	}
	for i := range draft.Requirements {
		draft.Requirements[i].Tests = append([]string(nil), draft.Tests...)
	}
	data, _ := json.Marshal(draft)
	name, _, e = pipeline.ApplyMapFile(context.Background(), l, name, data, root, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	var rec map[string]any
	if e = l.Manifest("eval", name, &rec); e != nil {
		t.Fatal(e)
	}
	prevDigest, _ := l.ManifestDigest("eval", name)
	work := t.TempDir()
	fake := &mappingQualificationRunner{t: t}
	res, e := qualify.Run(context.Background(), l, fake, rec, qualify.Options{Work: work, Settings: runner.DefaultSettings})
	if e != nil || res.State != "validated" {
		t.Fatalf("actual qualifier result %+v err=%v", res, e)
	}
	if fake.calls < 3 {
		t.Fatal("qualification did not exercise controls")
	}
	// Persist ONLY the returned qualifier decision and its actual fake audit files.
	files := map[string][]byte{}
	e = filepath.WalkDir(filepath.Join(work, "jobs"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(work, p)
		b, e := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	audit, e := l.PutObject(mappingTar(t, files))
	if e != nil {
		t.Fatal(e)
	}
	checks := []any{}
	for _, c := range res.Checks {
		raw, _ := json.Marshal(c)
		m := map[string]any{}
		json.Unmarshal(raw, &m)
		m["evidence"] = []any{audit}
		checks = append(checks, m)
	}
	reqs := []any{}
	for _, r := range res.Requirements {
		raw, _ := json.Marshal(r)
		m := map[string]any{}
		json.Unmarshal(raw, &m)
		reqs = append(reqs, m)
	}
	rec["version"] = rec["version"].(float64) + 1
	rec["previous"] = name
	rec["state"] = res.State
	rec["reasons"] = []any{}
	rec["qualification"] = map[string]any{"policy": qualify.Policy, "checks": checks, "requirements": reqs, "result_trust": res.ResultTrust}
	name, e = l.PutManifest("eval", rec)
	if e != nil {
		t.Fatal(e)
	}
	digest, _ := l.ManifestDigest("eval", name)
	qreq := qualify.ResolveIORequest{EvalName: name, ManifestDigest: digest, PreviousManifestDigest: prevDigest}
	if _, why := qualify.ResolveQualifiedIO(context.Background(), l, qreq); why != nil {
		t.Fatal("actual derived qualification audit", why)
	}
	pkg, e := project.BuildPackage(s, project.ExportOptions{All: true, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	if e = project.MarkExported(s, pkg, "mapping-fixture", "", time.Now()); e != nil {
		t.Fatal(e)
	}
	server := &testkit.Server{Docs: map[string][]byte{}}
	for _, d := range pkg.Docs {
		server.Docs[d.ID] = d.Raw
	}
	set := server.RecommendationFor(s.Cfg.ProjectNamespace)
	if len(set) == 0 {
		t.Fatal("normal fixture has no recommendation target")
	}
	var doc map[string]any
	json.Unmarshal(set, &doc)
	plans := doc["payload"].(map[string]any)["plans"].([]any)
	plan := plans[0].(map[string]any)["plan_id"].(string)
	cfg := activity.Configuration{}
	cfg.Agent.Name = "opencode"
	cfg.Agent.Version = activity.VersionState{State: "known", Value: runner.DockerOpenCodeVersion}
	cfg.Model.State = "unknown"
	cfg.Harness.Kind = "direct"
	cfg.Harness.Version.State = "unknown"
	cfg.ContextPolicy.State = "disabled"
	cfg.ConfigRefs.State = "known"
	cfg.Budget.MaxRequests.State = "unknown"
	cfg.Budget.MaxSeconds.State = "unknown"
	bref := col.Ref()
	def, e := project.WriteWorkDefinitionInputs(context.Background(), s, project.DefinitionRequest{Label: "selected-mapping-unit", Family: "synthetic-fix", ExactSetBytes: set, PlanID: plan, DeclaredConfiguration: cfg, QualifiedIO: qreq, Baseline: &bref}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	captured, e := project.CaptureWorkDefinitionSource(context.Background(), s, project.CaptureSourceRequest{Label: "selected-mapping-unit", Definition: def}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	source, e := project.LoadCaptureSource(context.Background(), s, *captured.Source())
	if e != nil {
		t.Fatal(e)
	}
	if source.Ref() != *captured.Source() {
		t.Fatal("exact source Ref getter")
	}
	return mappingFixture{s, l, baseline, source, name}
}
func TestSelectedProjectMappingActualProducerCallback(t *testing.T) {
	f := newMappingFixture(t)
	live, file := runner.SelectedMappingTestLive(t)
	r := &runner.SelectedMappingTestRunner{T: t, Nodes: []runner.SelectedMappingTestNode{{Path: ".git/HEAD", Body: "ref: refs/heads/synthetic\n"}, {Path: "extra.go", Body: "package extra\n"}}}
	var mapped *pipeline.SelectedProjectMapping
	seen := 0
	var mapErr error
	_, e := pipeline.RunSuite(context.Background(), f.s, r, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file, PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
		seen++
		before := v.Graph()
		var e error
		mapped, e = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
		mapErr = e
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(before, v.Graph()) {
			t.Fatal("mapper changed/replaced graph")
		}
		return nil
	}}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if seen != 1 || mapped == nil || r.Starts != 1 {
		t.Fatalf("actual mapping callback/start seen=%d mapped=%v starts=%d err=%v", seen, mapped != nil, r.Starts, mapErr)
	}
	extra := map[string]bool{}
	for _, n := range mapped.UnmappedInventory().Entries {
		extra[n.Path] = true
	}
	if !extra[".git/HEAD"] || !extra["extra.go"] {
		t.Fatal("unmapped extras lost", extra)
	}
	id := mapped.Identity()
	if id.Baseline != f.baseline.Binding() || id.Source != f.source.Ref() || id.Task.Attempt != 1 {
		t.Fatal("wrong actual closed binding")
	}
	if _, e = json.Marshal(mapped); e == nil {
		t.Fatal("wire mapping")
	}
	copy := mapped.UnmappedInventory()
	copy.Entries[0].Path = "changed"
	if reflect.DeepEqual(copy, mapped.UnmappedInventory()) {
		t.Fatal("unmapped aliases")
	}
}

func TestSelectedProjectMappingActualMismatchesStopBeforeStart(t *testing.T) {
	for _, kind := range []string{"byte", "mode", "deleted", "type", "absent", "alias", "cancel", "unrelated", "wrong_archive", "task_dir"} {
		t.Run(kind, func(t *testing.T) {
			var edit func(*library.Library, map[string]any)
			if kind == "wrong_archive" {
				edit = func(l *library.Library, rec map[string]any) {
					d, e := l.PutObject(mappingTar(t, map[string][]byte{"unrelated.sh": []byte("cat\n")}))
					if e != nil {
						t.Fatal(e)
					}
					rec["repositories"].([]any)[0].(map[string]any)["base_archive"] = d
				}
			}
			if kind == "unrelated" {
				edit = func(l *library.Library, rec map[string]any) {
					base := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "unrelated\n"})
					result := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
					b, e := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Repo: rec["repositories"].([]any)[0].(map[string]any)["repo"].(string), Base: base, Result: result, Request: "Uppercase stdin.", RequestAt: map[string]any{"source": library.Digest([]byte("synthetic")), "record": "u1"}, Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}}, CaseSeconds: 5, Now: time.Now()})
					if e != nil {
						t.Fatal(e)
					}
					for k := range rec {
						delete(rec, k)
					}
					for k, v := range b.Record {
						rec[k] = v
					}
				}
			}
			var f mappingFixture
			if edit == nil {
				f = newMappingFixture(t)
			} else {
				f = newMappingFixture(t, edit)
			}
			live, file := runner.SelectedMappingTestLive(t)
			r := &runner.SelectedMappingTestRunner{T: t}
			switch kind {
			case "task_dir":
				r.Before = func(task string) {
					mappingWrite(t, filepath.Join(task, "environment/repo/tool.sh"), []byte("changed actual task bytes\n"))
				}
			case "wrong_archive":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "go.mod", Body: "module example\n\ngo 1.26\n"}, {Path: "main.go", Body: "package main\nfunc Value()int{return 1}\n"}, {Path: "tool.sh", Body: "cat\n"}}
			case "byte":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "tool.sh", Body: "changed\n"}}
			case "mode":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "tool.sh", Body: "cat\n", Mode: 0755}}
			case "deleted":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "tool.sh", Deleted: true}}
			case "type":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "tool.sh", Type: tar.TypeDir, Mode: 0755}}
			case "absent":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "missing.txt", Type: tar.TypeDir, Mode: 0755}}
			case "alias":
				r.Nodes = []runner.SelectedMappingTestNode{{Path: "TOOL.SH", Body: "cat\n"}}
			}
			var mapping *pipeline.SelectedProjectMapping
			seen := 0
			_, e := pipeline.RunSuite(context.Background(), f.s, r, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file, PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
				seen++
				if kind == "cancel" {
					c, cancel := context.WithCancel(ctx)
					cancel()
					ctx = c
				}
				var e error
				mapping, e = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
				return e
			}}, time.Now())
			if e != nil {
				t.Fatal("unexpected suite preparation refusal", e)
			}
			if mapping != nil || r.Starts != 0 {
				t.Fatalf("invalid mapping started: kind=%s seen=%d mapping=%v starts=%d", kind, seen, mapping != nil, r.Starts)
			}
			if kind != "alias" && seen != 1 {
				t.Fatalf("negative did not reach actual mapping callback: %s %d", kind, seen)
			}
		})
	}
}
