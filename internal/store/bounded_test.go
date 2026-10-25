//go:build unix

package store

import (
	"bytes"
	"context"
	"errors"
	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func boundedFixture(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	s, _, e := Init(root, InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestBoundedStateExactBytesLimitsAndAtomicNoReplace(t *testing.T) {
	s := boundedFixture(t)
	ctx := context.Background()
	data := []byte("source bytes")
	if e := s.WriteStateImmutableContext(ctx, data, 64, "state", "typed", "one.json"); e != nil {
		t.Fatal(e)
	}
	got, e := s.ReadStateBounded(ctx, 64, "state", "typed", "one.json")
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal(e)
	}
	if e = s.WriteStateImmutableContext(ctx, []byte("other"), 64, "state", "typed", "one.json"); !errors.Is(e, os.ErrExist) {
		t.Fatal("immutable file overwritten", e)
	}
	if _, e = s.ReadStateBounded(ctx, 2, "state", "typed", "one.json"); e == nil {
		t.Fatal("read cap ignored")
	}
	if _, e = s.ReadStateBounded(ctx, 64, "state", "..", "typed"); e == nil {
		t.Fatal("traversal accepted")
	}
	for i := 0; i < 2; i++ {
		name := []string{"a", "b"}[i]
		if e = s.WriteStateImmutableContext(ctx, data, 64, "state", "typed", name); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.ReadStateDirBounded(ctx, 1, "state", "typed"); e == nil {
		t.Fatal("directory count ignored")
	}
	if _, e = s.ReadStateDirBounded(ctx, int(^uint(0)>>1), "state", "typed"); e == nil {
		t.Fatal("directory count overflow accepted")
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.ReadStateBounded(c, 64, "state", "typed", "one.json"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = s.WriteStateImmutableContext(c, data, 64, "state", "typed", "cancel.json"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = os.Stat(s.Path("state", "typed", "cancel.json")); !os.IsNotExist(e) {
		t.Fatal("cancelled write committed")
	}
}
func TestBoundedStateNoFollowFIFOAndChangedSource(t *testing.T) {
	for _, kind := range []string{"fifo", "file-link", "ancestor-link", "root-link", "replace", "grow", "mode"} {
		t.Run(kind, func(t *testing.T) {
			s := boundedFixture(t)
			ctx := context.Background()
			path := s.Path("state", "typed", "one.json")
			os.MkdirAll(filepath.Dir(path), 0700)
			os.WriteFile(path, []byte("data"), 0600)
			switch kind {
			case "fifo":
				os.Remove(path)
				if e := unix.Mkfifo(path, 0600); e != nil {
					t.Fatal(e)
				}
			case "file-link":
				os.Rename(path, path+".old")
				os.Symlink(path+".old", path)
			case "ancestor-link":
				dir := filepath.Dir(path)
				os.Rename(dir, dir+".old")
				os.Symlink(dir+".old", dir)
			case "root-link":
				old := s.Root
				s.Root = filepath.Join(t.TempDir(), "root-alias")
				os.Symlink(old, s.Root)
			default:
				src, e := openBoundedState(s.Root, []string{Dir, "state", "typed", "one.json"}, false)
				if e != nil {
					t.Fatal(e)
				}
				defer src.file.Close()
				switch kind {
				case "replace":
					os.Rename(path, path+".old")
					os.WriteFile(path, []byte("data"), 0600)
				case "grow":
					os.Truncate(path, 5)
				case "mode":
					os.Chmod(path, 0700)
				}
				if e = src.verify(); e == nil {
					t.Fatal("changed source accepted")
				}
				return
			}
			start := time.Now()
			if _, e := s.ReadStateBounded(ctx, 32, "state", "typed", "one.json"); e == nil {
				t.Fatal("unsafe source accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("unsafe source blocked")
			}
		})
	}
}
func TestBoundedStateLockBusyAndCancellationNeverWait(t *testing.T) {
	s := boundedFixture(t)
	lock, e := s.LockState(false)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Unlock()
	start := time.Now()
	if _, e = s.TryLockStateContext(context.Background()); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	if time.Since(start) > time.Second {
		t.Fatal("non-reentrant lock waited")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = s.TryLockStateContext(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestBoundedStateMetadataAndAggregateShape(t *testing.T) {
	s := boundedFixture(t)
	p := s.Path("state", "huge.json")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(1 << 30); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = s.ReadStateBounded(context.Background(), 1<<20, "state", "huge.json"); e == nil {
		t.Fatal("oversized sparse file read")
	}
	if e = s.WriteStateImmutableContext(context.Background(), []byte(strings.Repeat("x", 65)), 64, "state", "bad.json"); e == nil {
		t.Fatal("oversized record written")
	}
}
