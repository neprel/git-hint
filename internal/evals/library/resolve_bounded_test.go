//go:build unix

package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestResolveBoundedReadRejectsCorruptLinksAndNonregular(t *testing.T) {
	l := Open(project(t), 0)
	d, e := l.PutObject([]byte("original"))
	if e != nil {
		t.Fatal(e)
	}
	p, e := l.objectPath(d)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, []byte("changed!"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e = l.ResolveBoundedRead(context.Background(), "object", d, 1024); e == nil {
		t.Fatal("corrupt object accepted")
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	target := p + "-target"
	if e = os.WriteFile(target, []byte("original"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(target, p); e != nil {
		t.Fatal(e)
	}
	if _, e = l.ResolveBoundedRead(context.Background(), "object", d, 1024); e == nil {
		t.Fatal("symbolic link accepted")
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e = syscall.Mkfifo(p, 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e = l.ResolveBoundedRead(context.Background(), "object", d, 1024); e == nil {
		t.Fatal("fifo accepted")
	}
}
func TestResolveBoundedMetadataOneMiBCap(t *testing.T) {
	l := Open(project(t), 0)
	p := l.path("manifests", "eval")
	if e := os.MkdirAll(p, 0o700); e != nil {
		t.Fatal(e)
	}
	name := "ev-0123456789abcdef@1"
	if e := os.WriteFile(p+"/"+name+".json", []byte(strings.Repeat("x", (1<<20)+1)), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := l.ResolveBoundedRead(context.Background(), "eval", name, 1<<20); e == nil {
		t.Fatal("oversized metadata accepted")
	}
	for _, bad := range []string{"../outside", "x/y", ".hidden", ""} {
		if _, e := l.ResolveBoundedRead(context.Background(), "eval", bad, 1<<20); e == nil {
			t.Fatal("record traversal accepted")
		}
	}
}

// EV-2: identical bytes outside the original source ancestry are not an approved source.
func TestResolveRejectsSymlinkedSourceAncestors(t *testing.T) {
	for _, component := range []string{"library-root", "state-parent", "hint-parent", "object-parent", "project-root", "project-root-parent"} {
		t.Run(component, func(t *testing.T) {
			l := Open(project(t), 0)
			d, e := l.PutObject([]byte("synthetic object"))
			if e != nil {
				t.Fatal(e)
			}
			original := l.s.Path("state", "evals")
			switch component {
			case "state-parent":
				original = l.s.Path("state")
			case "hint-parent":
				original = l.s.Path()
			case "object-parent":
				original = l.path("objects")
			case "project-root":
				original = l.s.Root
			case "project-root-parent":
				alias := filepath.Join(t.TempDir(), "parent-alias")
				if e = os.Symlink(filepath.Dir(l.s.Root), alias); e != nil {
					t.Fatal(e)
				}
				l.s.Root = filepath.Join(alias, filepath.Base(l.s.Root))
				if _, e = l.ResolveBoundedRead(context.Background(), "object", d, 1024); e == nil {
					t.Fatal("project parent alias accepted")
				}
				return
			}
			external := filepath.Join(t.TempDir(), "external-source")
			if e = os.Rename(original, external); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink(external, original); e != nil {
				t.Fatal(e)
			}
			if _, e = l.ResolveBoundedRead(context.Background(), "object", d, 1024); e == nil {
				t.Fatal("replaced source accepted")
			}
		})
	}
}
