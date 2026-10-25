package library

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestCaptureRootsUseExistingRetentionBarrier(t *testing.T) {
	for _, dir := range []string{CapturePolicyStateDir, CaptureReservationStateDir, CaptureSourceStateDir} {
		for _, kind := range []string{"empty", "orphan", "corrupt", "link"} {
			t.Run(dir+"/"+kind, func(t *testing.T) {
				s := project(t)
				l := Open(s, 0)
				d, e := l.PutObject([]byte("synthetic-retained-capture"))
				if e != nil {
					t.Fatal(e)
				}
				if e = l.PutIndex("opencode/kept", map[string]string{"source": d}); e != nil {
					t.Fatal(e)
				}
				path := s.Path("state", dir)
				switch kind {
				case "empty":
					e = os.MkdirAll(path, 0700)
				case "orphan":
					e = os.MkdirAll(path+"/orphan", 0700)
				case "corrupt":
					e = os.WriteFile(path, []byte("invalid"), 0600)
				case "link":
					e = os.Symlink(t.TempDir(), path)
				}
				if e != nil {
					t.Fatal(e)
				}
				for _, fn := range []func() error{func() error { _, e := l.Prune(time.Now(), 1); return e }, func() error { _, e := l.Forget([]string{"anything"}); return e }, func() error { _, e := l.Purge(); return e }, func() error { _, e := l.CollectGarbage(); return e }, func() error { return l.DeleteIndex("opencode/kept") }} {
					if e = fn(); !errors.Is(e, ErrDefinitionReferences) {
						t.Fatal("retention did not refuse", e)
					}
					if _, e = l.GetObject(d); e != nil {
						t.Fatal("object deleted before guard")
					}
					var index map[string]string
					if e = l.Index("opencode/kept", &index); e != nil {
						t.Fatal("index deleted before guard")
					}
				}
			})
		}
	}
}
