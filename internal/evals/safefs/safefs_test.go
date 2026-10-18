package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T) (*Dir, string, string) {
	t.Helper()
	outer := t.TempDir()
	in := filepath.Join(outer, "in")
	d, err := Open(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, in, outer
}

// G10-1: the link chain d -> ., x -> d/.. must not let x/file reach the parent.
func TestLinkChainCannotLeave(t *testing.T) {
	d, _, outer := open(t)
	if err := d.Symlink("d", "."); err != nil {
		t.Fatal(err)
	}
	// x -> d/.. is lexically inside; it is either refused now or by Check.
	xerr := d.Symlink("x", "d/..")
	if err := d.WriteFile("x/file", []byte("escaped"), 0o644); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("write through link chain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outer, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file was written outside the directory")
	}
	if xerr == nil {
		if err := d.Check(); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("Check accepted an escaping link chain: %v", err)
		}
	}
}

func TestRefusals(t *testing.T) {
	d, in, outer := open(t)
	for _, n := range []string{"", "/abs", "..", "../x", "a/../../x", "a\\b", "."} {
		if err := d.WriteFile(n, nil, 0o644); !errors.Is(err, ErrUnsafe) {
			t.Errorf("WriteFile(%q) = %v", n, err)
		}
	}
	for _, tg := range []string{"/etc", "..", "../x", "a/../../x", ""} {
		if err := d.Symlink("l", tg); !errors.Is(err, ErrUnsafe) {
			t.Errorf("Symlink(l, %q) = %v", tg, err)
		}
	}
	// A link to a directory inside is allowed, but no write passes through it.
	if err := d.Mkdir("sub"); err != nil {
		t.Fatal(err)
	}
	if err := d.Symlink("ln", "sub"); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("ln/f", []byte("x"), 0o644); !errors.Is(err, ErrUnsafe) {
		t.Errorf("write through an inside link: %v", err)
	}
	if err := d.Mkdir("ln/deeper"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("mkdir through a link: %v", err)
	}
	// An existing file is not overwritten (and an existing link is not followed).
	if err := d.WriteFile("a", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("a", []byte("2"), 0o644); err == nil {
		t.Error("duplicate entry overwrote a file")
	}
	os.WriteFile(filepath.Join(outer, "target"), []byte("keep"), 0o644)
	os.Symlink(filepath.Join(outer, "target"), filepath.Join(in, "planted"))
	if err := d.WriteFile("planted", []byte("over"), 0o644); err == nil {
		t.Error("write followed a planted link")
	}
	if b, _ := os.ReadFile(filepath.Join(outer, "target")); string(b) != "keep" {
		t.Error("outside file changed")
	}
	if err := d.Check(); !errors.Is(err, ErrUnsafe) {
		t.Errorf("Check accepted an absolute link: %v", err)
	}
}

func TestCheckAllowsInsideAndDangling(t *testing.T) {
	d, _, _ := open(t)
	d.WriteFile("a/b.txt", []byte("x"), 0o644)
	d.Symlink("a/l", "b.txt")
	d.Symlink("a/up", "../a/b.txt")
	d.Symlink("dangling", "missing/file")
	if err := d.Check(); err != nil {
		t.Fatal(err)
	}
	d.Symlink("loop1", "loop2")
	d.Symlink("loop2", "loop1")
	if err := d.Check(); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Check accepted a loop: %v", err)
	}
}

// Links created in an order where the escape appears only later are still refused.
func TestCheckLateEscape(t *testing.T) {
	d, _, _ := open(t)
	if err := d.Symlink("x", "d/.."); err != nil {
		t.Fatal(err)
	}
	if err := d.Check(); err != nil {
		t.Fatal(err)
	}
	d.Symlink("d", ".")
	if err := d.Check(); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Check: %v", err)
	}
}
