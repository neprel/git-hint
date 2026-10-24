//go:build unix

package trees

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
)

func canonicalDir(t *testing.T) string {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func sourceFixture(t *testing.T) (*worktreeRoot, string) {
	t.Helper()
	d := canonicalDir(t)
	testkit.WriteFile(t, d, "nested/file", "original")
	s, e := openWorktreeRoot(d, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.close)
	return s, d
}

func TestSharedReaderLegacyGolden(t *testing.T) {
	l, dir := lib(t), repo(t)
	dir, e := filepath.EvalSymlinks(dir)
	if e != nil {
		t.Fatal(e)
	}
	d, e := FromWorktree(context.Background(), l, dir, Limits{})
	if e != nil {
		t.Fatal(e)
	}
	const golden = "sha256:fdcebb051070364be786fc5c29bad5bed82de61c99aae7cdee710a0702d1f1d0"
	if d != golden {
		t.Fatalf("legacy tree changed: %s", d)
	}
	if _, e = FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1); !isCode(e, "symlink_unsupported") {
		t.Fatalf("strict symlink: %v", e)
	}
	if e = os.Remove(filepath.Join(dir, "link.go")); e != nil {
		t.Fatal(e)
	}
	a, e := FromWorktree(context.Background(), l, dir, Limits{})
	if e != nil {
		t.Fatal(e)
	}
	b, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1)
	if e != nil || a != b {
		t.Fatalf("regular tree differs: %s/%s %v", a, b, e)
	}
}

func TestDescriptorReaderDeterministicMutations(t *testing.T) {
	for _, kind := range []string{"replace before open", "symlink before open", "fifo before open", "replace after open", "grow after open", "shrink after open", "mode after open", "ancestor after open", "root after open", "cancel after open"} {
		t.Run(kind, func(t *testing.T) {
			s, d := sourceFixture(t)
			name := filepath.Join(d, "nested/file")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutate := func() {
				switch kind {
				case "replace before open", "replace after open":
					if e := os.Rename(name, name+"-old"); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(name, []byte("original"), 0o644); e != nil {
						t.Fatal(e)
					}
				case "symlink before open":
					os.Remove(name)
					if e := os.Symlink("file-target", name); e != nil {
						t.Fatal(e)
					}
				case "fifo before open":
					os.Remove(name)
					if e := unix.Mkfifo(name, 0o600); e != nil {
						t.Fatal(e)
					}
				case "grow after open":
					if e := os.WriteFile(name, []byte(strings.Repeat("g", 100)), 0o644); e != nil {
						t.Fatal(e)
					}
				case "shrink after open":
					if e := os.WriteFile(name, []byte("short"), 0o644); e != nil {
						t.Fatal(e)
					}
				case "mode after open":
					if e := os.Chmod(name, 0o755); e != nil {
						t.Fatal(e)
					}
				case "ancestor after open":
					p := filepath.Join(d, "nested")
					if e := os.Rename(p, p+"-old"); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(p+"-old", p); e != nil {
						t.Fatal(e)
					}
				case "root after open":
					if e := os.Rename(d, d+"-old"); e != nil {
						t.Fatal(e)
					}
					t.Cleanup(func() { os.Remove(d); os.Rename(d+"-old", d) })
					if e := os.Symlink(d+"-old", d); e != nil {
						t.Fatal(e)
					}
				case "cancel after open":
					cancel()
				}
			}
			h := readHooks{afterOpen: mutate}
			if strings.HasSuffix(kind, "before open") {
				h = readHooks{beforeOpen: mutate}
			}
			if _, e := s.read(ctx, "nested/file", 16, true, h); e == nil {
				t.Fatal("mutated source accepted")
			}
		})
	}
}

func TestDescriptorReaderRootAndAncestorSymlinks(t *testing.T) {
	s, d := sourceFixture(t)
	alias := filepath.Join(canonicalDir(t), "alias")
	if e := os.Symlink(d, alias); e != nil {
		t.Fatal(e)
	}
	if _, e := openWorktreeRoot(alias, true); e == nil {
		t.Fatal("root symlink accepted")
	}
	parentAlias := filepath.Join(canonicalDir(t), "parent")
	if e := os.Symlink(filepath.Dir(d), parentAlias); e != nil {
		t.Fatal(e)
	}
	if _, e := openWorktreeRoot(filepath.Join(parentAlias, filepath.Base(d)), true); e == nil {
		t.Fatal("ancestor symlink accepted")
	}
	if e := os.Symlink("nested", filepath.Join(d, "alias")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.read(context.Background(), "alias/file", 16, true, readHooks{}); e == nil {
		t.Fatal("inner ancestor symlink accepted")
	}
}

func TestDescriptorReaderLimitsEOFAndModes(t *testing.T) {
	s, d := sourceFixture(t)
	if _, e := s.read(context.Background(), "nested/file", 7, true, readHooks{}); !isCode(e, "binary_too_large") {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(d, "nested/file"), 0o755); e != nil {
		t.Fatal(e)
	}
	entry, e := s.read(context.Background(), "nested/file", 8, true, readHooks{})
	if e != nil || string(entry.data) != "original" || entry.mode != "100755" {
		t.Fatalf("%+v %v", entry, e)
	}
	entry, e = s.read(context.Background(), "nested/file", math.MaxInt64, true, readHooks{})
	if e != nil || len(entry.data) != 8 {
		t.Fatal(e)
	}
	if e = os.RemoveAll(filepath.Join(d, "nested")); e != nil {
		t.Fatal(e)
	}
	entry, e = s.read(context.Background(), "nested/file", 16, true, readHooks{})
	if e != nil || !entry.absent {
		t.Fatalf("deleted subtree: %+v %v", entry, e)
	}
}

func TestDescriptorReaderFIFOAndReadErrors(t *testing.T) {
	s, d := sourceFixture(t)
	if e := unix.Mkfifo(filepath.Join(d, "fifo"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.read(context.Background(), "fifo", 16, true, readHooks{}); !isCode(e, "special_file_unsupported") {
		t.Fatal(e)
	}
	legacy, e := s.read(context.Background(), "fifo", 16, false, readHooks{})
	if e != nil || !legacy.skip {
		t.Fatal(e)
	}
	f, e := os.Open(filepath.Join(d, "nested/file"))
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = readRegular(context.Background(), f, 16, readHooks{}); e == nil {
		t.Fatal("closed descriptor read accepted")
	}
}

func TestStrictCapturePolicyRefusals(t *testing.T) {
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "file", "content")
	l := lib(t)
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, CapturePolicy("unreviewed")); !isCode(e, "capture_policy_unknown") {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := FromWorktreeWithCapturePolicy(ctx, l, dir, Limits{}, StrictCapturePolicyV1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, e := FromWorktreeWithCapturePolicy(ctx, l, dir, Limits{}, StrictCapturePolicyV1); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	testkit.WriteFile(t, dir, "second", "x")
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{MaxFiles: 1}, StrictCapturePolicyV1); !isCode(e, "binary_too_large") {
		t.Fatal(e)
	}
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{MaxTotalBytes: 2}, StrictCapturePolicyV1); !isCode(e, "binary_too_large") {
		t.Fatal(e)
	}
	testkit.WriteFile(t, dir, "fifo", "indexed regular file")
	testkit.Git(t, dir, "add", "fifo")
	if e := os.Remove(filepath.Join(dir, "fifo")); e != nil {
		t.Fatal(e)
	}
	if e := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1); !isCode(e, "special_file_unsupported") {
		t.Fatalf("selected FIFO: %v", e)
	}
	if e := os.Remove(filepath.Join(dir, "fifo")); e != nil {
		t.Fatal(e)
	}
	testkit.Git(t, dir, "rm", "--cached", "-f", "fifo")
	testkit.WriteFile(t, dir, ".hint/private", "must refuse")
	testkit.Git(t, dir, "add", "-f", ".hint/private")
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1); !isCode(e, "protected_path") {
		t.Fatal(e)
	}
}

func TestBoundedGitOutputsAndFailures(t *testing.T) {
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "long-file-name", "x")
	s, e := openWorktreeRoot(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer s.close()
	if _, e = s.git(context.Background(), 1, "ls-files", "-z", "--others"); e == nil {
		t.Fatal("Git output limit ignored")
	}
	if _, e = s.git(context.Background(), 1024, "not-a-git-command"); e == nil {
		t.Fatal("failed command accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = s.git(ctx, 1024, "ls-files", "-z"); e == nil {
		t.Fatal("cancelled command accepted")
	}
}

func TestDescriptorReaderRejectsChangedAncestorWithSameLeaf(t *testing.T) {
	s, d := sourceFixture(t)
	if e := os.Mkdir(filepath.Join(d, "alternate"), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.Link(filepath.Join(d, "nested/file"), filepath.Join(d, "alternate/file")); e != nil {
		t.Fatal(e)
	}
	h := readHooks{afterOpen: func() {
		if e := os.Rename(filepath.Join(d, "nested"), filepath.Join(d, "old")); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(filepath.Join(d, "alternate"), filepath.Join(d, "nested")); e != nil {
			t.Fatal(e)
		}
	}}
	if _, e := s.read(context.Background(), "nested/file", 16, true, h); e == nil {
		t.Fatal("changed ancestor accepted because leaf inode matched")
	}
}

func TestStrictCaptureStageFailureAndMalformedOutput(t *testing.T) {
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "input", "x")
	l := lib(t)
	for _, mode := range []string{"failed", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			bin := canonicalDir(t)
			action := "exit 7"
			if mode == "malformed" {
				action = "printf 'malformed\\000'; exit 0"
			}
			quoted := "'" + strings.ReplaceAll(realGit, "'", "'\\''") + "'"
			script := "#!/bin/sh\nfor arg do if [ \"$arg\" = \"--stage\" ]; then " + action + "; fi; done\nexec " + quoted + " \"$@\"\n"
			if e := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); e != nil {
				t.Fatal(e)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			source, e := openWorktreeRoot(dir, true)
			if e != nil {
				t.Fatal(e)
			}
			defer source.close()
			if selected, e := source.git(context.Background(), strictInventoryBytes, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate"); e != nil || len(selected) == 0 {
				t.Fatalf("selector control failed: %v", e)
			}
			if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1); !isCode(e, "inventory_unavailable") {
				t.Fatalf("stage %s accepted: %v", mode, e)
			}
		})
	}
}

func TestStrictCaptureDocumentsUntrackedFIFOScope(t *testing.T) {
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "selected", "x")
	l := lib(t)
	before, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1)
	if e != nil {
		t.Fatal(e)
	}
	if e := unix.Mkfifo(filepath.Join(dir, "untracked-fifo"), 0o600); e != nil {
		t.Fatal(e)
	}
	after, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1)
	if e != nil || after != before {
		t.Fatalf("Git-selected artifact changed: %v", e)
	}
	// Equality establishes only that this FIFO is outside Git's selection. It is NOT absence proof.
	tr, e := Load(l, after)
	if e != nil {
		t.Fatal(e)
	}
	if len(tr.Files) != 1 || tr.Files[0].Path != "selected" {
		t.Fatalf("unexpected selection: %+v", tr.Files)
	}
}
func TestStrictLimitsInvalidAndClamped(t *testing.T) {
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	l := lib(t)
	for _, lim := range []Limits{{MaxFileBytes: -1}, {MaxTotalBytes: -1}, {MaxFiles: -1}} {
		if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, lim, StrictCapturePolicyV1); !isCode(e, "capture_limits_invalid") {
			t.Fatal(e)
		}
	}
	for _, policy := range []CapturePolicy{"", "true", "1"} {
		if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, policy); !isCode(e, "capture_policy_unknown") {
			t.Fatal(e)
		}
	}
	big, e := os.Create(filepath.Join(dir, "large"))
	if e != nil {
		t.Fatal(e)
	}
	if e = big.Truncate((8 << 20) + 1); e != nil {
		t.Fatal(e)
	}
	big.Close()
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{MaxFileBytes: math.MaxInt64, MaxTotalBytes: math.MaxInt64, MaxFiles: math.MaxInt}, StrictCapturePolicyV1); !isCode(e, "binary_too_large") {
		t.Fatalf("larger caller bounds bypassed strict maximum: %v", e)
	}
}

func TestStrictEffectiveBoundsAreExplicit(t *testing.T) {
	defaults, e := strictEffectiveLimits(Limits{})
	if e != nil || defaults != (Limits{MaxFileBytes: 8 << 20, MaxTotalBytes: 64 << 20, MaxFiles: 10000}) {
		t.Fatalf("%+v %v", defaults, e)
	}
	large, e := strictEffectiveLimits(Limits{math.MaxInt64, math.MaxInt64, math.MaxInt})
	if e != nil || large != defaults {
		t.Fatal("strict maxima changed")
	}
	small, e := strictEffectiveLimits(Limits{1, 2, 3})
	if e != nil || small != (Limits{1, 2, 3}) {
		t.Fatal("caller bounds not retained")
	}
	old := DefaultLimits
	defer func() { DefaultLimits = old }()
	DefaultLimits = Limits{1, 1, 1}
	again, e := strictEffectiveLimits(Limits{})
	if e != nil || again != defaults {
		t.Fatal("legacy defaults changed strict contract")
	}
}

func TestStrictProtectedSpellingScope(t *testing.T) {
	for _, p := range []string{".hint", ".HINT/private", ".HiNt/private", ".git/config", ".GIT/config", "source/.GiT/config"} {
		if !strictProtectedPath(p) {
			t.Fatalf("strict protected alias accepted: %s", p)
		}
	}
	for _, p := range []string{".hinted/input", ".gitignore", ".HINT-copy/input", "nested/.hint/input", "git/input"} {
		if strictProtectedPath(p) {
			t.Fatalf("ordinary spelling refused: %s", p)
		}
	}
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	for _, p := range []string{".hinted/input", ".gitignore", ".HINT-copy/input", "nested/.hint/input", "git/input"} {
		testkit.WriteFile(t, dir, p, "ordinary\n")
	}
	l := lib(t)
	legacy, e := FromWorktree(context.Background(), l, dir, Limits{})
	if e != nil {
		t.Fatal(e)
	}
	strict, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1)
	if e != nil || legacy != strict {
		t.Fatalf("ordinary spelling/tree bytes changed: %v", e)
	}
}

func TestStrictProtectedPathActualAlias(t *testing.T) {
	dir := canonicalDir(t)
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, ".hint/private", "synthetic private store bytes\n")
	original, e := os.Stat(filepath.Join(dir, ".hint/private"))
	if e != nil {
		t.Fatal(e)
	}
	alias, e := os.Stat(filepath.Join(dir, ".HINT/private"))
	if e != nil || !os.SameFile(original, alias) {
		t.Skip("requires a filesystem where .HINT aliases .hint; spelling controls run everywhere")
	}
	oid := strings.TrimSpace(testkit.Git(t, dir, "hash-object", "-w", ".hint/private"))
	testkit.Git(t, dir, "config", "core.ignorecase", "true")
	testkit.Git(t, dir, "update-index", "--add", "--cacheinfo", "100644", oid, ".HINT/private")
	selected := testkit.Git(t, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate")
	if !strings.Contains(selected, ".HINT/private\x00") || strings.Contains(selected, ".hint/private\x00") {
		t.Fatalf("alias regression did not isolate uppercase Git spelling: %q", selected)
	}
	l := lib(t)
	if _, e := FromWorktreeWithCapturePolicy(context.Background(), l, dir, Limits{}, StrictCapturePolicyV1); !isCode(e, "protected_path") {
		t.Fatalf("actual .hint alias accepted: %v", e)
	}
	// Strict-only protection must not silently change the legacy opt-in boundary.
	if _, e := FromWorktree(context.Background(), l, dir, Limits{}); e != nil {
		t.Fatalf("legacy spelling behavior changed: %v", e)
	}
	t.Log("actual case-insensitive alias exercised; no skip")
}

func TestCaptureDoesNotRunConfiguredFsmonitorSameStore(t *testing.T) {
	for _, mode := range []string{"local-config", "ambient-count", "ambient-parameters"} {
		t.Run(mode, func(t *testing.T) {
			root := canonicalDir(t)
			testkit.Git(t, root, "init", "-q")
			testkit.WriteFile(t, root, "main.py", "print(1)\n")
			testkit.Git(t, root, "add", "main.py")
			t.Setenv("HINT_KEY_DIR", t.TempDir())
			s, _, e := store.Init(root, store.InitOptions{})
			if e != nil {
				t.Fatal(e)
			}
			l := library.Open(s, 0)
			marker := filepath.Join(root, ".git", "monitor-marker")
			hook := filepath.Join(root, ".git", "fake-monitor.sh")
			quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
			script := fmt.Sprintf("#!/bin/sh\nprintf hit >> %s\nprintf 'token\\000'\n", quote(marker))
			if e = os.WriteFile(hook, []byte(script), 0o700); e != nil {
				t.Fatal(e)
			}
			testkit.Git(t, root, "config", "core.fsmonitor", hook)
			testkit.Git(t, root, "config", "core.hooksPath", filepath.Join(root, ".git"))
			testkit.Git(t, root, "status", "--porcelain")
			if _, e = os.Stat(marker); e != nil {
				t.Fatal("sentinel control did not execute configured fake fsmonitor")
			}
			if e = os.Remove(marker); e != nil {
				t.Fatal(e)
			}
			if mode == "ambient-count" {
				t.Setenv("GIT_CONFIG_COUNT", "2")
				t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
				t.Setenv("GIT_CONFIG_VALUE_0", hook)
				t.Setenv("GIT_CONFIG_KEY_1", "core.hooksPath")
				t.Setenv("GIT_CONFIG_VALUE_1", filepath.Join(root, ".git"))
			}
			if mode == "ambient-parameters" {
				t.Setenv("GIT_CONFIG_PARAMETERS", quote("core.fsmonitor="+hook)+" "+quote("core.hooksPath="+filepath.Join(root, ".git")))
			}
			strict, e := FromWorktreeWithCapturePolicy(context.Background(), l, root, Limits{}, StrictCapturePolicyV1)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(marker); !os.IsNotExist(e) {
				t.Fatal("strict capture or transitive persistence executed configured hook")
			}
			legacy, e := FromWorktree(context.Background(), l, root, Limits{})
			if e != nil || strict != legacy {
				t.Fatalf("ordinary selected bytes changed: %v", e)
			}
			if _, e = os.Stat(marker); !os.IsNotExist(e) {
				t.Fatal("legacy read-only inspection executed configured hook")
			}
			t.Log("configured fsmonitor control executed; strict+transitive and legacy captures did not")
		})
	}
}
