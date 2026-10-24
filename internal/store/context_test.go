package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/testkit"
)

func TestCheckPathContextRejectsOutputFromFailedCommand(t *testing.T) {
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	s, _, e := Init(root, InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CheckPathContext(context.Background(), s.Path("state", "probe")); e != nil {
		t.Fatal(e)
	}
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	quoted := "'" + strings.ReplaceAll(realGit, "'", "'\\''") + "'"
	bin := t.TempDir()
	script := "#!/bin/sh\nfor arg do if [ \"$arg\" = check-ignore ]; then\n after=0\n for value do if [ \"$after\" = 1 ]; then printf '.git/info/exclude:1:/.hint/\\t%s\\n' \"$value\"; fi; if [ \"$value\" = -- ]; then after=1; fi; done\n exit 2\nfi; done\nexec " + quoted + " \"$@\"\n"
	if e = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if e = s.CheckPathContext(context.Background(), s.Path("state", "probe")); e == nil {
		t.Fatal("failed command's matching output accepted")
	}
}
