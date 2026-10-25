package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrelaunchRetainedBytesInventoryAndLifetime(t *testing.T) {
	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	for _, h := range []*tar.Header{{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "app/x", Typeflag: tar.TypeReg, Mode: 0751, Size: 3}, {Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "x"}} {
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			if _, e := tw.Write([]byte("abc")); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	raw := body.Bytes()
	root := t.TempDir()
	p := filepath.Join(root, "input.tar")
	os.WriteFile(p, raw, 0600)
	h := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(h[:])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, e := retainPrelaunch(ctx, p, PrelaunchTreeCapture{State: "complete", Digest: digest})
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(p)
	os.WriteFile(p, []byte("replacement"), 0600)
	dest := t.TempDir()
	inv, e := a.Materialize(context.Background(), dest)
	if e != nil {
		t.Fatal(e)
	}
	if len(inv.Entries) != 3 || inv.Digest != digest || len(inv.Files()) != 1 {
		t.Fatal(inv)
	}
	for _, e := range inv.Entries {
		if e.Path == "x" && (e.Mode != 0751 || e.LogicalMode != 0100755 || e.Size != 3 || e.Digest != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad") {
			t.Fatal(e)
		}
		if e.Path == "link" && (!e.Unresolved || e.LinkTarget != "x") {
			t.Fatal(e)
		}
	}
	if data, e := os.ReadFile(filepath.Join(dest, "x")); e != nil || string(data) != "abc" {
		t.Fatal("retained payload changed", e)
	}
	child, stop := context.WithCancel(context.Background())
	stop()
	if _, e := a.Materialize(child, t.TempDir()); e == nil {
		t.Fatal("ignored caller cancellation")
	}
	if _, e := a.Materialize(context.Background(), t.TempDir()); e != nil {
		t.Fatal("caller cancellation poisoned original context", e)
	}
	if _, e := json.Marshal(a); e == nil {
		t.Fatal("serialized capability")
	}
	inv.Entries[0].Path = "mutated"
	if e := a.close(); e != nil {
		t.Fatal(e)
	}
	if _, e := a.Materialize(context.Background(), t.TempDir()); e == nil {
		t.Fatal("used closed capability")
	}
	a, e = retainPrelaunch(ctx, filepath.Join(root, "absent"), PrelaunchTreeCapture{State: "complete", Digest: digest})
	if e == nil {
		t.Fatal("absent accepted")
	}
	os.WriteFile(p, raw, 0600)
	a, e = retainPrelaunch(ctx, p, PrelaunchTreeCapture{State: "complete", Digest: digest})
	if e != nil {
		t.Fatal(e)
	}
	defer a.close()
	cancel()
	if _, e := a.Materialize(context.Background(), t.TempDir()); e == nil {
		t.Fatal("caller renewed original context")
	}
}

func TestPrelaunchCallbackPrecedesFinalHook(t *testing.T) {
	checked := false
	hooks := 0
	_, c, s := studyFake(t, studyFakeOptions{prelaunchCheck: func(ctx context.Context, a *PrelaunchArtifact) error {
		if checked {
			t.Fatal("repeated callback")
		}
		checked = true
		if a.Metadata().Capture.State != "complete" {
			t.Fatal("incomplete")
		}
		_, e := a.Materialize(ctx, t.TempDir())
		return e
	}, hook: func(LaunchFacts) error {
		hooks++
		if !checked {
			t.Fatal("hook before check")
		}
		return nil
	}})
	s.mu.Lock()
	defer s.mu.Unlock()
	if !checked || hooks != 1 || s.starts != 1 {
		t.Fatalf("checked=%v hooks=%d starts=%d", checked, hooks, s.starts)
	}
	if c.Report().AgentRun == nil {
		t.Fatal("missing actual enclosure")
	}
}
func TestPrelaunchCallbackFailureNeverCallsHookOrStarts(t *testing.T) {
	calls := 0
	_, c, s := studyFake(t, studyFakeOptions{prelaunchCheck: func(context.Context, *PrelaunchArtifact) error { return errors.New("synthetic refusal") }, hook: func(LaunchFacts) error { calls++; return nil }})
	s.mu.Lock()
	defer s.mu.Unlock()
	if calls != 0 || s.starts != 0 || c.Requests() != 0 {
		t.Fatal(calls, s.starts, c.Requests())
	}
}
func TestPrelaunchCallbackRequiresCapture(t *testing.T) {
	if (RunOptions{PrelaunchCheck: func(context.Context, *PrelaunchArtifact) error { return nil }}).checkStudyLaunch() == nil {
		t.Fatal("missing capture accepted")
	}
}

func TestPrelaunchRetentionRejectsUnsafeAndChangedBytes(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	os.WriteFile(file, []byte("changed"), 0600)
	expected := "sha256:" + hex.EncodeToString(make([]byte, 32))
	for _, name := range []string{file, root, filepath.Join(root, "link")} {
		if name != file && name != root {
			if e := os.Symlink(file, name); e != nil {
				t.Fatal(e)
			}
		}
		if a, e := retainPrelaunch(context.Background(), name, PrelaunchTreeCapture{State: "complete", Digest: expected}); e == nil {
			a.close()
			t.Fatal("unsafe or changed source accepted")
		}
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range entries {
		if v.IsDir() {
			t.Fatal("failed retention leaked scratch")
		}
	}
}
func TestPrelaunchConsumerPanicStillClosesRetainedCapability(t *testing.T) {
	raw := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}).Bytes()
	p := filepath.Join(t.TempDir(), "input")
	os.WriteFile(p, raw, 0600)
	h := sha256.Sum256(raw)
	a, e := retainPrelaunch(context.Background(), p, PrelaunchTreeCapture{State: "complete", Digest: "sha256:" + hex.EncodeToString(h[:])})
	if e != nil {
		t.Fatal(e)
	}
	err := consumePrelaunch(context.Background(), a, func(context.Context, *PrelaunchArtifact) error { panic("PRIVATE PANIC SENTINEL") })
	if err == nil || err.Error() != "prelaunch callback panicked" {
		t.Fatal("missing static panic refusal", err)
	}

	if _, e := a.Materialize(context.Background(), t.TempDir()); e == nil {
		t.Fatal("panic retained capability")
	}
}

func TestPrelaunchCallbackPanicRoutesThroughOwnedLifecycle(t *testing.T) {
	hooks := 0
	r, c, s := studyFake(t, studyFakeOptions{prelaunchCheck: func(context.Context, *PrelaunchArtifact) error { panic("PRIVATE PANIC SENTINEL") }, hook: func(LaunchFacts) error { hooks++; return nil }})
	s.mu.Lock()
	starts := s.starts
	s.mu.Unlock()
	s.engine.mu.Lock()
	containers, networks := len(s.engine.containers), len(s.engine.networks)
	s.engine.mu.Unlock()
	if hooks != 0 || starts != 0 || c.Requests() != 0 || containers != 0 || networks != 0 {
		t.Fatalf("hooks=%d starts=%d requests=%d containers=%d networks=%d", hooks, starts, c.Requests(), containers, networks)
	}
	report := c.Report()
	if !c.sealStarted() || c.SealedAt() == "" || report.AgentRun == nil || report.AgentRun.Outcome != AgentRefusedBeforeLaunch {
		t.Fatal("counter not sealed/refused", report)
	}
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte("PRIVATE PANIC SENTINEL")) {
		t.Fatal("panic escaped")
	}
	var capture PrelaunchTreeCapture
	b, _ := json.Marshal(r.Container["prelaunch_tree"])
	if json.Unmarshal(b, &capture) != nil || capture.CheckState != "failed" || capture.CheckFinishedMs < capture.CheckStartedMs || r.ExceptionInfo == nil {
		t.Fatal("missing failed callback result", capture)
	}
}

func TestPrelaunchConsumerPreservesPrimaryAndStaticCleanupFailure(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "returned_error", true: "panic_nil"}[panics], func(t *testing.T) {
			raw := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}).Bytes()
			p := filepath.Join(t.TempDir(), "input")
			os.WriteFile(p, raw, 0600)
			h := sha256.Sum256(raw)
			a, e := retainPrelaunch(context.Background(), p, PrelaunchTreeCapture{State: "complete", Digest: "sha256:" + hex.EncodeToString(h[:])})
			if e != nil {
				t.Fatal(e)
			}
			primary := errors.New("synthetic primary")
			err := consumePrelaunch(context.Background(), a, func(context.Context, *PrelaunchArtifact) error {
				if e := a.file.Close(); e != nil {
					t.Fatal(e)
				}
				if panics {
					panic(nil)
				}
				return primary
			})
			if err == nil || !strings.Contains(err.Error(), "prelaunch retention cleanup failed") || strings.Contains(err.Error(), p) {
				t.Fatal("missing/static cleanup refusal", err)
			}
			if !panics && !errors.Is(err, primary) {
				t.Fatal("primary error lost", err)
			}
			if panics && !strings.Contains(err.Error(), "prelaunch callback panicked") {
				t.Fatal("nil panic lost", err)
			}
			if _, e := os.Stat(a.dir); !os.IsNotExist(e) {
				t.Fatal("retention not removed", e)
			}
		})
	}
}
