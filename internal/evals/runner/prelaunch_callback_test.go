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
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("lost panic")
			}
		}()
		consumePrelaunch(context.Background(), a, func(context.Context, *PrelaunchArtifact) error { panic("synthetic") })
	}()
	if _, e := a.Materialize(context.Background(), t.TempDir()); e == nil {
		t.Fatal("panic retained capability")
	}
}
