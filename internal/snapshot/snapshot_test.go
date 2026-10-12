package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/testkit"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func build(t *testing.T, dir string, prev *Local) *Local {
	t.Helper()
	repo := hint.RepoID(key, "r")
	res, err := scan.Scan(scan.Options{Root: dir, RepoID: repo, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	l, _, err := Build(prev, res, repo, key, ReadGitState(dir), time.Unix(1791700000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func repo(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for p, c := range files {
		testkit.WriteFile(t, dir, p, c)
	}
	testkit.Git(t, dir, "init", "-q")
	return dir
}

func ops(cs ChangeSet) map[string]int {
	m := map[string]int{}
	for _, c := range cs.Changes {
		m[c.Operation+"/"+c.Level+"/"+c.MatchMethod]++
	}
	return m
}

func TestRenameByHashAndAmbiguousCopies(t *testing.T) {
	dir := repo(t, map[string]string{"a/x.go": "package a\n\nfunc X() {}\n", "b.txt": "same\n"})
	before := build(t, dir, nil)
	if err := os.Rename(filepath.Join(dir, "a/x.go"), filepath.Join(dir, "a/y.go")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "b.txt"))
	testkit.WriteFile(t, dir, "c1.txt", "same\n")
	testkit.WriteFile(t, dir, "c2.txt", "same\n")
	after := build(t, dir, before)
	cs, _ := Diff(key, before, after)
	got := ops(cs)
	if got["RENAME/artifact/hash"] != 1 || got["AMBIGUOUS/artifact/hash"] != 1 || got["MOVE/symbol/key"] != 0 {
		t.Fatalf("%v", got)
	}
	for _, c := range cs.Changes {
		if c.Operation == "AMBIGUOUS" && len(c.Candidates) != 2 {
			t.Fatalf("ambiguous without candidates: %+v", c)
		}
	}
}

func TestSymbolMoveAndModify(t *testing.T) {
	dir := repo(t, map[string]string{"p/a.go": "package p\n\nfunc A() {}\n\nfunc B() int { return 1 }\n", "p/c.go": "package p\n\nfunc C() {}\n"})
	before := build(t, dir, nil)
	testkit.WriteFile(t, dir, "p/a.go", "package p\n\nfunc B() int { return 2 }\n")
	testkit.WriteFile(t, dir, "p/c.go", "package p\n\nfunc C() {}\n\nfunc A() {}\n")
	after := build(t, dir, before)
	cs, _ := Diff(key, before, after)
	got := ops(cs)
	if got["MOVE/symbol/key"] != 1 || got["MODIFY/symbol/key"] != 1 || got["MODIFY/artifact/key"] != 2 {
		t.Fatalf("%v", got)
	}
}

func TestCollidingSymbolsGetLocalIDs(t *testing.T) {
	dir := repo(t, map[string]string{"p/a.go": "package p\n\nfunc init() {}\n", "p/b.go": "package p\n\nfunc init() {}\n"})
	l := build(t, dir, nil)
	stable := hint.SymbolID(key, hint.RepoID(key, "r"), "go", "p.init")
	if len(l.Ambiguous[stable]) != 2 {
		t.Fatalf("ambiguous: %v", l.Ambiguous)
	}
	n := 0
	for _, e := range l.Entities {
		if e.ID == stable {
			t.Fatal("collided stable ID exported")
		}
		if e.Stability == "snapshot_local" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("local entities: %d", n)
	}
}

func TestParseErrorKeepsFile(t *testing.T) {
	dir := repo(t, map[string]string{"p/a.go": "package p\n\nfunc broken( {\n"})
	repoID := hint.RepoID(key, "r")
	res, err := scan.Scan(scan.Options{Root: dir, RepoID: repoID, Key: key})
	if err != nil || res.Stats.Unparsed != 1 {
		t.Fatalf("%v %+v", err, res.Stats)
	}
	if _, ok := map[string]bool{hint.FileID(key, repoID, "p/a.go"): true}[res.Entities[len(res.Entities)-1].ID]; !ok {
		t.Fatalf("file entity missing: %+v", res.Entities)
	}
}

func TestDigestIsStable(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "package a\n"})
	a := build(t, dir, nil)
	b := build(t, dir, nil)
	if a.Record.SnapshotID != b.Record.SnapshotID || a.Record.Digest != b.Record.Digest {
		t.Fatal("two scans of one tree differ")
	}
	c := build(t, dir, a)
	if c != a {
		t.Fatal("unchanged scan made a new snapshot")
	}
}
