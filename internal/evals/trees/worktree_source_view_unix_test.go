//go:build unix

package trees

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
)

func sourceViewFixture(t *testing.T) (*SelectedSourceView, string) {
	t.Helper()
	installed, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	installed, e = filepath.EvalSymlinks(installed)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", filepath.Dir(installed)+string(os.PathListSeparator)+"/usr/bin:/bin")
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") || strings.HasPrefix(strings.ToUpper(key), "LD_") || strings.HasPrefix(strings.ToUpper(key), "DYLD_") {
			k, v := key, value
			t.Cleanup(func() { os.Setenv(k, v) })
			os.Unsetenv(key)
		}
	}
	root := canonicalDir(t)
	home := canonicalDir(t)
	xdg := canonicalDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, ".gitignore", ".hint/\n")
	testkit.WriteFile(t, root, "nested/input", "literal actual source")
	testkit.WriteFile(t, root, "gone", "deleted")
	testkit.Git(t, root, "add", ".gitignore", "nested/input", "gone")
	testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "-qm", "synthetic")
	if e := os.Remove(filepath.Join(root, "gone")); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	syntheticStore, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	l := library.Open(syntheticStore, 0)
	v, e := OpenSelectedSourceView(context.Background(), l, root, SourceViewOptions{})
	if e != nil {
		t.Fatalf("actual installed Git/system source profile unsupported: %v", e)
	}
	t.Cleanup(func() { v.Close() })
	return v, root
}
func TestSourceViewActualInstalled256Positive(t *testing.T) {
	v, _ := sourceViewFixture(t)
	a, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r := a.Sources()
	if r.Version != SourceViewVersion || r.Authority != "unknown" || r.State != "observed_source_inputs" || r.ChainTiming != "discovered_after_selection" || r.Pass != 1 || len(r.Facts) == 0 {
		t.Fatal("source receipt")
	}
	if r.SourceQueryAttempts != 24 || r.TotalDispatchAttempts <= r.SourceQueryAttempts || r.PersistenceGuardAttempts != r.TotalDispatchAttempts-r.SourceQueryAttempts || r.ExecutableReads != r.TotalDispatchAttempts+1 {
		t.Fatalf("counter scope %d %d %d %d", r.SourceQueryAttempts, r.TotalDispatchAttempts, r.PersistenceGuardAttempts, r.ExecutableReads)
	}
	b, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r2 := b.Sources()
	if r2.SourceQueryAttempts != 38 || r2.ChainTiming != "observed_before_selection" || r2.TreeDigest != r.TreeDigest || r2.SourceDigest != r.SourceDigest || r2.Pass != 2 {
		t.Fatal("repeat source receipt")
	}
	if r2.MetadataBytes > 4<<20 || r2.MetadataReads > 1024 || r2.GuardStderrBytes != 0 || r2.SourceStderrBytes != 0 {
		t.Fatal("bounds")
	}
	if a.Selected().TreeDigest() != r.TreeDigest || r.BeginOffsetNS < a.Selected().Receipt().BeginOffsetNS || r.EndOffsetNS > int64(strictCaptureSeconds) {
		t.Fatal("shared private anchor")
	}
	t.Logf("actual Git2.56 source positive: source queries=%d, guards=%d, executable reads=%d; no authority", r2.SourceQueryAttempts, r2.PersistenceGuardAttempts, r2.ExecutableReads)
}

func TestSourceViewCopiesLiteralFactsAndLegacyParity(t *testing.T) {
	v, root := sourceViewFixture(t)
	c, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r := c.Sources()
	roles := map[string]int{}
	for _, f := range r.Facts {
		roles[f.Role]++
		if f.State == "present_regular" && (f.ObjectDigest == "" || f.NodeIdentity == "" || f.AbsenceIdentity != "") {
			t.Fatal("present fact")
		}
		if f.State == "observed_absent" && (f.AbsenceIdentity == "" || f.ObjectDigest != "" || f.Bytes != 0) {
			t.Fatal("absent fact")
		}
	}
	if roles["system_config"] != 1 || roles["global_config"] != 2 || roles["repo_config"] != 1 || roles["global_exclude"] != 1 || roles["local_exclude"] != 1 || roles["selected_chain_gitignore"] != 2 {
		t.Fatal("literal source roles", roles)
	}
	r.Facts[0].Path = "mutation"
	r.CandidatePaths[0] = "mutation"
	q := c.Selected().Receipt()
	q.Selected[0].Path = "mutation"
	if c.Sources().Facts[0].Path == "mutation" || c.Sources().CandidatePaths[0] == "mutation" || c.Selected().Receipt().Selected[0].Path == "mutation" {
		t.Fatal("copied receipt aliases")
	}
	legacy, e := FromWorktree(context.Background(), v.session.library, root, Limits{})
	if e != nil || legacy != c.Selected().TreeDigest() {
		t.Fatal("legacy tree bytes changed", e)
	}
	strict, e := FromWorktreeWithCapturePolicy(context.Background(), v.session.library, root, Limits{}, StrictCapturePolicyV1)
	if e != nil || strict != legacy {
		t.Fatal("strict bytes changed", e)
	}
	if _, e = v.Capture(context.Background()); e != nil {
		t.Fatal(e)
	}
	if out, e := v.Capture(context.Background()); e == nil || out != nil {
		t.Fatal("third attempt")
	}
}

func TestSourceViewChangesRefuseWithoutPartialReceipt(t *testing.T) {
	for _, name := range []string{"global", "ignore", "index", "head", "selected", "selected_chain_link", "same_bytes_new_node"} {
		t.Run(name, func(t *testing.T) {
			v, root := sourceViewFixture(t)
			if _, e := v.Capture(context.Background()); e != nil {
				t.Fatalf("initial production capture refused at static phase %s: %v", v.checkPhase, e)
			}
			switch name {
			case "global":
				testkit.WriteFile(t, v.home, ".gitconfig", "[safe]\n directory = /synthetic\n")
			case "ignore":
				testkit.WriteFile(t, root, "nested/.gitignore", "nothing\n")
			case "index":
				testkit.Git(t, root, "update-index", "--chmod=+x", "nested/input")
			case "head":
				testkit.Git(t, root, "symbolic-ref", "HEAD", "refs/heads/other")
			case "selected":
				testkit.WriteFile(t, root, "nested/input", "changed")
			case "same_bytes_new_node":
				if e := os.Rename(filepath.Join(root, "nested/input"), filepath.Join(root, "nested/moved")); e != nil {
					t.Fatal(e)
				}
				testkit.WriteFile(t, root, "nested/input", "literal actual source")
				if e := os.Remove(filepath.Join(root, "nested/moved")); e != nil {
					t.Fatal(e)
				}
			case "selected_chain_link":
				if e := os.Symlink("/dev/null", filepath.Join(root, "nested/.gitignore")); e != nil {
					t.Fatal(e)
				}
			}
			if out, e := v.Capture(context.Background()); e == nil || out != nil {
				t.Fatal("changed source returned a receipt")
			}
			if out, e := v.Capture(context.Background()); e == nil || out != nil {
				t.Fatal("failed view reused")
			}
		})
	}
}

func TestSourceViewConfigProfileAndRawFraming(t *testing.T) {
	// Literal v2.56 list framing: scope NUL origin NUL key[LFvalue] NUL.
	raw := []byte("global\x00file:/synthetic/home/.gitconfig\x00custom.bare\x00global\x00file:/synthetic/home/.gitconfig\x00custom.empty\n\x00global\x00file:/synthetic/home/.gitconfig\x00custom.multi\nfirst\nsecond\x00")
	got, e := parseConfig(raw)
	want := []configTuple{{"global", "file:/synthetic/home/.gitconfig", "custom.bare", "", false}, {"global", "file:/synthetic/home/.gitconfig", "custom.empty", "", true}, {"global", "file:/synthetic/home/.gitconfig", "custom.multi", "first\nsecond", true}}
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("source framing differs")
	}
	for _, bad := range [][]byte{raw[:len(raw)-1], []byte("global\x00file:x\x00\x00"), []byte("scope\x00origin\x00key\x00tail"), bytes.Repeat([]byte("x\x00x\x00x\x00"), 8193)} {
		if _, e := parseConfig(bad); e == nil {
			t.Fatal("bad framing")
		}
	}
	v := &SelectedSourceView{root: "/synthetic/repo", gitdir: "/synthetic/repo/.git", candidates: []string{"/synthetic/system/gitconfig", "/synthetic/xdg/git/config", "/synthetic/home/.gitconfig"}}
	fixed := []configTuple{{"command", "command line:", "core.fsmonitor", "false", true}, {"command", "command line:", "core.hookspath", "/dev/null", true}}
	if e := v.validateConfig(fixed); e != nil {
		t.Fatal(e)
	}
	for _, row := range []configTuple{{"local", "file:.git/config", "include.path", "/synthetic/include", true}, {"local", "file:.git/config", "includeIf.foo.path", "/x", true}, {"local", "file:.git/config", "extensions.worktreeconfig", "false", true}, {"local", "file:.git/config", "core.worktree", "/x", true}, {"local", "file:.git/config", "core.sparsecheckout", "true", true}, {"local", "file:.git/config", "core.excludesfile", "relative", true}, {"global", "file:/not-discovered", "x.y", "z", true}, {"worktree", "file:.git/config", "x.y", "z", true}} {
		if e := v.validateConfig(append(append([]configTuple{}, fixed...), row)); e == nil {
			t.Fatal("unsupported config", row.Key)
		}
	}
	if _, e := sourceLines([]byte("/a\n/b\n"), 1); e == nil {
		t.Fatal("multiple path candidates")
	}
	if _, e := sourceLines([]byte("/a\n"), 1); e != nil {
		t.Fatal(e)
	}
}

func TestSourceViewActualConfigSourcesAndProfileRefusal(t *testing.T) {
	for _, name := range []string{"tilde_excludes", "include", "relative_excludes", "config_symlink", "worktree_config"} {
		t.Run(name, func(t *testing.T) {
			v, root := sourceViewFixture(t)
			l := v.session.library
			if e := v.Close(); e != nil {
				t.Fatal(e)
			}
			switch name {
			case "tilde_excludes":
				testkit.WriteFile(t, v.home, ".gitconfig", "[core]\n excludesFile = ~/ignore\n")
				testkit.WriteFile(t, v.home, "ignore", "ignored\n")
				testkit.WriteFile(t, root, "ignored", "not selected")
			case "include":
				testkit.Git(t, root, "config", "include.path", "/synthetic/missing")
			case "relative_excludes":
				testkit.Git(t, root, "config", "core.excludesFile", "relative")
			case "config_symlink":
				if e := os.Symlink("/dev/null", filepath.Join(v.home, ".gitconfig")); e != nil {
					t.Fatal(e)
				}
			case "worktree_config":
				testkit.WriteFile(t, root, ".git/config.worktree", "")
			}
			next, e := OpenSelectedSourceView(context.Background(), l, root, SourceViewOptions{})
			if name != "tilde_excludes" {
				if e == nil {
					next.Close()
					t.Fatal("unsupported source admitted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer next.Close()
			c, e := next.Capture(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, f := range c.Sources().Facts {
				if f.Role == "global_exclude" && f.Path == filepath.Join(v.home, "ignore") && f.State == "present_regular" {
					found = true
				}
			}
			if !found {
				t.Fatal("Git-expanded path missing")
			}
			for _, f := range c.Selected().Receipt().Selected {
				if f.Path == "ignored" {
					t.Fatal("effective exclude not used")
				}
			}
		})
	}
}

func TestSourceViewOriginalContextAndCaps(t *testing.T) {
	v, _ := sourceViewFixture(t)
	deadline, _ := v.session.ctx.Deadline()
	if deadline.Sub(v.session.anchor) > strictCaptureSeconds {
		t.Fatal("renewed discovery bound")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, e := v.Capture(ctx); e == nil || c != nil {
		t.Fatal("cancelled caller")
	}
	if c, e := v.Capture(context.Background()); e == nil || c != nil {
		t.Fatal("cancelled view reused")
	}
	t.Run("read_cap", func(t *testing.T) {
		v, _ := sourceViewFixture(t)
		v.reads[0] = 1024
		if c, e := v.Capture(context.Background()); e == nil || c != nil {
			t.Fatal("metadata read cap")
		}
	})
	t.Run("byte_cap", func(t *testing.T) {
		v, _ := sourceViewFixture(t)
		v.readBytes[0] = sourceMetadataCap + 1
		if c, e := v.Capture(context.Background()); e == nil || c != nil {
			t.Fatal("metadata byte cap")
		}
	})
	t.Run("query_cap", func(t *testing.T) {
		v, _ := sourceViewFixture(t)
		v.queries = 38
		before := v.invocation.Stats()
		if c, e := v.Capture(context.Background()); e == nil || c != nil {
			t.Fatal("query cap")
		}
		if v.invocation.Stats().Dispatches != before.Dispatches {
			t.Fatal("dispatch after source cap")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		v, _ := sourceViewFixture(t)
		v.session.cancel()
		if c, e := v.Capture(context.Background()); e == nil || c != nil {
			t.Fatal("expired original context")
		}
	})
}

func TestSourceViewFrozenPersistenceGuardsAndLegacyControl(t *testing.T) {
	v, _ := sourceViewFixture(t)
	frozen := v.invocation
	// Existing object writes call readgit.Output transitively. Ambient mutation must
	// not change their scoped executable or HOME; unscoped callers stay legacy.
	t.Setenv("PATH", "/synthetic/missing-bin")
	t.Setenv("HOME", "/synthetic/unused-home")
	before := frozen.Stats()
	if _, e := v.session.library.PutObjectContext(v.session.ctx, []byte("literal scoped object")); e != nil {
		t.Fatal(e)
	}
	after := frozen.Stats()
	if after.Dispatches <= before.Dispatches || after.ExecutableReads <= before.ExecutableReads {
		t.Fatal("actual object guard bypassed invocation")
	}
	if _, e := v.session.library.PutObjectContext(context.Background(), []byte("literal legacy object")); e == nil {
		t.Fatal("legacy unexpectedly used scope")
	}
	if c, e := v.Capture(context.Background()); e != nil || c == nil {
		t.Fatal("source view lost frozen dispatch", e)
	}
	if _, e := readgit.Output(context.Background(), v.root, 1024, "--version"); e == nil {
		t.Fatal("legacy command ambient path unchanged")
	}
}

func TestSourceViewCancellationAndConcurrentClose(t *testing.T) {
	v, _ := sourceViewFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	v.session.hooks.afterInventory = func() { close(entered); <-release }
	type answer struct {
		c *SelectedSourceViewCapture
		e error
	}
	done := make(chan answer, 1)
	go func() { c, e := v.Capture(context.Background()); done <- answer{c, e} }()
	<-entered
	if c, e := v.Capture(context.Background()); e == nil || c != nil {
		t.Fatal("concurrent source capture admitted")
	}
	close(release)
	out := <-done
	if out.e == nil || out.c != nil {
		t.Fatal("cancelled active capture returned partial success")
	}
	if e := v.Close(); e != nil {
		t.Fatal(e)
	}
	if c, e := v.Capture(context.Background()); e == nil || c != nil {
		t.Fatal("closed view reused")
	}
}

func TestSourceViewObjectAlternatesRefuse(t *testing.T) {
	for _, name := range []string{"alternates", "http-alternates"} {
		for _, moment := range []string{"before_open", "before_first", "between_passes"} {
			for _, shape := range []string{"regular", "empty", "link", "directory", "fifo", "ancestor_link"} {
				t.Run(name+"/"+moment+"/"+shape, func(t *testing.T) {
					v, root := sourceViewFixture(t)
					l := v.session.library
					if moment == "before_open" {
						if e := v.Close(); e != nil {
							t.Fatal(e)
						}
					}
					if moment == "between_passes" {
						if _, e := v.Capture(context.Background()); e != nil {
							t.Fatal(e)
						}
					}
					path := filepath.Join(root, ".git", "objects", "info", name)
					switch shape {
					case "regular":
						if e := os.WriteFile(path, []byte("/synthetic/object-source\n"), 0600); e != nil {
							t.Fatal(e)
						}
					case "empty":
						if e := os.WriteFile(path, nil, 0600); e != nil {
							t.Fatal(e)
						}
					case "link":
						if e := os.Symlink("/dev/null", path); e != nil {
							t.Fatal(e)
						}
					case "directory":
						if e := os.Mkdir(path, 0700); e != nil {
							t.Fatal(e)
						}
					case "fifo":
						if e := unix.Mkfifo(path, 0600); e != nil {
							t.Fatal(e)
						}
					case "ancestor_link":
						if e := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-moved"); e != nil {
							t.Fatal(e)
						}
						if e := os.Symlink(filepath.Base(filepath.Dir(path))+"-moved", filepath.Dir(path)); e != nil {
							t.Fatal(e)
						}
					}
					if moment == "before_open" {
						next, e := OpenSelectedSourceView(context.Background(), l, root, SourceViewOptions{})
						if e == nil {
							next.Close()
							t.Fatal("unsupported alternates profile admitted at open")
						}
						return
					}
					if c, e := v.Capture(context.Background()); e == nil || c != nil {
						t.Fatal("unsupported alternates profile returned receipt")
					}
				})
			}
		}
	}
}

func TestSourceViewObjectAlternatesActualAbsentFacts(t *testing.T) {
	v, _ := sourceViewFixture(t)
	for pass := 0; pass < 2; pass++ {
		c, e := v.Capture(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		found := map[string]bool{}
		for _, f := range c.Sources().Facts {
			if f.Path == filepath.Join(v.gitdir, "objects/info/alternates") || f.Path == filepath.Join(v.gitdir, "objects/info/http-alternates") {
				if f.Role != "git_state" || f.State != "observed_absent" || f.AbsenceIdentity == "" || f.NodeIdentity != "" || f.ObjectDigest != "" || f.Bytes != 0 {
					t.Fatal("alternate absence fact malformed")
				}
				found[filepath.Base(f.Path)] = true
			}
		}
		if !reflect.DeepEqual(found, map[string]bool{"alternates": true, "http-alternates": true}) {
			t.Fatal("missing required alternate absence")
		}
	}
}
