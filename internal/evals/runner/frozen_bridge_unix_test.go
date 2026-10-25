//go:build unix

package runner

// Owned temporary library/keys and synthetic Unix SDK only. No user store,
// external endpoint, real engine, agent, model or report writer participates.
import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func bridgeInputs(t *testing.T, seconds int) (*Task, *FrozenIOInputs) {
	t.Helper()
	_, l, rec := boundedExportFixture(t)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	files := map[string]string{"io/spec.json": `{"interface":"cli-file-v1","entry":["sh","tool.sh"],"cases":["lower"],"case_seconds":1,"compare":"exact"}`, "io/inputs/lower.in": "hello\n"}
	for _, name := range []string{"io/spec.json", "io/inputs/lower.in"} {
		data := []byte(files[name])
		if e := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	digest, e := l.PutObject(b.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	rec["verifier"] = map[string]any{"kind": IOInterface, "archive": digest, "checks": []any{map[string]any{"id": "B1", "tests": []any{"cases/lower::output"}}}}
	settings := DefaultSettings
	settings.VerifierSeconds = seconds
	task, e := ExportBounded(context.Background(), l, rec, filepath.Join(t.TempDir(), "task"), settings)
	if e != nil {
		t.Fatal(e)
	}
	inputs, e := task.FrozenIOInputs()
	if e != nil {
		t.Fatal(e)
	}
	legacy, e := Export(l, rec, filepath.Join(t.TempDir(), "legacy"), settings)
	if e != nil {
		t.Fatal(e)
	}
	x, e := DirDigest(task.Dir)
	if e != nil {
		t.Fatal(e)
	}
	y, e := DirDigest(legacy.Dir)
	if e != nil || x != y {
		t.Fatal("B1 export bytes changed", e, x, y)
	}
	if _, e = legacy.FrozenIOInputs(); e == nil {
		t.Fatal("legacy export minted snapshot")
	}
	return task, inputs
}
func bridgeArtifact(t *testing.T, actual bool) *FrozenIOArtifact {
	t.Helper()
	p := fakeTree(t)
	if actual {
		p = filepath.Join(t.TempDir(), "actual.tar")
		if _, e := freezeTree(bytes.NewReader(completeTestTree(t, tar.FormatUSTAR, "actual.go")), p); e != nil {
			t.Fatal(e)
		}
	}
	data, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	h, e := ValidateFrozenIOArtifact(context.Background(), FrozenIOArtifactIdentity{referenceTARDomain, referenceDigest(data), int64(len(data))}, data)
	if e != nil {
		t.Fatal(e)
	}
	for i := range data {
		data[i] = 0
	}
	if !h.valid() {
		t.Fatal("artifact retained caller slice")
	}
	return h
}
func bridgeTranscript(t *testing.T, inputs *FrozenIOInputs, text string) *FrozenIOReferenceTranscript {
	t.Helper()
	cases := []IOTranscriptCase{{"lower", 0, []byte(text)}}
	digest, e := IOTranscriptDigest(cases)
	if e != nil {
		t.Fatal(e)
	}
	h, e := ValidateFrozenIOReferenceTranscript(context.Background(), inputs, cases, digest)
	if e != nil {
		t.Fatal(e)
	}
	cases[0].Output[0] = 'X'
	if h.digest != digest || string(h.cases[0].Output) != text {
		t.Fatal("transcript retained caller bytes")
	}
	return h
}
func bridgePolicy(t *testing.T, mode string) (FrozenVerificationPolicyRef, *referenceFake, *referenceCountListener) {
	t.Helper()
	entry, f, count := policyUnixFixture(t, mode)
	entry.Purpose = frozenPurpose
	entry.Image = fakeImage
	entry.Procedure = referencePreparationPolicy{identity: entry.ID, operation: 300 * time.Second, resolve: 30 * time.Second, commit: 30 * time.Second, overhead: 10 * time.Second, cleanup: 30 * time.Second, archiveCap: 96 << 20, totalCap: 256 << 20}
	ref := installReferenceTestEntry(t, entry)
	return FrozenVerificationPolicyRef{ref.PolicyID, ref.PolicyDigest}, f, count
}
func TestFrozenBridgeActualBoundedSnapshot(t *testing.T) {
	task, h := bridgeInputs(t, 300)
	before := h.Identity()
	if e := os.WriteFile(filepath.Join(task.Dir, "tests", "io", "inputs", "lower.in"), []byte("changed\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(task.Dir, "task.toml"), []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	task.Inputs["verifier"] = "changed"
	task.Dir = "changed"
	copyID := h.Identity()
	copyID.SourceInputs["reference"] = "changed"
	copyID.Cases[0] = "changed"
	if string(h.task.inputs["lower"]) != "hello\n" {
		t.Fatal("snapshot reread changed generated files")
	}
	if !reflect.DeepEqual(before, h.Identity()) {
		t.Fatal("snapshot mutable")
	}
	next, e := task.FrozenIOInputs()
	if e != nil || !reflect.DeepEqual(before, next.Identity()) {
		t.Fatal("Task public fields rewrote snapshot", e)
	}
	for _, task := range []*Task{nil, {}, {Dir: "forged", Inputs: map[string]string{"verifier": "fake"}}} {
		if _, e := task.FrozenIOInputs(); e == nil {
			t.Fatal("forged Task minted snapshot")
		}
	}
}
func TestFrozenBridgePurposePathAndDrift(t *testing.T) {
	for _, mode := range []string{"", "difference", "drift", "exit", "remove_fail", "candidate-output-cut", "candidate-timeout", "cancel_wait"} {
		t.Run(mode, func(t *testing.T) {
			fakeMode := ""
			if mode == "difference" {
				fakeMode = "disagree"
			}
			if mode == "remove_fail" || mode == "cancel_wait" {
				fakeMode = mode
			}
			policy, f, _ := bridgePolicy(t, fakeMode)
			_, inputs := bridgeInputs(t, 300)
			expected := "OK\n"
			if mode == "drift" {
				expected = "old\n"
			}
			req := FrozenVerificationRequest{policy, inputs, bridgeArtifact(t, false), bridgeArtifact(t, true), bridgeTranscript(t, inputs, expected)}
			if mode == "exit" || mode == "candidate-output-cut" || mode == "candidate-timeout" {
				f.BeforeCreate = func(bool) {
					f.Mu.Lock()
					defer f.Mu.Unlock()
					if f.Creates == 2 {
						switch mode {
						case "exit":
							f.Mode = "exit"
						case "candidate-output-cut":
							f.Mode = "output_cut"
						case "candidate-timeout":
							f.Mode = "wait_late"
						}
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
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
			h, e := VerifyFrozenIOForPolicy(ctx, req)
			got := h.Identity()
			if mode == "" || mode == "difference" || mode == "exit" {
				want := "finite_equivalence_observed"
				if mode != "" {
					want = "finite_difference_observed"
				}
				if e != nil || got.State != want || got.Authority != "unknown" || len(got.Cases) != 3 {
					t.Fatal(got, e)
				}
			} else if e == nil || got.State != "unknown" {
				t.Fatal("failed observation admitted", got, e)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatal("lost finite cleanup", time.Since(start))
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if mode == "drift" && f.Creates != 2 {
				t.Fatal("candidate ran after reference drift", f.Creates)
			}
			if f.Builds != 0 {
				t.Fatal("verification rebuilt image")
			}
			if mode != "remove_fail" && len(f.Removed) != f.Creates {
				t.Fatal("cleanup omitted", len(f.Removed), f.Creates)
			}
			if mode == "" {
				data, ok := h.Output(0)
				if !ok || string(data) != "OK\n" {
					t.Fatal("original output missing")
				}
				data[0] = 'X'
				again, _ := h.Output(0)
				if string(again) != "OK\n" {
					t.Fatal("output mutable")
				}
				got.Cases[0].Status = 99
				if h.Identity().Cases[0].Status == 99 {
					t.Fatal("audit mutable")
				}
			}
		})
	}
}
func TestFrozenBridgeDefaultCrossPurposeAndCapacity(t *testing.T) {
	if len(referencePreparationPolicies) != 0 {
		t.Fatal("production catalogue not empty")
	}
	if h, e := VerifyFrozenIOForPolicy(context.Background(), FrozenVerificationRequest{Policy: FrozenVerificationPolicyRef{"not-installed", fakeImage}}); e == nil || h.Identity().State != "unknown" {
		t.Fatal(h, e)
	}
	t.Run("preparation-only", func(t *testing.T) {
		entry, _, count := policyUnixFixture(t, "")
		ref := installReferenceTestEntry(t, entry)
		before := count.accepted.Load()
		h, e := VerifyFrozenIOForPolicy(context.Background(), FrozenVerificationRequest{Policy: FrozenVerificationPolicyRef{ref.PolicyID, ref.PolicyDigest}})
		if e == nil || h.Identity().State != "unknown" || count.accepted.Load() != before {
			t.Fatal("cross purpose reached engine", e)
		}
	})
	for _, mode := range []string{"declared-limit", "caller-limit", "nil-inputs", "zero-artifact", "wrong-transcript"} {
		t.Run(mode, func(t *testing.T) {
			policy, f, count := bridgePolicy(t, "")
			seconds := 300
			if mode == "declared-limit" {
				seconds = 1
			}
			_, inputs := bridgeInputs(t, seconds)
			req := FrozenVerificationRequest{policy, inputs, bridgeArtifact(t, false), bridgeArtifact(t, false), bridgeTranscript(t, inputs, "OK\n")}
			ctx := context.Background()
			switch mode {
			case "caller-limit":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			case "nil-inputs":
				req.Inputs = nil
			case "zero-artifact":
				req.Candidate = &FrozenIOArtifact{}
			case "wrong-transcript":
				req.ExpectedReference = &FrozenIOReferenceTranscript{}
			}
			before := count.accepted.Load()
			h, e := VerifyFrozenIOForPolicy(ctx, req)
			if e == nil || h.Identity().State != "unknown" || count.accepted.Load() != before {
				t.Fatal("refusal reached engine", e)
			}
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if f.Creates != 0 {
				t.Fatal("invalid input launched", f.Creates)
			}
		})
	}
}

func TestFrozenBridgeFactoriesAndSnapshotBindings(t *testing.T) {
	_, inputs := bridgeInputs(t, 300)
	good := bridgeArtifact(t, false)
	data := bytes.Clone(good.data)
	id := good.Identity()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, mode := range []string{"nil-context", "cancelled", "hash", "size", "domain", "oversize", "bad-tar"} {
		t.Run(mode, func(t *testing.T) {
			expected := id
			raw := data
			ctx := context.Background()
			switch mode {
			case "nil-context":
				ctx = nil
			case "cancelled":
				ctx = cancelled
			case "hash":
				expected.Digest = fakeImage
			case "size":
				expected.Bytes++
			case "domain":
				expected.Domain = "hint-tree-manifest"
			case "oversize":
				expected.Bytes = (96 << 20) + 1
			case "bad-tar":
				raw = []byte("not a tar")
				expected.Bytes = int64(len(raw))
				expected.Digest = referenceDigest(raw)
			}
			if h, e := ValidateFrozenIOArtifact(ctx, expected, raw); e == nil || h != nil {
				t.Fatal("invalid artifact accepted", h, e)
			}
		})
	}
	for _, mode := range []string{"nil-context", "cancelled", "missing", "extra", "wrong-id", "nonzero", "oversize", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			cases := []IOTranscriptCase{{"lower", 0, []byte("OK\n")}}
			digest, e := IOTranscriptDigest(cases)
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			switch mode {
			case "nil-context":
				ctx = nil
			case "cancelled":
				ctx = cancelled
			case "missing":
				cases = nil
			case "extra":
				cases = append(cases, cases[0])
			case "wrong-id":
				cases[0].ID = "other"
			case "nonzero":
				cases[0].Status = 3
			case "oversize":
				cases[0].Output = make([]byte, maxIOBytes+1)
			case "aggregate":
				digest = fakeImage
			}
			if h, e := ValidateFrozenIOReferenceTranscript(ctx, inputs, cases, digest); e == nil || h != nil {
				t.Fatal("invalid expected transcript accepted", h, e)
			}
		})
	}
	policy, _, count := bridgePolicy(t, "")
	_, other := bridgeInputs(t, 600)
	req := FrozenVerificationRequest{policy, inputs, good, good, bridgeTranscript(t, other, "OK\n")}
	before := count.accepted.Load()
	h, e := VerifyFrozenIOForPolicy(context.Background(), req)
	if e == nil || h.Identity().State != "unknown" || count.accepted.Load() != before {
		t.Fatal("foreign declaration transcript admitted", e)
	}
	if _, e := ResolveReferencePreparationPolicy(ReferencePreparationPolicyRef{policy.PolicyID, policy.PolicyDigest}); e == nil {
		t.Fatal("verification policy authorized preparation")
	}
}
