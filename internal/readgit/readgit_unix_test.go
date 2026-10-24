//go:build unix

package readgit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadGitExitOneCannotMaskIncompleteStreams(t *testing.T) {
	bin := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nsleep 3 &\nprintf '%s' \"$!\" > " + quote(pidfile) + "\nprintf 'matching-prefix\\n'\nexit 1\n"
	if e := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer func() {
		if raw, e := os.ReadFile(pidfile); e == nil {
			if pid, e := strconv.Atoi(string(raw)); e == nil {
				if child, e := os.FindProcess(pid); e == nil {
					_ = child.Kill()
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, e := Output(ctx, t.TempDir(), 1024, "check-ignore")
	if e == nil || ExitOne(e) || len(raw) != 0 {
		t.Fatalf("incomplete exit1 stream accepted: %q %v", raw, e)
	}
}
