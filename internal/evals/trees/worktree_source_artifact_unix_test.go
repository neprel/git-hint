//go:build unix

package trees

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type artifactFixture struct {
	l                *library.Library
	root, domain     string
	globalExclude    []byte
	nativeGlobalPath string
}

// Synthetic owned repository with nested precedence controls. The Native owned
// XDG global exclude carries the SAME bytes the artifact profile replays.
func newArtifactFixture(t *testing.T) artifactFixture {
	t.Helper()
	sourceViewFixtureEnv(t)
	domain := sourceViewDomain(t)
	for _, name := range []string{"root", "home", "xdg", "keys"} {
		if e := os.Mkdir(filepath.Join(domain, name), 0700); e != nil {
			t.Fatal(e)
		}
	}
	root := filepath.Join(domain, "root")
	t.Setenv("HOME", filepath.Join(domain, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(domain, "xdg"))
	t.Setenv("HINT_KEY_DIR", filepath.Join(domain, "keys"))
	global := []byte("*.log\n!keep.log\nsecret*\n")
	globalPath := filepath.Join(domain, "xdg", "git", "ignore")
	testkit.WriteFile(t, filepath.Join(domain, "xdg"), "git/ignore", string(global))
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, ".gitignore", ".hint/\n")
	testkit.WriteFile(t, root, "sub/.gitignore", "keep.log\n")
	testkit.WriteFile(t, root, "tracked.go", "package tracked\n")
	testkit.Git(t, root, "add", ".gitignore", "sub/.gitignore", "tracked.go")
	testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "-qm", "synthetic")
	for _, p := range []string{"a.log", "keep.log", "secret.txt", "secret-ok.txt", "sub/keep.log", "sub/b.txt"} {
		testkit.WriteFile(t, root, p, "synthetic "+p+"\n")
	}
	if e := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("!secret-ok.txt\n"), 0644); e != nil {
		t.Fatal(e)
	}
	s, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return artifactFixture{library.Open(s, 0), root, domain, global, globalPath}
}

// sourceViewFixtureEnv repeats the existing fixture's environment isolation.
func sourceViewFixtureEnv(t *testing.T) {
	t.Helper()
	installed, e := filepath.EvalSymlinks(mustLookGit(t))
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
}
func mustLookGit(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, "git")
		if fi, e := os.Stat(p); e == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	t.Fatal("git not installed")
	return ""
}

func (f artifactFixture) options() SourceViewOptions {
	return SourceViewOptions{Artifact: &ArtifactSourceOptions{ReplayState: ArtifactReplayNativePresent, ReplayDigest: library.Digest(f.globalExclude), Replay: f.globalExclude}}
}

func capturePair(t *testing.T, v *SelectedSourceView) (*SelectedSourceViewCapture, *SelectedSourceViewCapture) {
	t.Helper()
	a, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	b, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = v.Close(); e != nil {
		t.Fatal(e)
	}
	return a, b
}

func selectedPaths(r SelectedReceipt) []string {
	out := []string{}
	for _, f := range r.Selected {
		out = append(out, f.Path)
	}
	return out
}

// Git itself keeps per-directory > info/exclude > global precedence; the
// replayed global bytes produce exactly the Native selection.
func TestArtifactSourceViewMatchesNativeSelection(t *testing.T) {
	f := newArtifactFixture(t)
	native, e := OpenSelectedSourceView(context.Background(), f.l, f.root, SourceViewOptions{})
	if e != nil {
		t.Fatal(e)
	}
	na, nb := capturePair(t, native)
	var trace [][]string
	sourceDispatchTrace = func(_ string, args []string) { trace = append(trace, args) }
	defer func() { sourceDispatchTrace = nil }()
	artifact, e := OpenSelectedSourceView(context.Background(), f.l, f.root, f.options())
	if e != nil {
		t.Fatal(e)
	}
	control := artifact.artifact.control
	aa, ab := capturePair(t, artifact)
	if _, e = os.Lstat(control); !os.IsNotExist(e) {
		t.Fatal("owned control remains after close")
	}
	n, a := na.Selected().Receipt(), aa.Selected().Receipt()
	want := []string{".gitignore", "keep.log", "secret-ok.txt", "sub/.gitignore", "sub/b.txt", "tracked.go"}
	if !reflect.DeepEqual(selectedPaths(n), want) || !reflect.DeepEqual(selectedPaths(a), want) || n.TreeDigest != a.TreeDigest || n.SelectedInventoryDigest != a.SelectedInventoryDigest {
		t.Fatalf("selection native=%v artifact=%v", selectedPaths(n), selectedPaths(a))
	}
	x, y := aa.Sources(), ab.Sources()
	if x.Version != SourceViewArtifactVersion || x.State != artifactReceiptState || x.SourceQueryAttempts != 18 || y.SourceQueryAttempts != 29 || x.ExecutableReads != x.TotalDispatchAttempts+2 {
		t.Fatalf("artifact receipt %+v", x)
	}
	for _, fact := range x.Facts {
		if fact.Path == f.nativeGlobalPath || fact.Role == "global_exclude" || strings.HasPrefix(fact.Path, filepath.Join(f.domain, "home")) || strings.HasPrefix(fact.Path, filepath.Join(f.domain, "xdg")) {
			t.Fatal("host source in artifact receipt", fact)
		}
	}
	// First dispatch is the control version; the first repository-aware query
	// follows the explicit-file config parse.
	if len(trace) < 3 || !reflect.DeepEqual(trace[0], []string{"--version"}) || trace[1][0] != "config" || trace[1][2] != "--file" || !strings.HasPrefix(trace[2][0], "--git-dir=") {
		t.Fatal("artifact query order", trace[:3])
	}
	for _, args := range trace[:2] {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") {
				t.Fatal("rooted argument before config validation")
			}
		}
	}
	lim := Limits{}
	wire, e := EncodeSelectedSourcePair(lim, aa.Selected().Receipt(), ab.Selected().Receipt(), x, y)
	if e != nil {
		t.Fatal("artifact pair codec", e)
	}
	pair, e := DecodeSelectedSourcePair(lim, wire)
	if e != nil {
		t.Fatal(e)
	}
	objects := map[string][]byte{}
	for _, ref := range pair.ObjectRefs() {
		b, e := f.l.GetObject(ref.Digest)
		if e != nil {
			t.Fatal(e)
		}
		objects[ref.Digest] = b
	}
	if _, e = pair.ValidateObjects(objects); e != nil {
		t.Fatal("artifact objects", e)
	}
	// Mixed Native/artifact pairs refuse in both orders.
	nx, ny := na.Sources(), nb.Sources()
	if _, e = EncodeSelectedSourcePair(lim, na.Selected().Receipt(), ab.Selected().Receipt(), nx, y); e == nil {
		t.Fatal("mixed profile pair admitted")
	}
	if _, e = EncodeSelectedSourcePair(lim, aa.Selected().Receipt(), nb.Selected().Receipt(), x, ny); e == nil {
		t.Fatal("mixed profile pair admitted")
	}
	// Native receipt stays the Native domain.
	if nx.Version != SourceViewVersion || nx.SourceQueryAttempts != 24 || ny.SourceQueryAttempts != 38 || len(nx.CandidatePaths) != 3 || nx.ExecutableReads != nx.TotalDispatchAttempts+1 {
		t.Fatal("native receipt changed")
	}
}

// Malformed or redirecting artifact metadata refuses BEFORE any repository-aware
// dispatch. Only the control version and explicit-file parser may run.
func TestArtifactSourceViewRefusesBeforeRepositoryDispatch(t *testing.T) {
	cases := map[string]func(t *testing.T, root, domain string){
		"excludes_absolute": func(t *testing.T, root, domain string) {
			appendConfig(t, root, "[core]\n\texcludesFile = "+filepath.Join(domain, "outside")+"\n")
		},
		"excludes_home": func(t *testing.T, root, _ string) { appendConfig(t, root, "[core]\n\texcludesFile = ~/outside\n") },
		"include": func(t *testing.T, root, domain string) {
			appendConfig(t, root, "[include]\n\tpath = "+filepath.Join(domain, "outside")+"\n")
		},
		"include_if": func(t *testing.T, root, domain string) {
			appendConfig(t, root, "[includeIf \"gitdir:/\"]\n\tpath = "+filepath.Join(domain, "outside")+"\n")
		},
		"worktree":  func(t *testing.T, root, domain string) { appendConfig(t, root, "[core]\n\tworktree = "+domain+"\n") },
		"extension": func(t *testing.T, root, _ string) { appendConfig(t, root, "[extensions]\n\tworktreeConfig = true\n") },
		"attributes": func(t *testing.T, root, domain string) {
			appendConfig(t, root, "[core]\n\tattributesFile = "+filepath.Join(domain, "outside")+"\n")
		},
		"unknown_key":     func(t *testing.T, root, _ string) { appendConfig(t, root, "[remote \"origin\"]\n\turl = /synthetic\n") },
		"non_boolean":     func(t *testing.T, root, _ string) { appendConfig(t, root, "[core]\n\tignorecase = yes\n") },
		"commondir":       func(t *testing.T, root, domain string) { writeGit(t, root, "commondir", domain+"\n") },
		"gitdir_file":     func(t *testing.T, root, domain string) { writeGit(t, root, "gitdir", domain+"\n") },
		"alternates":      func(t *testing.T, root, domain string) { writeGit(t, root, "objects/info/alternates", domain+"\n") },
		"config_worktree": func(t *testing.T, root, _ string) { writeGit(t, root, "config.worktree", "[core]\n") },
		"head_redirect":   func(t *testing.T, root, _ string) { writeGit(t, root, "HEAD", "ref: refs/remotes/origin/main\n") },
		"gitfile": func(t *testing.T, root, domain string) {
			if e := os.Rename(filepath.Join(root, ".git"), filepath.Join(domain, "moved-git")); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+filepath.Join(domain, "moved-git")+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newArtifactFixture(t)
			sentinel := []byte("SYNTHETIC-OUTSIDE-SENTINEL-" + name + "\n")
			if e := os.WriteFile(filepath.Join(f.domain, "outside"), sentinel, 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(f.domain, "home", "outside"), sentinel, 0600); e != nil {
				t.Fatal(e)
			}
			mutate(t, f.root, f.domain)
			var mu sync.Mutex
			var trace [][]string
			sourceDispatchTrace = func(_ string, args []string) { mu.Lock(); trace = append(trace, args); mu.Unlock() }
			defer func() { sourceDispatchTrace = nil }()
			v, e := OpenSelectedSourceView(context.Background(), f.l, f.root, f.options())
			if e == nil || v != nil {
				t.Fatal("artifact redirection admitted")
			}
			parsed := false
			for _, args := range trace {
				config := len(args) > 2 && args[0] == "config" && args[2] == "--file"
				parsed = parsed || config
				if !(reflect.DeepEqual(args, []string{"--version"}) || config) {
					t.Fatal("repository-aware dispatch before validation", args)
				}
			}
			// Config cases must reach and fail the closed allowlist; metadata cases
			// must refuse before the parser runs.
			metadata := map[string]bool{"commondir": true, "gitdir_file": true, "alternates": true, "config_worktree": true, "head_redirect": true, "gitfile": true}
			if parsed == metadata[name] || len(trace) == 0 || !reflect.DeepEqual(trace[0], []string{"--version"}) {
				t.Fatal("refusal at an unexpected phase", trace)
			}
			if _, e := f.l.GetObject(library.Digest(sentinel)); e == nil {
				t.Fatal("outside sentinel retained")
			}
		})
	}
}
func appendConfig(t *testing.T, root, extra string) {
	t.Helper()
	p := filepath.Join(root, ".git", "config")
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, append(b, []byte("\n"+extra)...), 0644); e != nil {
		t.Fatal(e)
	}
}
func writeGit(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, ".git", filepath.FromSlash(name))
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0644); e != nil {
		t.Fatal(e)
	}
}

func TestArtifactSourceViewOptionsClosed(t *testing.T) {
	f := newArtifactFixture(t)
	for _, o := range []ArtifactSourceOptions{
		{},
		{ReplayState: "observed_absent", ReplayDigest: library.Digest(nil)},
		{ReplayState: ArtifactReplayNativePresent, ReplayDigest: library.Digest([]byte("other")), Replay: f.globalExclude},
		{ReplayState: ArtifactReplayNativeAbsent, ReplayDigest: library.Digest(f.globalExclude), Replay: f.globalExclude},
	} {
		o := o
		if v, e := OpenSelectedSourceView(context.Background(), f.l, f.root, SourceViewOptions{Artifact: &o}); e == nil || v != nil {
			t.Fatal("unclosed artifact replay admitted", o.ReplayState)
		}
	}
}
