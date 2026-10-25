package runner

// Synthetic engine only. No Docker/OpenCode/model, store, key or external endpoint.
import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/neprel/git-hint/internal/testkit/referenceengine"
)

type referenceFake = referenceengine.State

func referenceEngine(t *testing.T, mode string) (*Docker, *referenceFake) {
	return referenceEngineListener(t, mode, nil)
}
func referenceEngineListener(t *testing.T, mode string, listener net.Listener) (*Docker, *referenceFake) {
	cli, f := referenceengine.Start(t, mode, listener)
	return &Docker{cli: cli}, f
}
func referenceTaskOptions(t *testing.T) (string, ReferencePrepareOptions) {
	t.Helper()
	dir := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	// Exact runner limits, not model limits. ioTaskDir default fixture uses other settings.
	text, e := os.ReadFile(filepath.Join(dir, "task.toml"))
	if e != nil {
		t.Fatal(e)
	}
	s := string(text)
	s = strings.ReplaceAll(s, "memory_mb = 64", "memory_mb = 2048")
	s = strings.ReplaceAll(s, "cpus = 1", "cpus = 2")
	_ = os.WriteFile(filepath.Join(dir, "task.toml"), []byte(s), 0600)
	iot, e := readIOTask(dir)
	if e != nil {
		t.Fatal(e)
	}
	ref, e := tarDir(filepath.Join(dir, "tests", "io", "reference"), "solution")
	if e != nil {
		t.Fatal(e)
	}
	env, e := tarDir(filepath.Join(dir, "environment"), "")
	if e != nil {
		t.Fatal(e)
	}
	return dir, ReferencePrepareOptions{OutputDir: filepath.Join(t.TempDir(), "new"), Expected: ReferenceInputIdentity{iot.specDigest, iot.inputsDigest, referenceDigest(ref), referenceDigest(env)}, policy: &referencePreparationPolicy{identity: "synthetic-private-test-policy-v1", operation: 30 * time.Second, resolve: time.Second, commit: time.Second, build: time.Second, apply: time.Second, overhead: time.Millisecond, cleanup: 300 * time.Millisecond, archiveCap: 1 << 20, totalCap: 4 << 20}}
}
func TestReferencePrepareDefaultDisabled(t *testing.T) {
	a, e := (&Docker{}).PrepareReference(context.Background(), "not-read", ReferencePrepareOptions{})
	if e == nil || a.State != "unknown" || len(a.FailureCodes) != 1 || a.FailureCodes[0] != "policy_disabled" {
		t.Fatal(a, e)
	}
}
func TestReferencePrepareActualFakePath(t *testing.T) {
	d, f := referenceEngine(t, "")
	task, o := referenceTaskOptions(t)
	a, e := d.PrepareReference(context.Background(), task, o)
	if e != nil {
		t.Fatalf("%+v %v", a, e)
	}
	if a.State != "prepared_reference_observed" || a.FinalTAR.Domain != referenceTARDomain || a.OracleOutputsDigest == "" || len(a.Cases) != 4 {
		t.Fatalf("%+v", a)
	}
	b, e := os.ReadFile(filepath.Join(o.OutputDir, "final.tar"))
	if e != nil || referenceDigest(b) != a.FinalTAR.Digest {
		t.Fatal(e)
	}
	// Golden expected B1 hash independently spells the existing byte recipe.
	if a.OracleOutputsDigest != referenceDigest([]byte("lower\x000\x003\x00OK\nmixed\x000\x003\x00OK\n")) {
		t.Fatalf("output recipe changed: %+v", a)
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Creates != 5 || f.Builds != 1 || len(f.Copied) != 4 || !bytes.Equal(f.Copied[0], f.Copied[1]) || len(f.Removed) != 5 {
		t.Fatalf("creates=%d builds=%d copied=%d removed=%d", f.Creates, f.Builds, len(f.Copied), len(f.Removed))
	}
	for _, p := range a.Phases[1:] {
		if !p.Started || !p.WaitObserved || !p.Removed || !p.Exited || !p.NetworkNone || p.ContainerRef == "" || p.CommandDigest == "" {
			t.Fatalf("incomplete %+v", p)
		}
	}
	for _, c := range f.Cfg {
		if c.Image != fakeImage {
			t.Fatal("image changed")
		}
		if !c.OpenStdin && !strings.Contains(strings.Join(c.Cmd, " "), "/solution/solve.sh") {
			t.Fatal("non reference execution")
		}
	}
}
func TestReferencePrepareMutations(t *testing.T) {
	for _, mode := range []string{"build_malformed", "inspect_fail", "network", "remove_fail", "exit", "logs_cut", "tar_cut", "output_cut", "output_hang", "disagree"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := referenceEngine(t, mode)
			task, o := referenceTaskOptions(t)
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" || len(a.FailureCodes) == 0 {
				t.Fatalf("accepted %+v %v", a, e)
			}
		})
	}
}
func TestReferencePrepareInputAndScheduleBeforeEngine(t *testing.T) {
	for _, mode := range []string{"input", "capacity", "cancelled", "output_exists"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, "")
			task, o := referenceTaskOptions(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "input":
				o.Expected.SpecDigest = "different"
			case "capacity":
				o.policy.operation = time.Second
			case "cancelled":
				cancel()
			case "output_exists":
				_ = os.Mkdir(o.OutputDir, 0700)
			}
			a, e := d.PrepareReference(ctx, task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal(a, e)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.Builds != 0 || f.Creates != 0 {
				t.Fatal("engine used")
			}
		})
	}
}
func TestReferencePrepareBoundedJoin(t *testing.T) {
	p := &referencePhase{joinEnd: time.Now().Add(10 * time.Millisecond)}
	if e := p.join(make(chan error)); e == nil || p.err == nil {
		t.Fatal("unjoined stream passed")
	}
	ch := make(chan error, 1)
	ch <- nil
	p = &referencePhase{joinEnd: time.Now().Add(time.Second)}
	if e := p.join(ch); e != nil {
		t.Fatal(e)
	}
}
func TestReferencePrepareArchiveCapAndHeaderDomain(t *testing.T) {
	d, f := referenceEngine(t, "")
	task, o := referenceTaskOptions(t)
	o.policy.archiveCap = 512
	a, e := d.PrepareReference(context.Background(), task, o)
	if e == nil || a.State != "unknown" {
		t.Fatal(a, e)
	}
	if _, e = os.Stat(filepath.Join(o.OutputDir, "final.tar")); !os.IsNotExist(e) {
		t.Fatal("partial artifact retained")
	}
	var other bytes.Buffer
	tw := tar.NewWriter(&other)
	_ = tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(456, 0)})
	_ = tw.WriteHeader(&tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644, Size: 1, ModTime: time.Unix(123, 0)})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	x := filepath.Join(t.TempDir(), "x")
	y := filepath.Join(t.TempDir(), "y")
	dx, e := freezeTree(bytes.NewReader(f.Archive), x)
	if e != nil {
		t.Fatal(e)
	}
	dy, e := freezeTree(bytes.NewReader(other.Bytes()), y)
	if e != nil || dx == dy {
		t.Fatal("metadata erased", e)
	}
}

func TestReferencePrepareLateRPCsCannotBecomeEvidence(t *testing.T) {
	for _, mode := range []string{"wait_late", "inspect_late", "remove_late", "stop_late"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, mode)
			task, o := referenceTaskOptions(t)
			o.policy.build = 30 * time.Millisecond
			o.policy.apply = 30 * time.Millisecond
			o.policy.cleanup = 60 * time.Millisecond
			start := time.Now()
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal("late execution accepted", a, e)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("cleanup renewed budget")
			}
			if mode == "inspect_late" {
				f.Mu.Lock()
				defer f.Mu.Unlock()
				if f.Creates != 1 || len(f.Removed) != 1 {
					t.Fatal("lost created identity", f.Creates, len(f.Removed))
				}
			}
		})
	}
}
func TestReferencePrepareJoinRejectsLateReadyWorker(t *testing.T) {
	ch := make(chan error, 1)
	ch <- nil
	p := &referencePhase{joinEnd: time.Now().Add(-time.Millisecond)}
	if p.join(ch) == nil || p.err == nil {
		t.Fatal("late worker became evidence")
	}
}

func TestReferencePrepareClosedChannelIsMissingEvidence(t *testing.T) {
	ch := make(chan error)
	close(ch)
	p := &referencePhase{joinEnd: time.Now().Add(time.Second)}
	if p.join(ch) == nil || p.err == nil {
		t.Fatal("missing worker result accepted")
	}
}

func TestReferencePrepareSourceAndRetainedArtifactCannotChange(t *testing.T) {
	for _, mode := range []string{"reference", "retained_tar"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, "")
			task, o := referenceTaskOptions(t)
			f.BeforeCreate = func(isCase bool) {
				if mode == "reference" && !isCase {
					if e := os.WriteFile(filepath.Join(task, "tests", "io", "reference", "solve.sh"), []byte("changed\n"), 0755); e != nil {
						t.Error(e)
					}
				}
				if mode == "retained_tar" && isCase {
					p := filepath.Join(o.OutputDir, "final.tar")
					file, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
					if e != nil {
						t.Error(e)
						return
					}
					_, _ = file.Write(make([]byte, 512))
					_ = file.Close()
				}
			}
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal("source substitution accepted", a, e)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.CaseStarts != 0 {
				t.Fatal("changed artifact executed", f.CaseStarts)
			}
		})
	}
}

func TestReferencePrepareCancellationRetainsCleanupBudget(t *testing.T) {
	d, f := referenceEngine(t, "cancel_wait")
	task, o := referenceTaskOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.Waiting:
			cancel()
		case <-ctx.Done():
		}
	}()
	a, e := d.PrepareReference(ctx, task, o)
	if e == nil || a.State != "unknown" {
		t.Fatal("cancelled work became success", a, e)
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Stops != 1 || len(f.Removed) != 1 {
		t.Fatal("cancelled work lost original cleanup", f.Stops, len(f.Removed))
	}
}

func TestReferencePrepareOrdinaryIOPhaseGolden(t *testing.T) {
	d, _ := referenceEngine(t, "")
	task := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	iot, e := readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	if e = os.Mkdir(filepath.Join(dir, "verifier"), 0700); e != nil {
		t.Fatal(e)
	}
	reward, prov, e := d.ioPhase(context.Background(), task, iot, taskLimits{agent: time.Second, verifier: 20 * time.Second, cpus: 2, memoryMB: 2048}, fakeImage, fakeTree(t), dir)
	if e != nil || reward != 1 {
		t.Fatal(reward, e)
	}
	golden := "TEST B1 cases/lower::output PASS\nTEST B1 cases/mixed::output PASS\nCHECK B1 PASS\n"
	b, e := os.ReadFile(filepath.Join(dir, "verifier", "test-stdout.txt"))
	if e != nil || string(b) != golden {
		t.Fatalf("ordinary output changed %q %v", b, e)
	}
	expected := map[string]any{"interface": IOInterface, "spec": iot.specDigest, "inputs": iot.inputsDigest, "oracle_runs": OracleRuns, "verdict_scope": "equivalence_to_reference_on_declared_cases", "reference_tree": "sha256:897cf7e72a6771a5d5650ff4f770128f8fba6cd8210e889319f0bca073a755c6", "oracle_outputs": referenceDigest([]byte("lower\x000\x003\x00OK\nmixed\x000\x003\x00OK\n")), "candidate_outputs": referenceDigest([]byte("lower\x000\x00false\x00false\x003\x00OK\nmixed\x000\x00false\x00false\x003\x00OK\n")), "network": "engine_inspect:blocked"}
	if referenceJSON(prov) != referenceJSON(expected) {
		t.Fatalf("ordinary provenance changed %+v", prov)
	}
}

// Inverts publication's original unsafe RP-LC-1 assertions using the same actual
// create/runToEnd/join/remove helpers and original fixed endpoints.
func TestReferencePrepareCleanupPrecedesUnjoinedWorker(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "blocked", false: "ready"}[blocked], func(t *testing.T) {
			d, f := referenceEngine(t, "wait_late")
			anchor := time.Now()
			parent, cancel := context.WithDeadline(context.Background(), anchor.Add(time.Second))
			defer cancel()
			p := &referencePhase{parent: parent, anchor: anchor, workEnd: anchor.Add(100 * time.Millisecond), stopEnd: anchor.Add(200 * time.Millisecond), removeEnd: anchor.Add(300 * time.Millisecond), joinEnd: anchor.Add(500 * time.Millisecond)}
			work, done := context.WithDeadline(context.Background(), p.workEnd)
			defer done()
			work = context.WithValue(work, referencePhaseKey{}, p)
			cid, e := d.create(work, &container.Config{Image: fakeImage, Cmd: []string{"sh", "-c", "synthetic"}}, d.hostConfig(taskLimits{cpus: 2, memoryMB: 2048}))
			if e != nil {
				t.Fatal(e)
			}
			if e = d.runToEnd(work, cid, time.Second, "synthetic"); e == nil {
				t.Fatal("wait did not time out")
			}
			worker := make(chan error, 1)
			if !blocked {
				worker <- nil
			}
			if e = p.join(worker); e == nil {
				t.Fatal("expired work or missing worker became success")
			}
			// Exact outer finalizer call is idempotent, not a second cleanup request/window.
			p.ensureCleanup()
			p.cleaning = true
			d.removeForPhase(work, cid)
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.Stops != 1 || len(f.Removed) != 1 || f.Running[cid] || !p.audit.Removed {
				t.Fatalf("missed original cleanup slots stops=%d removed=%d running=%v audit=%+v", f.Stops, len(f.Removed), f.Running[cid], p.audit)
			}
			if blocked && p.err == nil {
				t.Fatal("unjoined worker lacks unknown state")
			}
		})
	}
}
func TestReferencePrepareLegacyTimeoutDistinction(t *testing.T) {
	task := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	parsed, e := readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	_, _, _, e = referenceExpectations(parsed, func(string, int) (caseRun, error) { return caseRun{}, timeoutError{"case"} })
	var verdict errIOVerdict
	if !errors.As(e, &verdict) {
		t.Fatalf("per-case timeout changed legacy classification: %T %v", e, e)
	}
	if e.Error() != "no B1 verdict: reference run of case lower failed" {
		t.Fatal(e)
	}
	_, _, _, e = referenceExpectations(parsed, func(string, int) (caseRun, error) {
		return caseRun{}, referenceOverallDeadline{timeoutError{"verifier"}}
	})
	var deadline timeoutError
	if !errors.As(e, &deadline) || errors.As(e, &verdict) || deadline.phase != "verifier" {
		t.Fatalf("outer deadline lost legacy classification: %T %v", e, e)
	}
}
