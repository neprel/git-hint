//go:build unix

package trees

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
)

func selectedFixture(t *testing.T) (*SelectedSourceSession, string) {
	t.Helper()
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "present", "literal")
	testkit.WriteFile(t, dir, "missing", "deleted")
	testkit.WriteFile(t, dir, "nested/deleted", "deleted")
	testkit.Git(t, dir, "add", "present", "missing", "nested/deleted")
	if e := os.Remove(filepath.Join(dir, "missing")); e != nil {
		t.Fatal(e)
	}
	if e := os.RemoveAll(filepath.Join(dir, "nested")); e != nil {
		t.Fatal(e)
	}
	s, e := OpenSelectedSourceSession(context.Background(), lib(t), dir, SourceSessionOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}
func TestSelectedReceiptActualCoreLiteralFacts(t *testing.T) {
	s, dir := selectedFixture(t)
	// An untracked FIFO is outside the Git-selected artifact; this is not absence proof.
	if e := unix.Mkfifo(filepath.Join(dir, "unselected-fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r := c.Receipt()
	if r.Version != SelectedReceiptV1 || r.Pass != 1 || r.PresentCount != 1 || r.IndexedAbsentCount != 2 || r.UnindexedAbsentCount != 0 || len(r.Selected) != 3 || len(r.Stages) != 3 {
		t.Fatalf("unexpected facts: %+v", r)
	}
	if r.RootIdentity == "" || r.TreeDigest != c.TreeDigest() || r.SelectedInventoryDigest == "" || r.StageInventoryDigest == "" || r.EndOffsetNS < r.BeginOffsetNS || r.BeginOffsetNS < 0 {
		t.Fatal("receipt binding")
	}
	paths := []string{}
	for _, v := range r.Selected {
		paths = append(paths, v.Path)
		if !v.Indexed {
			t.Fatal("tracked missing flag")
		}
		if v.Path == "present" {
			if v.State != SelectedRegularPresent || v.Mode != "100644" || v.Size != 7 || v.ObjectDigest != "sha256:829f8d848b44fa3098194754af5b60e2fb1517b0195956841beb6cac9bc68067" || v.PathIdentity == "" || v.AbsenceParentIdentity != "" {
				t.Fatalf("present %+v", v)
			}
		} else {
			if v.State != SelectedAbsent || v.Mode != "" || v.Size != 0 || v.ObjectDigest != "" || v.PathIdentity != "" || v.AbsenceParentIdentity == "" {
				t.Fatalf("absent %+v", v)
			}
		}
	}
	if !reflect.DeepEqual(paths, []string{"missing", "nested/deleted", "present"}) {
		t.Fatal(paths)
	}
	tree, e := Load(s.library, c.TreeDigest())
	if e != nil || len(tree.Files) != 1 || tree.Files[0].Path != "present" {
		t.Fatal(tree, e)
	}
	second, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r2 := second.Receipt()
	if r2.Pass != 2 || r2.TreeDigest != r.TreeDigest || r2.SelectedInventoryDigest != r.SelectedInventoryDigest || r2.StageInventoryDigest != r.StageInventoryDigest {
		t.Fatal("matching views differ")
	}
	if _, e = s.Capture(context.Background()); e == nil {
		t.Fatal("third capture accepted")
	}
}
func TestSelectedReceiptCopiesAndChangedSecondView(t *testing.T) {
	s, dir := selectedFixture(t)
	a, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r := a.Receipt()
	r.Selected[0].Path = "mutated"
	r.Stages[0].Mode = "bad"
	if a.Receipt().Selected[0].Path != "missing" || a.Receipt().Stages[0].Mode != "100644" {
		t.Fatal("receipt alias")
	}
	testkit.WriteFile(t, dir, "missing", "new")
	b, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if b.TreeDigest() == a.TreeDigest() || b.Receipt().IndexedAbsentCount != 1 || b.Receipt().PresentCount != 2 {
		t.Fatal("second view hidden")
	}
	if a.Receipt().IndexedAbsentCount != 2 {
		t.Fatal("first view mutated")
	}
}
func TestSelectedReceiptHonestUnindexedAbsence(t *testing.T) {
	s, dir := selectedFixture(t)
	testkit.WriteFile(t, dir, "untracked", "temporary")
	s.hooks.afterInventory = func() {
		if e := os.Remove(filepath.Join(dir, "untracked")); e != nil {
			t.Fatal(e)
		}
	}
	c, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r := c.Receipt()
	if r.UnindexedAbsentCount != 1 || r.IndexedAbsentCount != 2 {
		t.Fatal(r)
	}
	for _, v := range r.Selected {
		if v.Path == "untracked" && (v.Indexed || v.State != SelectedAbsent || v.AbsenceParentIdentity == "") {
			t.Fatal(v)
		}
	}
}
func TestSelectedReceiptAbsenceReplacementRefuses(t *testing.T) {
	for _, mode := range []string{"appeared", "parent replaced", "missing subtree appeared", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, dir := selectedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			s.hooks.readHooks.beforeAbsenceVerify = func() {
				if called {
					return
				}
				called = true
				switch mode {
				case "appeared":
					testkit.WriteFile(t, dir, "missing", "appeared")
				case "parent replaced":
					old := dir + "-old"
					if e := os.Rename(dir, old); e != nil {
						t.Fatal(e)
					}
					t.Cleanup(func() { os.RemoveAll(dir); os.Rename(old, dir) })
					if e := os.Mkdir(dir, 0700); e != nil {
						t.Fatal(e)
					}
				case "missing subtree appeared":
					testkit.WriteFile(t, dir, "nested/deleted", "appeared")
				case "cancel":
					cancel()
				}
			}
			c, e := s.Capture(ctx)
			if e == nil || c != nil {
				t.Fatal("changed absence accepted", mode)
			}
			if _, e = s.Capture(context.Background()); e == nil {
				t.Fatal("failed pass retried")
			}
		})
	}
}
func TestSelectedReceiptContextsAlwaysTighten(t *testing.T) {
	for _, mode := range []string{"original cancelled", "caller cancelled", "earlier caller deadline", "original earlier deadline"} {
		t.Run(mode, func(t *testing.T) {
			dir := canonicalDir(t)
			testkit.Git(t, dir, "init", "-q")
			testkit.WriteFile(t, dir, "file", "x")
			l := lib(t)
			original, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "original earlier deadline" {
				var stop context.CancelFunc
				original, stop = context.WithTimeout(original, 200*time.Millisecond)
				defer stop()
			}
			s, e := OpenSelectedSourceSession(original, l, dir, SourceSessionOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			caller := context.Background()
			if mode == "original cancelled" {
				cancel()
			}
			if mode == "caller cancelled" {
				var stop context.CancelFunc
				caller, stop = context.WithCancel(caller)
				stop()
			}
			if mode == "earlier caller deadline" {
				var stop context.CancelFunc
				caller, stop = context.WithTimeout(caller, 200*time.Millisecond)
				defer stop()
			}
			if strings.Contains(mode, "deadline") {
				s.hooks.afterInventory = func() { <-original.Done() }
				if mode == "earlier caller deadline" {
					s.hooks.afterInventory = func() { <-caller.Done() }
				}
			}
			c, e := s.Capture(caller)
			if e == nil || c != nil {
				t.Fatal("context widened")
			}
			if _, e = s.Capture(context.Background()); e == nil {
				t.Fatal("retry after bound")
			}
		})
	}
	s, _ := selectedFixture(t)
	deadline, _ := s.ctx.Deadline()
	first, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if first == nil {
		t.Fatal("no capture")
	}
	after, _ := s.ctx.Deadline()
	if !deadline.Equal(after) {
		t.Fatal("deadline renewed")
	}
}
func TestSelectedReceiptConcurrentAndCloseRefuse(t *testing.T) {
	s, _ := selectedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.hooks.afterInventory = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() {
		c, e := s.Capture(context.Background())
		if c != nil {
			done <- errors.New("concurrent capture succeeded")
			return
		}
		done <- e
	}()
	<-entered
	if _, e := s.Capture(context.Background()); !isCode(e, "receipt_session_busy") {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e == nil {
		t.Fatal("busy session survived")
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Capture(context.Background()); e == nil {
		t.Fatal("closed session")
	}
	var zero SelectedSourceSession
	if e := zero.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := zero.Capture(context.Background()); e == nil {
		t.Fatal("zero session accepted")
	}
}
func TestSelectedReceiptBoundsAndStageFacts(t *testing.T) {
	s, _ := selectedFixture(t)
	s.limits = Limits{MaxFileBytes: 1, MaxTotalBytes: 2, MaxFiles: 3}
	if c, e := s.Capture(context.Background()); e == nil || c != nil {
		t.Fatal("partial success on limit")
	}
	for _, raw := range []string{"100644 " + strings.Repeat("a", 40) + " 0\tfile\x00" + "100755 " + strings.Repeat("b", 40) + " 0\tfile\x00", "bad\x00", strings.Repeat("100644 "+strings.Repeat("a", 40)+" 0\tfile\x00", 10001)} {
		r := receiptCapture{indexed: map[string]bool{}}
		if e := r.stages([]byte(raw)); e == nil {
			t.Fatal("invalid stage accepted")
		}
	}
	r := receiptCapture{indexed: map[string]bool{}}
	if e := r.stages([]byte("100644 " + strings.Repeat("a", 40) + " 1\tconflict\x00100755 " + strings.Repeat("b", 40) + " 2\tconflict\x00")); e != nil || len(r.value.Stages) != 2 || r.value.Stages[0].Stage != 1 || r.value.Stages[1].Stage != 2 {
		t.Fatal("conflict facts lost", e)
	}
}

func TestSelectedReceiptActualReadMutationHasNoPartialResult(t *testing.T) {
	for _, mode := range []string{"grow", "replace", "mode"} {
		t.Run(mode, func(t *testing.T) {
			s, dir := selectedFixture(t)
			s.hooks.readHooks.afterOpen = func() {
				p := filepath.Join(dir, "present")
				switch mode {
				case "grow":
					os.WriteFile(p, []byte("growing synthetic input"), 0600)
				case "replace":
					os.Rename(p, p+"-old")
					os.WriteFile(p, []byte("literal"), 0600)
				case "mode":
					os.Chmod(p, 0755)
				}
			}
			c, e := s.Capture(context.Background())
			if e == nil || c != nil {
				t.Fatal("mutated read succeeded")
			}
			if _, e = s.Capture(context.Background()); e == nil {
				t.Fatal("retry accepted")
			}
		})
	}
}
func TestSelectedReceiptModesAndIgnoredScope(t *testing.T) {
	s, dir := selectedFixture(t)
	testkit.WriteFile(t, dir, ".git/info/exclude", "ignored-secret\n")
	testkit.WriteFile(t, dir, "ignored-secret", "local private synthetic content")
	a, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range a.Receipt().Selected {
		if v.Path == "ignored-secret" {
			t.Fatal("ignored path observed")
		}
	}
	if e = os.Chmod(filepath.Join(dir, "present"), 0755); e != nil {
		t.Fatal(e)
	}
	b, e := s.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if b.TreeDigest() == a.TreeDigest() {
		t.Fatal("mode hidden")
	}
	r := b.Receipt()
	if r.Selected[2].Mode != "100755" || r.Stages[2].Mode != "100644" {
		t.Fatal("worktree/index mode domains conflated")
	}
}
func TestSelectedReceiptNoRenewalAfterSuccessfulFirstPass(t *testing.T) {
	old, dir := selectedFixture(t)
	old.Close()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, e := OpenSelectedSourceSession(parent, old.library, dir, SourceSessionOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Capture(context.Background()); e != nil {
		t.Fatal(e)
	}
	cancel()
	c, e := s.Capture(context.Background())
	if !errors.Is(e, context.Canceled) || c != nil {
		t.Fatal("second call widened original bound", e)
	}
	if s.attempts != 2 {
		t.Fatal("attempt accounting")
	}
}
func TestSelectedReceiptSessionParityWithExistingStrictCore(t *testing.T) {
	s, dir := selectedFixture(t)
	want, e := FromWorktreeWithCapturePolicy(context.Background(), s.library, dir, Limits{}, StrictCapturePolicyV1)
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.Capture(context.Background())
	if e != nil || c.TreeDigest() != want {
		t.Fatal("strict content changed", e)
	}
	if e = os.Symlink("present", filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if c, e = s.Capture(context.Background()); !isCode(e, "symlink_unsupported") || c != nil {
		t.Fatal("session weakened strict link rule", e)
	}
}

func TestSelectedReceiptMalformedStageThroughActualCaptureCore(t *testing.T) {
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"duplicate", "malformed", "cut"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := selectedFixture(t)
			bin := canonicalDir(t)
			output := "malformed\\000"
			if mode == "duplicate" {
				output = "100644 " + strings.Repeat("a", 40) + " 0\\tpresent\\000100644 " + strings.Repeat("a", 40) + " 0\\tpresent\\000"
			}
			if mode == "cut" {
				output = "100644 " + strings.Repeat("a", 40) + " 0\\tpresent"
			}
			quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
			script := "#!/bin/sh\nfor arg do if [ \"$arg\" = \"--stage\" ]; then printf " + quote(output) + "; exit 0; fi; done\nexec " + quote(realGit) + " \"$@\"\n"
			if e = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0755); e != nil {
				t.Fatal(e)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if c, e := s.Capture(context.Background()); !isCode(e, "receipt_stage_invalid") || c != nil {
				t.Fatal("stage accepted or wrong failure", mode, e)
			}
		})
	}
}
func TestSelectedReceiptCloseFailureIsStatic(t *testing.T) {
	s, _ := selectedFixture(t)
	if e := s.source.anchor.Close(); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); !isCode(e, "receipt_close_failed") {
		t.Fatal(e)
	}
	if _, e := s.Capture(context.Background()); e == nil {
		t.Fatal("capture after bad close")
	}
}

func TestSelectedReceiptFramingBoundBeforeSplit(t *testing.T) {
	r := receiptCapture{indexed: map[string]bool{}}
	for _, raw := range [][]byte{[]byte{0}, []byte("file\x00\x00"), []byte("cut"), []byte("a\x00b\x00")} {
		if e := r.selection(raw, 1); e == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	if e := r.selection([]byte("one\x00"), 1); e != nil {
		t.Fatal(e)
	}
	if e := r.selection(nil, 1); e != nil {
		t.Fatal(e)
	}
	if e := r.stages([]byte{0}); e == nil {
		t.Fatal("empty stage accepted")
	}
}
