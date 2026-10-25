//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
)

func boundedExportFixture(t *testing.T) (*store.Store, *library.Library, map[string]any) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	s, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	l := library.Open(s, 0)
	return s, l, evaltest.GoBugFix(t, l)
}
func TestExportBoundedSameCoreLegacyParity(t *testing.T) {
	_, l, rec := boundedExportFixture(t)
	a := filepath.Join(t.TempDir(), "legacy")
	b := filepath.Join(t.TempDir(), "bounded")
	old, e := Export(l, rec, a, Settings{})
	if e != nil {
		t.Fatal(e)
	}
	fresh, e := ExportBounded(context.Background(), l, rec, b, Settings{})
	if e != nil {
		t.Fatal(e)
	}
	da, e := DirDigest(a)
	if e != nil {
		t.Fatal(e)
	}
	db, e := DirDigest(b)
	if e != nil || da != db || old.Name != fresh.Name || old.Canary != fresh.Canary || !reflect.DeepEqual(old.Inputs, fresh.Inputs) {
		t.Fatal("legacy export changed", e, old, fresh, da, db)
	}
}
func TestExportBoundedContextBeforeIO(t *testing.T) {
	ctx, c := context.WithCancel(context.Background())
	c()
	for _, ctx := range []context.Context{nil, ctx} {
		dir := filepath.Join(t.TempDir(), "absent")
		if task, e := ExportBounded(ctx, nil, nil, dir, Settings{}); e == nil || task != nil {
			t.Fatal(task, e)
		}
		if _, e := os.Lstat(dir); !os.IsNotExist(e) {
			t.Fatal("IO before context refusal", e)
		}
	}
}
func TestExportBoundedSourceRefusals(t *testing.T) {
	for _, kind := range []string{"corrupt", "oversize", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			s, l, rec := boundedExportFixture(t)
			d := rec["instruction"].(string)
			hex := strings.TrimPrefix(d, "sha256:")
			p := s.Path("state", "evals", "objects", hex[:2], hex[2:])
			raw, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "corrupt":
				e = os.WriteFile(p, []byte("wrong"), 0600)
			case "oversize":
				var big string
				big, e = l.PutObject([]byte(strings.Repeat("x", (1<<20)+1)))
				rec["instruction"] = big
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				e = os.WriteFile(target, raw, 0600)
				if e == nil {
					e = os.Remove(p)
				}
				if e == nil {
					e = os.Symlink(target, p)
				}
			case "fifo":
				e = os.Remove(p)
				if e == nil {
					e = unix.Mkfifo(p, 0600)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if task, e := ExportBounded(context.Background(), l, rec, filepath.Join(t.TempDir(), "task"), Settings{}); e == nil || task != nil {
				t.Fatal("accepted invalid bounded source", task, e)
			}
		})
	}
}
func TestExportObjectBudgetPartTotalAndCancellation(t *testing.T) {
	_, l, _ := boundedExportFixture(t)
	d, e := l.PutObject([]byte("x"))
	if e != nil {
		t.Fatal(e)
	}
	for _, b := range []*exportObjectBudget{{ctx: context.Background(), used: 128 << 20}, {ctx: context.Background(), parts: 5}} {
		if _, e := b.read(l, "instruction", d); e == nil {
			t.Fatal("budget widened")
		}
	}
	b := &exportObjectBudget{ctx: context.Background()}
	if _, e = b.read(l, "unknown", d); e == nil {
		t.Fatal("unknown part")
	}
	ctx, c := context.WithCancel(context.Background())
	c()
	b.ctx = ctx
	if _, e = b.read(l, "instruction", d); e == nil {
		t.Fatal("canceled source read")
	}
}
