// Package evaltest builds synthetic eval fixtures for tests of the eval packages.
package evaltest

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// Library opens an eval library in a new temporary project.
func Library(t testing.TB) *library.Library {
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

// Tree stores a tree of regular files.
func Tree(t testing.TB, l *library.Library, files map[string]string) string {
	t.Helper()
	var fs []trees.File
	for p, c := range files {
		d, err := l.PutObject([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		fs = append(fs, trees.File{Path: p, Mode: "100644", Size: int64(len(c)), Digest: d})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	b, _ := json.Marshal(trees.Tree{TreeVersion: 1, Files: fs})
	d, err := l.PutObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Go bug-fix project (standard library only).
const (
	GoMod   = "module shop\n\ngo 1.25\n"
	GoBug   = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price }\n"
	GoFix   = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price - discount }\n"
	GoOld   = "package shop\n\nimport \"testing\"\n\nfunc TestNoDiscount(t *testing.T) {\n\tif Total(10, 0) != 10 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	GoAdded = "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif got := Total(10, 3); got != 7 {\n\t\tt.Fatalf(\"Total(10, 3) = %d\", got)\n\t}\n}\n"
)

// GoBugFix packages the synthetic Go bug fix and returns its eval record.
func GoBugFix(t testing.TB, l *library.Library) map[string]any {
	t.Helper()
	base := Tree(t, l, map[string]string{"go.mod": GoMod, "cart.go": GoBug, "cart_test.go": GoOld})
	result := Tree(t, l, map[string]string{"go.mod": GoMod, "cart.go": GoFix, "cart_test.go": GoOld, "discount_test.go": GoAdded})
	at := map[string]any{"source": library.Digest([]byte("synthetic-session")), "record": "u1"}
	b, err := build.Build(l, build.Input{EpisodeID: "ep-00000000000000e1", Family: "bug_fix", Repo: "shop", Base: base, Result: result,
		Paths: []string{"cart.go", "discount_test.go"}, Attribution: "exclusive", Outcome: "test_passed",
		Request: "Total ignores the discount. Fix Total so it subtracts the discount from the price.", RequestAt: at,
		CheckHint: "go test ./...", Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return b.Record
}
