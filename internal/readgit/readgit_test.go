package readgit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/testkit"
)

func TestReadGitArgsAndEnvironment(t *testing.T) {
	want := []string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", "/synthetic/root", "ls-files", "-z"}
	if got := Args("/synthetic/root", "ls-files", "-z"); !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe command argv: %q", got)
	}
	base := []string{"PATH=/synthetic/bin", "HOME=/synthetic/home", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/synthetic/global", "GIT_CONFIG_PARAMETERS=core.fsmonitor=evil", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=evil", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=evil", "git_config_count=1", "GIT_OPTIONAL_LOCKS=1"}
	copyBase := append([]string(nil), base...)
	got := Env(base)
	expected := []string{"PATH=/synthetic/bin", "HOME=/synthetic/home", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/synthetic/global", "GIT_OPTIONAL_LOCKS=0"}
	if !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(base, copyBase) {
		t.Fatalf("env not safely copied: %q", got)
	}
}
func TestReadGitGuardDoesNotRenewDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	guarded, done := Guard(ctx)
	defer done()
	nested, doneNested := Guard(guarded)
	defer doneNested()
	a, _ := ctx.Deadline()
	b, _ := nested.Deadline()
	if !a.Equal(b) {
		t.Fatal("caller deadline renewed")
	}
	fallback, end := Guard(context.Background())
	defer end()
	d, ok := fallback.Deadline()
	if !ok || time.Until(d) > FallbackTimeout {
		t.Fatal("unbounded legacy fallback")
	}
}
func TestReadGitCompleteExitOneAndFailures(t *testing.T) {
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	out, e := Output(context.Background(), root, 1024, "check-ignore", "--no-index", "-v", "-n", "--", "not-ignored")
	if !ExitOne(e) || !strings.Contains(string(out), "not-ignored") {
		t.Fatalf("complete nonmatch lost: %q %v", out, e)
	}
	if out, e = Output(context.Background(), root, 1, "rev-parse", "--show-toplevel"); e == nil || len(out) != 0 {
		t.Fatal("bounded prefix accepted")
	}
	if out, e = Output(context.Background(), root, 1024, "not-a-git-command"); e == nil || len(out) != 0 {
		t.Fatal("failed output accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, e = Output(ctx, root, 1024, "rev-parse", "--show-toplevel"); !errors.Is(e, context.Canceled) || len(out) != 0 {
		t.Fatal("cancelled prefix accepted")
	}
}
