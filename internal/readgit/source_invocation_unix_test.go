//go:build unix

package readgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sourceExecutableFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	script := []byte("#!/bin/sh\nprintf '%s' \"$HOME\"\n")
	if e := os.WriteFile(filepath.Join(dir, "git"), script, 0700); e != nil {
		t.Fatal(e)
	}
	return dir, []string{"PATH=" + dir, "HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "UNRETAINED_SECRET=synthetic"}
}
func TestSourceInvocationSharedCoreCopiesAndScope(t *testing.T) {
	dir, base := sourceExecutableFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inv, e := FreezeSourceInvocation(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	base[0] = "PATH=/synthetic/missing"
	copyEnv := inv.Environment()
	copyEnv[0] = "mutated"
	if strings.Contains(strings.Join(inv.Environment(), "\n"), "SECRET") || reflect.DeepEqual(copyEnv, inv.Environment()) {
		t.Fatal("environment alias or leak")
	}
	t.Setenv("PATH", "/synthetic/missing")
	t.Setenv("HOME", "/synthetic/ambient")
	raw, e := OutputWithSourceInvocation(ctx, inv, dir, 1024, "--version")
	if e != nil || string(raw) != dir {
		t.Fatal("explicit invocation", e)
	}
	raw, e = Output(WithSourceInvocation(ctx, inv), dir, 1024, "--version")
	if e != nil || string(raw) != dir {
		t.Fatal("same scoped core", e)
	}
	if raw, e = Output(ctx, dir, 1024, "--version"); e == nil || raw != nil {
		t.Fatal("legacy used scoped invocation")
	}
	stats := inv.Stats()
	if stats.Dispatches != 2 || stats.ExecutableReads != 3 || stats.StdoutBytes != 2*int64(len(dir)) || stats.StderrBytes != 0 || stats.ExecutableBytes == 0 {
		t.Fatal("actual accounting", stats)
	}
}
func TestSourceInvocationRefusesExecutableReplacementAndCaps(t *testing.T) {
	for _, kind := range []string{"bytes", "link", "cap", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			dir, env := sourceExecutableFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inv, e := FreezeSourceInvocation(ctx, env)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "bytes":
				if e := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
					t.Fatal(e)
				}
			case "link":
				if e := os.Remove(filepath.Join(dir, "git")); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink("/bin/sh", filepath.Join(dir, "git")); e != nil {
					t.Fatal(e)
				}
			case "cap":
				inv.stats.Dispatches = SourceDispatchLimit
			case "cancel":
				cancel()
			}
			before := inv.Stats().Dispatches
			if b, e := OutputWithSourceInvocation(ctx, inv, dir, 1024, "--version"); e == nil || b != nil {
				t.Fatal("invalid executable dispatched")
			}
			if inv.Stats().Dispatches != before {
				t.Fatal("dispatch after admission refusal")
			}
		})
	}
}
func TestSourceInvocationAdmissionSelectors(t *testing.T) {
	_, env := sourceExecutableFixture(t)
	for _, extra := range []string{"GIT_CONFIG_NOSYSTEM=1", "git_config_count=1", "LD_PRELOAD=/x", "DYLD_INSERT_LIBRARIES=/x", "HOME=/duplicate", "PATH=relative"} {
		t.Run(strings.Split(extra, "=")[0], func(t *testing.T) {
			if i, e := FreezeSourceInvocation(context.Background(), append(append([]string{}, env...), extra)); e == nil || i != nil {
				t.Fatal("unresolved source selector admitted")
			}
		})
	}
	if i, e := FreezeSourceInvocation(nil, env); e == nil || i != nil {
		t.Fatal("nil context")
	}
}

// The artifact control cwd sits below the fresh ceiling directory. Even inside
// an ancestor repository, Git must not discover it; the Native profile would.
// A ceiling is a Git discovery bound, not a kernel sandbox.
func TestArtifactSourceInvocationCeilingStopsDiscovery(t *testing.T) {
	installed, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	installed, e = filepath.EvalSymlinks(installed)
	if e != nil {
		t.Fatal(e)
	}
	parent, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	init := exec.Command(installed, "init", "-q", parent)
	init.Env = []string{"PATH=" + filepath.Dir(installed), "HOME=" + parent, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if out, e := init.CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	control := filepath.Join(parent, "control")
	for _, d := range []string{"home", "xdg", "cwd"} {
		if e := os.MkdirAll(filepath.Join(control, d), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(control, "global"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	base := []string{"PATH=" + filepath.Dir(installed), "HOME=" + parent}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	artifact, e := FreezeArtifactSourceInvocation(ctx, base, ArtifactSourceControl{Home: filepath.Join(control, "home"), XDG: filepath.Join(control, "xdg"), Global: filepath.Join(control, "global"), Ceiling: control})
	if e != nil {
		t.Fatal(e)
	}
	native, e := FreezeSourceInvocation(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	cwd := filepath.Join(control, "cwd")
	if out, e := OutputWithSourceInvocation(ctx, native, cwd, 4096, "rev-parse", "--absolute-git-dir"); e != nil || string(out) != filepath.Join(parent, ".git")+"\n" {
		t.Fatal("control: Native profile should discover the ancestor", string(out), e)
	}
	if out, e := OutputWithSourceInvocation(ctx, artifact, cwd, 4096, "rev-parse", "--absolute-git-dir"); e == nil || len(out) != 0 {
		t.Fatal("artifact control cwd discovered an ancestor repository", string(out))
	}
	if artifact.Identity() == native.Identity() || !strings.Contains(strings.Join(artifact.Environment(), "\n"), "GIT_CONFIG_NOSYSTEM=1") {
		t.Fatal("artifact identity/profile")
	}
	for _, kv := range artifact.Environment() {
		if kv == "HOME="+parent {
			t.Fatal("caller HOME selected by artifact profile")
		}
	}
}
