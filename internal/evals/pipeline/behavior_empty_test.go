package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func TestObservedStartRejectsDirectoryOnlyCaptureBeforeAmbientGit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "host.go", "package host\n")
	testkit.Git(t, root, "add", "host.go")
	s := &store.Store{Root: root, Cfg: store.Config{Repos: []store.Repo{{Name: "app", Path: "."}}}}
	if err := os.MkdirAll(s.Path("cache"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".git", "monitor-marker")
	hook := filepath.Join(root, ".git", "fake-monitor.sh")
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	script := fmt.Sprintf("#!/bin/sh\nprintf hit >> %s\nprintf 'token\\000'\n", quote(marker))
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, root, "config", "core.fsmonitor", hook)
	// Prove the exact ambient discovery command can invoke the callback here.
	testkit.Git(t, s.Path("cache"), "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("fsmonitor positive control did not run", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	artifact := filepath.Join(work, "jobs", "trial", "trial__1", "agent", "prelaunch.tar")
	if err := os.MkdirAll(filepath.Dir(artifact), 0700); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	for _, name := range []string{"app/", "app/empty/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body.Bytes()))
	files, err := runner.MaterializePrelaunchTree(artifact, digest, t.TempDir())
	if err != nil || len(files) != 0 {
		t.Fatalf("directory-only materialization: files=%v err=%v", files, err)
	}
	var job jobResult
	start := int64(4)
	execution := "sha256:" + strings.Repeat("c", 64)
	job.Container.PrelaunchTree = &runner.PrelaunchTreeCapture{State: "complete", Digest: digest, Clock: "ledger_anchor_monotonic_ms", StartedMs: 1, FinishedMs: 3, ExecutionDigest: execution}
	job.Container.AgentBinary.ExecutionDigest = execution
	job.Container.Ledger = &runner.LedgerReport{AgentRun: &runner.ReportAgentRun{ExecutionDigest: execution, EnclosureStartMs: &start}}
	capture := *job.Container.PrelaunchTree
	rec := map[string]any{"repositories": []any{map[string]any{"repo": "app", "base_tree": "sha256:" + strings.Repeat("b", 64)}}}
	resolve, base, observed, cleanup, err := observedStartResolver(context.Background(), s, rec, work, job)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || err.Error() != "observed start unavailable" || resolve != nil || base != "" || observed != "" || cleanup != nil {
		t.Fatalf("empty capture returned geometry: base=%q observed=%q cleanup=%t err=%v", base, observed, cleanup != nil, err)
	}
	if !reflect.DeepEqual(capture, *job.Container.PrelaunchTree) {
		t.Fatal("resolver changed capture status")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("empty capture invoked ambient Git", err)
	}
	remaining, err := os.ReadDir(s.Path("cache"))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("refused capture left materialized files: %v %v", remaining, err)
	}
	// A regular-file capture still resolves only its explicit materialized file.
	digest, err = writeObservedArchive(artifact)
	if err != nil {
		t.Fatal(err)
	}
	job.Container.PrelaunchTree.Digest = digest
	resolve, base, observed, cleanup, err = observedStartResolver(context.Background(), s, rec, work, job)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if id, ok := resolve("/app/observed_only.go"); !ok || id == "" || base != rec["repositories"].([]any)[0].(map[string]any)["base_tree"] || observed != digest {
		t.Fatal("regular capture did not preserve explicit geometry")
	}
	if _, ok := resolve("/app/host.go"); ok {
		t.Fatal("regular capture included host repository file")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("regular capture invoked ambient Git", err)
	}
}
