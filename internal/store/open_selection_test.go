//go:build unix

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func aliasSelectionFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := boundedFixture(t)
	parent, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(parent, "selected-project")
	if e = os.Symlink(s.Root, alias); e != nil {
		t.Fatal(e)
	}
	return s, alias
}

func TestOpenCanonicalProjectSelectionPreservesConfigAndLockDomain(t *testing.T) {
	s, alias := aliasSelectionFixture(t)
	before, e := os.ReadFile(s.Path("state", "project.json"))
	if e != nil {
		t.Fatal(e)
	}
	opened, e := Open(alias)
	if e != nil {
		t.Fatal(e)
	}
	if opened.Root != s.Root || !reflect.DeepEqual(opened.Cfg, s.Cfg) || !bytes.Equal(opened.Key(), s.Key()) {
		t.Fatal("Open changed selected store identity/config")
	}
	for _, repo := range s.Cfg.Repos {
		if opened.RepoID(repo) != s.RepoID(repo) || opened.RepoRoot(repo) != s.RepoRoot(repo) {
			t.Fatal("repository refs changed")
		}
	}
	after, _ := os.ReadFile(s.Path("state", "project.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("Open rewrote project config")
	}
	legacy, e := s.LockState(false)
	if e != nil {
		t.Fatal(e)
	}
	if lock, e := opened.TryLockStateContext(context.Background()); !errors.Is(e, ErrBusy) {
		if lock != nil {
			lock.Unlock()
		}
		t.Fatal("lock domains differ", e)
	}
	legacy.Unlock()
	strict, e := opened.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if lock, e := s.LockState(false); !errors.Is(e, ErrBusy) {
		if lock != nil {
			lock.Unlock()
		}
		t.Fatal("reverse lock domains differ", e)
	}
	strict.Unlock()
	forged := *s
	forged.Root = alias
	if _, e = forged.ReadStateBounded(context.Background(), projectConfigMaxBytes, "state", "project.json"); !errors.Is(e, ErrBoundedState) {
		t.Fatal("strict reader followed forged root alias", e)
	}
	if lock, e := forged.TryLockStateContext(context.Background()); !errors.Is(e, ErrBoundedState) {
		if lock != nil {
			lock.Unlock()
		}
		t.Fatal("strict lock followed forged root alias", e)
	}
}

func TestOpenSelectionReplacementAndExactBytesRefuse(t *testing.T) {
	for _, kind := range []string{"root-retarget-equal-bytes", "config-replace-equal-bytes", "config-change-equal-shape", "canonical-root-link", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, alias := aliasSelectionFixture(t)
			selection, e := selectStoreRoot(alias)
			if e != nil {
				t.Fatal(e)
			}
			raw, e := selection.read(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			info, e := os.Stat(s.Path("state", "project.json"))
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			switch kind {
			case "root-retarget-equal-bytes":
				other, e := filepath.EvalSymlinks(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				p := filepath.Join(other, Dir, "state", "project.json")
				if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, raw, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(alias); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(other, alias); e != nil {
					t.Fatal(e)
				}
			case "config-replace-equal-bytes":
				p := s.Path("state", "project.json")
				if e = os.Rename(p, p+".saved"); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, raw, info.Mode().Perm()); e != nil {
					t.Fatal(e)
				}
				if e = os.Chtimes(p, info.ModTime(), info.ModTime()); e != nil {
					t.Fatal(e)
				}
			case "config-change-equal-shape":
				changed := append([]byte(nil), raw...)
				i := bytes.IndexByte(changed, ' ')
				if i < 0 {
					t.Fatal("fixture lacks JSON whitespace")
				}
				changed[i] = '\t'
				p := s.Path("state", "project.json")
				if e = os.WriteFile(p, changed, info.Mode().Perm()); e != nil {
					t.Fatal(e)
				}
				if e = os.Chtimes(p, info.ModTime(), info.ModTime()); e != nil {
					t.Fatal(e)
				}
			case "canonical-root-link":
				saved := s.Root + "-saved"
				if e = os.Rename(s.Root, saved); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(saved, s.Root); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { os.Remove(s.Root); os.Rename(saved, s.Root) })
			case "cancel":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if e = selection.verify(ctx, raw); e == nil {
				t.Fatal("changed/ambiguous selection accepted")
			}
		})
	}
}

func TestOpenSelectionRejectsUnsafeStateAndExplicitConfigBound(t *testing.T) {
	for _, kind := range []string{"config-link", "state-link", "oversize", "exact-bound", "lock-link", "library-link"} {
		t.Run(kind, func(t *testing.T) {
			s, alias := aliasSelectionFixture(t)
			p := s.Path("state", "project.json")
			switch kind {
			case "config-link":
				if e := os.Rename(p, p+".saved"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(p+".saved", p); e != nil {
					t.Fatal(e)
				}
			case "state-link":
				state := s.Path("state")
				if e := os.Rename(state, state+"-saved"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(state+"-saved", state); e != nil {
					t.Fatal(e)
				}
			case "oversize", "exact-bound":
				raw, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				size := projectConfigMaxBytes
				if kind == "oversize" {
					size++
				}
				raw = append(raw, bytes.Repeat([]byte(" "), int(size)-len(raw))...)
				if e = os.WriteFile(p, raw, 0600); e != nil {
					t.Fatal(e)
				}
			case "lock-link":
				target := filepath.Join(t.TempDir(), "lock")
				if e := os.WriteFile(target, nil, 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, s.Path("state", "lock")); e != nil {
					t.Fatal(e)
				}
			case "library-link":
				target := t.TempDir()
				if e := os.WriteFile(filepath.Join(target, "data"), []byte("untrusted"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, s.Path("state", "evals")); e != nil {
					t.Fatal(e)
				}
			}
			opened, e := Open(alias)
			if kind == "exact-bound" || kind == "lock-link" || kind == "library-link" {
				if e != nil {
					t.Fatal("ordinary selected config refused", e)
				}
				if kind == "lock-link" {
					if lock, e := opened.TryLockStateContext(context.Background()); e == nil {
						if lock != nil {
							lock.Unlock()
						}
						t.Fatal("lock link accepted")
					}
				}
				if kind == "library-link" {
					if _, e = opened.ReadStateBounded(context.Background(), 32, "state", "evals", "data"); !errors.Is(e, ErrBoundedState) {
						t.Fatal("library link accepted", e)
					}
				}
				return
			}
			if e == nil {
				t.Fatal("unsafe or oversized config accepted")
			}
		})
	}
}
