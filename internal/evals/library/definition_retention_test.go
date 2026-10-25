package library

import (
	"context"
	"errors"
	"github.com/neprel/git-hint/internal/store"
	"os"
	"testing"
	"time"
)

func TestDefinitionRetentionGuardBlocksEveryDestructiveEntry(t *testing.T) {
	for _, kind := range []string{"directory", "corrupt-file", "link"} {
		t.Run(kind, func(t *testing.T) {
			s := project(t)
			l := Open(s, 0)
			d, e := l.PutObject([]byte("kept"))
			if e != nil {
				t.Fatal(e)
			}
			if e = l.PutIndex("opencode/kept", map[string]string{"source": d}); e != nil {
				t.Fatal(e)
			}
			p := s.Path("state", WorkDefinitionStateDir)
			switch kind {
			case "directory":
				os.MkdirAll(p, 0700)
			case "corrupt-file":
				os.WriteFile(p, []byte("invalid"), 0600)
			case "link":
				os.Symlink(t.TempDir(), p)
			}
			for _, action := range []func() error{func() error { _, e := l.Prune(time.Now(), 1); return e }, func() error { _, e := l.Forget([]string{"anything"}); return e }, func() error { _, e := l.Purge(); return e }, func() error { _, e := l.CollectGarbage(); return e }, func() error { return l.DeleteIndex("opencode/kept") }} {
				if e = action(); !errors.Is(e, ErrDefinitionReferences) {
					t.Fatal("destructive action did not refuse", e)
				}
				var index map[string]string
				if e = l.Index("opencode/kept", &index); e != nil {
					t.Fatal("index deleted before retention guard", e)
				}
				if _, e = l.GetObject(d); e != nil {
					t.Fatal("referent deleted", e)
				}
			}
		})
	}
}
func TestDefinitionWriterAndDestructorUseSameNonblockingBoundary(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	d, e := l.PutObject([]byte("not yet referenced"))
	if e != nil {
		t.Fatal(e)
	}
	if e = l.PutIndex("opencode/kept", map[string]string{"source": d}); e != nil {
		t.Fatal(e)
	}
	lock, e := s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := Open(s, 0).CollectGarbage(); done <- e }()
	select {
	case e = <-done:
		if !errors.Is(e, store.ErrBusy) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("destructor blocked on held writer state lock")
	}
	if _, e = l.GetObject(d); e != nil {
		t.Fatal("busy destructor deleted bytes")
	}
	if e = Open(s, 0).DeleteIndex("opencode/kept"); !errors.Is(e, store.ErrBusy) {
		t.Fatal("index destructor missed common lock", e)
	}
	var index map[string]string
	if e = l.Index("opencode/kept", &index); e != nil {
		t.Fatal("busy index destructor deleted bytes", e)
	}
	lock.Unlock()
	if e = l.DeleteIndex("opencode/kept"); e != nil {
		t.Fatal("ordinary index deletion refused", e)
	}
	if n, e := l.CollectGarbage(); e != nil || n != 1 {
		t.Fatal("ordinary unowned garbage collection no longer works", n, e)
	}
}
