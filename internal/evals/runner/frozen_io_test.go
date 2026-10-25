package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestFrozenIOSourceIdentityGuards(t *testing.T) {
	_, sources, _, e := referenceBundleIdentity()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"bounded_phase.go", "frozen_io.go"} {
		for _, mutation := range []string{"bytes", "omission"} {
			fixture := fstest.MapFS{}
			for _, source := range sources {
				raw, e := referencePolicySources.ReadFile(source.Path)
				if e != nil {
					t.Fatal(e)
				}
				fixture[source.Path] = &fstest.MapFile{Data: bytes.Clone(raw)}
			}
			if _, ok := fixture[name]; !ok {
				t.Fatal("new dependency absent", name)
			}
			if mutation == "bytes" {
				fixture[name].Data = append(fixture[name].Data, '\n')
			} else {
				delete(fixture, name)
			}
			if e = referencePolicySourceSet(fixture, sources); e == nil {
				t.Fatal("source drift/omission accepted", name, mutation)
			}
		}
	}
}

func frozenFixture(t *testing.T) (*ioTask, taskLimits, frozenIORequest, *frozenVerificationPolicy) {
	t.Helper()
	task := ioTaskDir(t, "cat\n", "cat\n", 1, 300, nil)
	iot, e := readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	spec := iot.spec
	spec.Cases = []string{"lower"}
	raw, e := json.Marshal(spec)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(task, "tests", "io", "spec.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(task, "tests", "io", "inputs", "mixed.in")); e != nil {
		t.Fatal(e)
	}
	iot, e = readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	artifact := func() frozenIOArtifact {
		p := fakeTree(t)
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		return frozenIOArtifact{p, referenceDigest(b), fakeImage}
	}
	req := frozenIORequest{artifact(), artifact(), t.TempDir()}
	// Distinct actual bytes prove that the shared core never substitutes the
	// reference artifact for the actual artifact, even when finite outputs agree.
	req.candidate.path = filepath.Join(t.TempDir(), "actual.tar")
	digest, e := freezeTree(bytes.NewReader(completeTestTree(t, tar.FormatUSTAR, "actual.go")), req.candidate.path)
	if e != nil {
		t.Fatal(e)
	}
	req.candidate.digest = digest
	if e = os.Mkdir(filepath.Join(req.output, "verifier"), 0700); e != nil {
		t.Fatal(e)
	}
	lim, e := readLimits(task)
	if e != nil {
		t.Fatal(e)
	}
	return iot, lim, req, &frozenVerificationPolicy{frozenVerificationProcess, fakeImage}
}

func TestFrozenIOActualSharedCoreAndLifecycle(t *testing.T) {
	for _, mode := range []string{"", "disagree", "equal-artifacts"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, mode)
			iot, lim, req, p := frozenFixture(t)
			if mode == "equal-artifacts" {
				req.candidate = req.reference
			}
			reward, prov, a, e := d.verifyFrozenIO(context.Background(), iot, lim, req, p)
			wantState, wantReward, wantCheck := "finite_equivalence_observed", 1.0, "PASS"
			if mode == "disagree" {
				wantState, wantReward, wantCheck = "finite_difference_observed", 0.0, "FAIL"
			}
			if e != nil || reward != wantReward || a.State != wantState || a.Authority != "unknown" || len(a.Phases) != 3 {
				t.Fatal(reward, prov, a, e)
			}
			b, e := os.ReadFile(filepath.Join(req.output, "verifier", "test-stdout.txt"))
			if e != nil || string(b) != "TEST B1 cases/lower::output "+wantCheck+"\nCHECK B1 "+wantCheck+"\n" {
				t.Fatal(string(b), e)
			}
			if prov["oracle_outputs"] != referenceDigest([]byte("lower\x000\x003\x00OK\n")) {
				t.Fatal("oracle golden changed", prov)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.Creates != 3 || f.Builds != 0 || len(f.Copied) != 3 || len(f.Removed) != 3 {
				t.Fatal("wrong physical population", f.Creates, f.Builds, len(f.Copied), len(f.Removed))
			}
			if !bytes.Equal(f.Copied[0], f.Copied[1]) || (bytes.Equal(f.Copied[0], f.Copied[2]) != (mode == "equal-artifacts")) {
				t.Fatal("actual/reference artifact substitution")
			}
			for _, phase := range a.Phases {
				if !phase.Started || !phase.WaitObserved || !phase.Exited || !phase.Removed || !phase.StreamsJoined || !phase.NetworkNone || phase.ImageID != fakeImage || phase.JoinEndMS > int64(lim.verifier/time.Millisecond) {
					t.Fatal("incomplete phase", phase)
				}
			}
		})
	}
}

func TestFrozenIORefusesBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"nil-policy", "unissued", "wrong-image", "capacity", "caller-end", "cancel", "nil-context"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, "")
			iot, lim, req, p := frozenFixture(t)
			ctx := context.Background()
			switch mode {
			case "nil-policy":
				p = nil
				req.reference.path = "not-read"
			case "unissued":
				p = &frozenVerificationPolicy{image: fakeImage}
				req.reference.path = "not-read"
			case "wrong-image":
				req.candidate.image = "sha256:" + strings.Repeat("2", 64)
			case "capacity":
				lim.verifier = time.Second
			case "caller-end":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-context":
				ctx = nil
			}
			_, _, a, e := d.verifyFrozenIO(ctx, iot, lim, req, p)
			if e == nil || a.State != "unknown" {
				t.Fatal(a, e)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.Creates != 0 {
				t.Fatal("refusal dispatched", f.Creates)
			}
		})
	}
}

func TestFrozenIOActualFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"network", "remove_fail", "candidate-output-cut", "candidate-timeout", "byte-drift", "candidate-byte-drift", "cancel_wait"} {
		t.Run(mode, func(t *testing.T) {
			fakeMode := mode
			if mode == "byte-drift" || strings.HasPrefix(mode, "candidate-") {
				fakeMode = ""
			}
			d, f := referenceEngine(t, fakeMode)
			iot, lim, req, p := frozenFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "byte-drift" {
				f.BeforeCreate = func(bool) {
					if e := os.WriteFile(req.reference.path, []byte("changed"), 0600); e != nil {
						t.Error(e)
					}
				}
			}
			if strings.HasPrefix(mode, "candidate-") {
				f.BeforeCreate = func(bool) {
					f.Mu.Lock()
					defer f.Mu.Unlock()
					if f.Creates != 2 {
						return
					}
					switch mode {
					case "candidate-output-cut":
						f.Mode = "output_cut"
					case "candidate-timeout":
						f.Mode = "wait_late"
					case "candidate-byte-drift":
						if e := os.WriteFile(req.candidate.path, []byte("changed"), 0600); e != nil {
							t.Error(e)
						}
					}
				}
			}
			if mode == "cancel_wait" {
				go func() {
					select {
					case <-f.Waiting:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			start := time.Now()
			_, _, a, e := d.verifyFrozenIO(ctx, iot, lim, req, p)
			if e == nil || a.State != "unknown" || time.Since(start) > 5*time.Second {
				t.Fatal(a, e, time.Since(start))
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if mode == "remove_fail" {
				if len(f.Removed) != 0 || f.Creates != 1 {
					t.Fatal("remove failure fixture did not refuse", f.Creates, len(f.Removed))
				}
			} else if len(f.Removed) != f.Creates {
				t.Fatal("cleanup omitted", f.Creates, len(f.Removed))
			}
		})
	}
}

func TestFrozenIOUnusableCandidateIsUnknownButLegacyFails(t *testing.T) {
	for _, kind := range []string{"timeout", "truncated", "input", "network"} {
		t.Run(kind, func(t *testing.T) {
			iot, _, _, _ := frozenFixture(t)
			for _, strict := range []bool{false, true} {
				dir := t.TempDir()
				if e := os.Mkdir(filepath.Join(dir, "verifier"), 0700); e != nil {
					t.Fatal(e)
				}
				reward, _, e := compareFrozenIO(context.Background(), iot, dir, map[string]any{}, strict, func(oracle bool, _ string, _ int) (caseRun, error) {
					r := caseRun{out: []byte("same"), networkNone: true}
					if !oracle {
						switch kind {
						case "timeout":
							r.timedOut = true
						case "truncated":
							r.truncated = true
						case "input":
							r.inputFailed = true
						case "network":
							r.networkNone = false
						}
					}
					return r, nil
				})
				if strict {
					if e == nil {
						t.Fatal("strict accepted unusable candidate", reward)
					}
				} else if e != nil || (kind != "network" && reward != 0) {
					t.Fatal("legacy classification changed", reward, e)
				}
			}
		})
	}
}

func TestFrozenIOCandidateTerminalWaitInspectBinding(t *testing.T) {
	for _, mode := range []string{"exit", "wait_exit", "inspect_exit", "running_after_wait", "reference_exit"} {
		t.Run(mode, func(t *testing.T) {
			fakeMode := ""
			if mode == "reference_exit" {
				fakeMode = "exit"
			}
			d, f := referenceEngine(t, fakeMode)
			iot, lim, req, p := frozenFixture(t)
			if mode != "reference_exit" {
				f.BeforeCreate = func(bool) {
					f.Mu.Lock()
					defer f.Mu.Unlock()
					if f.Creates == 2 {
						f.Mode = mode
					}
				}
			}
			reward, prov, a, e := d.verifyFrozenIO(context.Background(), iot, lim, req, p)
			if mode == "exit" {
				if e != nil || reward != 0 || a.State != "finite_difference_observed" || len(a.Phases) != 3 || a.Phases[2].ExitStatus != 3 {
					t.Fatal("complete candidate exit lost", reward, prov, a, e)
				}
			} else if e == nil || a.State != "unknown" {
				t.Fatal("inconsistent or reference exit admitted", reward, prov, a, e)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if len(f.Removed) != f.Creates {
				t.Fatal("terminal path lost cleanup", f.Creates, len(f.Removed))
			}
		})
	}
}
