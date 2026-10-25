//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryPathPhysicalAliasMissingAndDirectory(t *testing.T) {
	s, alias := aliasSelectionFixture(t)
	file := filepath.Join(s.Root, "notes.md")
	if e := os.WriteFile(file, []byte("synthetic"), 0755); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{file, filepath.Join(alias, "notes.md")} {
		got, e := RepositoryPath(s.Root, path)
		if e != nil || got.Resolved != file || got.Relative != "notes.md" || !got.Present || !got.Mode.IsRegular() || got.Mode&0111 == 0 {
			t.Fatal("same-file alias", got, e)
		}
	}
	dir := filepath.Join(s.Root, "directory")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	got, e := RepositoryPath(alias, filepath.Join(alias, "directory"))
	if e != nil || !got.Present || !got.Mode.IsDir() || got.Relative != "directory" {
		t.Fatal("directory observation", got, e)
	}
	got, e = RepositoryPath(s.Root, filepath.Join(alias, "missing", "new.go"))
	if e != nil || got.Present || got.Mode != 0 || got.Relative != "missing/new.go" || got.Resolved != filepath.Join(s.Root, "missing", "new.go") {
		t.Fatal("missing declaration", got, e)
	}
	for _, name := range []string{".hinted", ".github", "..notes"} {
		if _, e = RepositoryPath(s.Root, filepath.Join(alias, name)); e != nil {
			t.Fatal("safe prefix refused", name, e)
		}
	}
}

func TestRepositoryPathRefusesUnsafeAndPrivateTargets(t *testing.T) {
	s, alias := aliasSelectionFixture(t)
	file := filepath.Join(s.Root, "notes.md")
	if e := os.WriteFile(file, nil, 0600); e != nil {
		t.Fatal(e)
	}
	outside, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(outside, "notes.md"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	for name, target := range map[string]string{"inside-link": file, "outside-link": filepath.Join(outside, "notes.md"), "broken-link": filepath.Join(outside, "missing"), "parent-link": outside, "loop": "loop"} {
		if e = os.Symlink(target, filepath.Join(s.Root, name)); e != nil {
			t.Fatal(e)
		}
	}
	paths := []string{
		filepath.Join(alias, "inside-link"), filepath.Join(alias, "outside-link"), filepath.Join(alias, "broken-link"), filepath.Join(alias, "parent-link", "notes.md"), filepath.Join(alias, "loop"),
		filepath.Join(outside, "notes.md"), s.Root + "-sibling/notes.md", alias + "/../notes.md", alias,
	}
	for _, private := range []string{".hint", ".HINT", ".git", ".GIT"} {
		paths = append(paths, filepath.Join(alias, private, "secret"))
	}
	// A parent link cannot launder a private input path into a public path.
	if e = os.Symlink(s.Root, filepath.Join(s.Root, "private-link")); e != nil {
		t.Fatal(e)
	}
	paths = append(paths, filepath.Join(alias, "private-link", ".hint", "state", "project.json"))
	if e = os.MkdirAll(filepath.Join(s.Root, "nested", ".hint"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(s.Root, "nested", ".hint"), filepath.Join(s.Root, "private-parent")); e != nil {
		t.Fatal(e)
	}
	paths = append(paths, filepath.Join(alias, "private-parent", "declared-secret"))
	for _, path := range paths {
		if got, e := RepositoryPath(s.Root, path); e == nil {
			t.Errorf("unsafe path accepted %q: %+v", path, got)
		}
	}
	if _, e = RepositoryPath(filepath.Join(s.Root, "missing-root"), file); e == nil {
		t.Fatal("missing root accepted")
	}
}

func TestResolveLegacyFallbackParity(t *testing.T) {
	s, alias := aliasSelectionFixture(t)
	if e := os.Symlink("loop", filepath.Join(s.Root, "loop")); e != nil {
		t.Fatal(e)
	}
	for _, rel := range []string{"missing/new.go", "loop", "loop/child"} {
		if got := resolve(filepath.Join(alias, rel)); got != filepath.Join(s.Root, rel) {
			t.Errorf("legacy resolve changed for %s: %s", rel, got)
		}
	}
	if _, e := resolveExistingPrefix(filepath.Join(alias, "loop")); e == nil {
		t.Fatal("strict resolver hid loop")
	}
}
