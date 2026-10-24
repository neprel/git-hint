package library

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPutObjectContextCancellationDuringExclusionCheck(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(s.Root, ".git", "context-command-started")
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nfor arg do if [ \"$arg\" = check-ignore ]; then printf started > " + quote(marker) + "; printf 'partial-output'; exec sleep 30; fi; done\nexec " + quote(realGit) + " \"$@\"\n"
	bin := t.TempDir()
	if e = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := []byte("uncommitted synthetic bytes")
	done := make(chan error, 1)
	go func() { _, e := l.PutObjectContext(ctx, data); done <- e }()
	// Wait for observed command entry under parallel build/test load; the actual
	// post-cancel join bound below remains one second.
	wait := time.NewTimer(8 * time.Second)
	defer wait.Stop()
	for {
		if _, e = os.Stat(marker); e == nil {
			break
		}
		select {
		case <-wait.C:
			t.Fatal("check command did not start")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case e = <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel not preserved: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("persistence command outlived caller cancellation")
	}
	path, _ := l.objectPath(Digest(data))
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("cancelled safety check persisted object")
	}
}
func TestPutObjectContextCancelledWhileWaitingForWriter(t *testing.T) {
	l := Open(project(t), 0)
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := l.PutObjectContext(ctx, []byte("pending")); done <- e }()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("writer wait ignored cancellation")
	}
}
