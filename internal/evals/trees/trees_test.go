package trees

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func lib(t *testing.T) *library.Library {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return library.Open(s, 0)
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "cart.go", "package shop\n")
	testkit.WriteFile(t, dir, "old.go", "package shop\n// old\n")
	testkit.WriteFile(t, dir, "gone.go", "package shop\n// gone\n")
	testkit.WriteFile(t, dir, ".gitignore", "*.log\n")
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "base")
	// dirty state: modified, renamed, removed, untracked, ignored, link
	testkit.WriteFile(t, dir, "cart.go", "package shop\n// dirty\n")
	testkit.Git(t, dir, "mv", "old.go", "renamed.go")
	os.Remove(filepath.Join(dir, "gone.go"))
	testkit.WriteFile(t, dir, "notes.txt", "untracked input\n")
	testkit.WriteFile(t, dir, "debug.log", "ignored\n")
	os.Symlink("cart.go", filepath.Join(dir, "link.go"))
	return dir
}

func gitState(t *testing.T, dir string) string {
	t.Helper()
	st := testkit.Git(t, dir, "status", "--porcelain=v1", "--untracked-files=all")
	idx, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	var objs []string
	filepath.Walk(filepath.Join(dir, ".git", "objects"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			objs = append(objs, p)
		}
		return nil
	})
	return st + "|" + string(idx) + "|" + strings.Join(objs, ",")
}

func paths(tr Tree) []string {
	var out []string
	for _, f := range tr.Files {
		out = append(out, f.Path+":"+f.Mode)
	}
	return out
}

func TestWorktreeStateIsExactAndReadOnly(t *testing.T) {
	l, dir := lib(t), repo(t)
	before := gitState(t, dir)
	d, err := FromWorktree(context.Background(), l, dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if gitState(t, dir) != before {
		t.Fatal("capture changed the work tree or Git metadata")
	}
	tr, _ := Load(l, d)
	got := strings.Join(paths(tr), " ")
	want := ".gitignore:100644 cart.go:100644 link.go:120000 notes.txt:100644 renamed.go:100644"
	if got != want {
		t.Fatalf("tree %q", got)
	}
	again, _ := FromWorktree(context.Background(), l, dir, Limits{})
	if again != d {
		t.Fatal("the same state gives another digest")
	}
	testkit.WriteFile(t, dir, "cart.go", "package shop\n// later edit\n")
	later, _ := FromWorktree(context.Background(), l, dir, Limits{})
	lt, _ := Load(l, later)
	_, _, changed := Diff(tr, lt)
	if later == d || len(changed) != 1 || changed[0] != "cart.go" {
		t.Fatalf("later edit: %v", changed)
	}
	if _, err := FromWorktree(context.Background(), l, dir, Limits{MaxFileBytes: 4}); !isCode(err, "binary_too_large") {
		t.Fatalf("file limit: %v", err)
	}
}

// snapshotRepo builds a bare repository with one tree, the way OpenCode's snapshot
// repository holds step trees (written with a separate index, never a commit).
func snapshotRepo(t *testing.T, src string) (gitDir, tree string) {
	t.Helper()
	gitDir = filepath.Join(t.TempDir(), "snap")
	if out, err := exec.Command("git", "init", "-q", "--bare", gitDir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	cmd := exec.Command("sh", "-c", "git --git-dir \"$1\" --work-tree \"$2\" add -A . ':!.git' && git --git-dir \"$1\" --work-tree \"$2\" write-tree", "sh", gitDir, src)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "idx"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return gitDir, lines[len(lines)-1]
}

func TestOpenCodeSnapshotTreeMatchesWorktree(t *testing.T) {
	l := lib(t)
	src := t.TempDir()
	testkit.WriteFile(t, src, "a/cart.go", "package shop\n")
	testkit.WriteFile(t, src, "run.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(src, "run.sh"), 0o755)
	gitDir, tree := snapshotRepo(t, src)
	d, err := FromGitTree(context.Background(), l, []string{t.TempDir(), gitDir}, tree, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// the same content from a work tree gives the same HINT tree
	wt := t.TempDir()
	testkit.Git(t, wt, "init", "-q")
	testkit.WriteFile(t, wt, "a/cart.go", "package shop\n")
	testkit.WriteFile(t, wt, "run.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(wt, "run.sh"), 0o755)
	d2, err := FromWorktree(context.Background(), l, wt, Limits{})
	if err != nil || d2 != d {
		t.Fatalf("snapshot %s vs work tree %s (%v)", d, d2, err)
	}
	if _, err := FromGitTree(context.Background(), l, []string{gitDir}, strings.Repeat("a", 40), Limits{}); !isCode(err, "baseline_missing") {
		t.Fatalf("missing tree: %v", err)
	}
}

func TestLFSAndSubmoduleAreUnsupported(t *testing.T) {
	l := lib(t)
	src := t.TempDir()
	testkit.WriteFile(t, src, "big.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:00\nsize 1\n")
	gitDir, tree := snapshotRepo(t, src)
	if _, err := FromGitTree(context.Background(), l, []string{gitDir}, tree, Limits{}); !isCode(err, "lfs_unsupported") {
		t.Fatalf("lfs: %v", err)
	}
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	testkit.Git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",vendor/lib")
	if _, err := FromWorktree(context.Background(), l, dir, Limits{}); !isCode(err, "submodule_unsupported") {
		t.Fatalf("submodule: %v", err)
	}
}

func TestMaterializeRoundTripAndSafety(t *testing.T) {
	l, dir := lib(t), repo(t)
	d, _ := FromWorktree(context.Background(), l, dir, Limits{})
	out := t.TempDir()
	if err := Materialize(l, d, out); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	testkit.Git(t, wt, "init", "-q")
	cp := exec.Command("sh", "-c", "cp -R \"$1\"/. \"$2\"/", "sh", out, wt)
	if b, err := cp.CombinedOutput(); err != nil {
		t.Fatal(string(b))
	}
	d2, _ := FromWorktree(context.Background(), l, wt, Limits{})
	if d2 != d {
		t.Fatal("materialized state differs")
	}
	if err := Materialize(l, d, out); err == nil {
		t.Fatal("materialize into a non-empty directory")
	}
	for _, bad := range []File{
		{Path: "../escape.go", Mode: "100644"},
		{Path: ".git/config", Mode: "100644"},
		{Path: "link", Mode: "120000"},
	} {
		obj := "x"
		if bad.Mode == "120000" {
			obj = "../../outside"
		}
		dg, _ := l.PutObject([]byte(obj))
		bad.Digest, bad.Size = dg, int64(len(obj))
		b, _ := json.Marshal(Tree{TreeVersion: 1, Files: []File{bad}})
		td, _ := l.PutObject(b)
		if err := Materialize(l, td, t.TempDir()); err == nil {
			t.Errorf("unsafe entry %s accepted", bad.Path)
		}
	}
}

func isCode(err error, code string) bool {
	var u *Unsupported
	return errors.As(err, &u) && u.Code == code
}
