package library

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/neprel/git-hint/internal/store"
)

func cachedPath(t *testing.T, l *Library, data []byte) string {
	t.Helper()
	p, e := l.objectPath(Digest(data))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestCachedRegularDamageRemainsRepairable(t *testing.T) {
	for _, kind := range []string{"short", "same_length", "oversized_sparse"} {
		t.Run(kind, func(t *testing.T) {
			l := Open(project(t), 0)
			data := []byte("expected object bytes")
			p := cachedPath(t, l, data)
			old := []byte("bad")
			if kind == "same_length" {
				old = bytes.Repeat([]byte("x"), len(data))
			}
			if e := os.WriteFile(p, old, 0600); e != nil {
				t.Fatal(e)
			}
			if kind == "oversized_sparse" {
				if e := os.Truncate(p, 1<<30); e != nil {
					t.Fatal(e)
				}
			}
			d, e := l.PutObjectContext(context.Background(), data)
			if e != nil || d != Digest(data) {
				t.Fatalf("regular repair: %s %v", d, e)
			}
			got, e := l.GetObject(d)
			if e != nil || !bytes.Equal(got, data) {
				t.Fatalf("repaired bytes: %v", e)
			}
		})
	}
}

func TestCachedHitStillChecksExclusion(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	data := []byte("already cached")
	if _, e := l.PutObject(data); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.Root, ".git", "info", "exclude"), nil, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := l.PutObjectContext(context.Background(), data); !errors.Is(e, store.ErrNotExcluded) {
		t.Fatalf("cached hit bypassed exclusion: %v", e)
	}
}

type cacheReadProbe struct {
	total  int
	cancel context.CancelFunc
}

func (r *cacheReadProbe) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.total += len(p)
	if r.cancel != nil {
		r.cancel()
	}
	return len(p), nil
}
func TestCachedReadUsesExpectedLengthPlusOneAndCallerContext(t *testing.T) {
	r := &cacheReadProbe{}
	b, e := readCachedPrefix(context.Background(), r, 3)
	if e != nil || len(b) != 4 || r.total != 4 {
		t.Fatalf("read escaped bound: %d/%d %v", len(b), r.total, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r = &cacheReadProbe{cancel: cancel}
	if _, e = readCachedPrefix(ctx, r, 1<<20); !errors.Is(e, context.Canceled) || r.total > 32<<10 {
		t.Fatalf("read ignored cancellation: %d %v", r.total, e)
	}
	if _, e = readCachedPrefix(context.Background(), bytes.NewReader(nil), 0); e != nil {
		t.Fatal(e)
	}
}

func TestCachedCancellationAndReplacementLeaveObjectUnchanged(t *testing.T) {
	for _, mode := range []string{"cancel", "replace", "grow", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			l := Open(project(t), 0)
			data := []byte("expected cache")
			p := cachedPath(t, l, data)
			old := []byte("damaged regular")
			if e := os.WriteFile(p, old, 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := func() {
				switch mode {
				case "cancel":
					cancel()
				case "replace":
					if e := os.Rename(p, p+".original"); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(p, old, 0600); e != nil {
						t.Fatal(e)
					}
				case "grow":
					if e := os.Truncate(p, 1<<20); e != nil {
						t.Fatal(e)
					}
				case "symlink":
					if e := os.Rename(p, p+".original"); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(p+".original", p); e != nil {
						t.Fatal(e)
					}
				}
			}
			same, e := l.checkCachedObject(ctx, p, data, hook)
			if e == nil || same {
				t.Fatal("changed/cancelled cache accepted")
			}
			if mode == "cancel" && !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			fi, se := os.Lstat(p)
			if se != nil {
				t.Fatal("refusal removed object", se)
			}
			if mode == "symlink" {
				if fi.Mode()&os.ModeSymlink == 0 {
					t.Fatal("refusal changed link")
				}
				return
			}
			if mode == "grow" {
				if fi.Size() != 1<<20 {
					t.Fatal("refusal changed grown object")
				}
				return
			}
			got, se := os.ReadFile(p)
			if se != nil || !bytes.Equal(got, old) {
				t.Fatal("refusal changed regular bytes", se)
			}
		})
	}
}

func TestCachedSymlinkRefusesWithoutReadingOrRepair(t *testing.T) {
	l := Open(project(t), 0)
	data := []byte("expected object")
	p := cachedPath(t, l, data)
	target := filepath.Join(t.TempDir(), "foreign")
	if e := os.WriteFile(target, data, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, p); e != nil {
		t.Fatal(e)
	}
	if _, e := l.PutObjectContext(context.Background(), data); !errors.Is(e, ErrEscape) {
		t.Fatalf("cached link accepted: %v", e)
	}
	fi, e := os.Lstat(p)
	if e != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("cached link changed")
	}
	f, e := os.Open(target)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	got, e := io.ReadAll(f)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("foreign target changed")
	}
}

func TestCancelledCallerCannotAcceptCacheHitOrRepairDamage(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "hit", true: "repair"}[damaged], func(t *testing.T) {
			l := Open(project(t), 0)
			want := []byte("expected cache")
			p := cachedPath(t, l, want)
			before := want
			if damaged {
				before = []byte("damaged cache")
			}
			if e := os.WriteFile(p, before, 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if d, e := l.PutObjectContext(ctx, want); d != "" || !errors.Is(e, context.Canceled) {
				t.Fatalf("cancelled caller: %q %v", d, e)
			}
			after, e := os.ReadFile(p)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("cancelled caller changed cache", e)
			}
		})
	}
}
