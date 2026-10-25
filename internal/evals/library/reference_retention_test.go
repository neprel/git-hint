package library

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestReferencePreparationRetentionUsesExistingBarrier(t *testing.T) {
	for _, kind := range []string{"empty-directory", "orphan", "corrupt-file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := project(t)
			l := Open(s, 0)
			d, e := l.PutObject([]byte("retained-reference"))
			if e != nil {
				t.Fatal(e)
			}
			if e = l.PutIndex("opencode/kept", map[string]string{"source": d}); e != nil {
				t.Fatal(e)
			}
			p := s.Path("state", ReferencePreparationStateDir)
			switch kind {
			case "empty-directory":
				e = os.MkdirAll(p, 0700)
			case "orphan":
				e = os.MkdirAll(p+"/orphan", 0700)
			case "corrupt-file":
				e = os.WriteFile(p, []byte("invalid"), 0600)
			case "symlink":
				e = os.Symlink(t.TempDir(), p)
			}
			if e != nil {
				t.Fatal(e)
			}
			for _, action := range []func() error{func() error { _, e := l.Prune(time.Now(), 1); return e }, func() error { _, e := l.Forget([]string{"anything"}); return e }, func() error { _, e := l.Purge(); return e }, func() error { _, e := l.CollectGarbage(); return e }, func() error { return l.DeleteIndex("opencode/kept") }} {
				if e = action(); !errors.Is(e, ErrDefinitionReferences) {
					t.Fatal(e)
				}
				if _, e = l.GetObject(d); e != nil {
					t.Fatal(e)
				}
				var v map[string]string
				if e = l.Index("opencode/kept", &v); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
