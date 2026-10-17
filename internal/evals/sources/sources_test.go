package sources

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "evals", "sessions", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func project(t *testing.T) (*library.Library, string) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return library.Open(s, 0), s.Root
}

// fakeOpenCode answers like the OpenCode 2.0 CLI and records every call.
type fakeOpenCode struct {
	t       *testing.T
	root    string
	exports map[string][]byte
	calls   []string
}

func (f *fakeOpenCode) Output(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if name != "opencode" {
		return nil, errors.New("unexpected program")
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		return []byte("2.0.26\n"), nil
	case len(args) >= 2 && args[0] == "session" && args[1] == "list":
		b, _ := json.Marshal([]map[string]any{{"id": "ses_synth_main_0001", "directory": f.root, "projectId": "p", "created": 1, "updated": 2},
			{"id": "ses_elsewhere", "directory": "/other/project", "projectId": "q", "created": 1, "updated": 2}})
		return b, nil
	case len(args) == 3 && args[0] == "session" && args[1] == "export":
		if b, ok := f.exports[args[2]]; ok {
			return b, nil
		}
		return nil, errors.New("no such session")
	}
	return nil, errors.New("unexpected arguments")
}

func TestOpenCodeDiscoveryFollowsChildren(t *testing.T) {
	l, root := project(t)
	f := &fakeOpenCode{t: t, root: root, exports: map[string][]byte{
		"ses_synth_main_0001":  fixture(t, "opencode-2.0.26-main.json"),
		"ses_synth_child_0001": fixture(t, "opencode-2.0.26-child.json"),
	}}
	rep := Run(context.Background(), l, Options{Agents: []string{"opencode"}, Roots: []string{root}, Cmd: f})
	if len(rep) != 1 || rep[0].New != 2 || rep[0].Failed != 0 {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "ses_elsewhere") || strings.Contains(c, " run") || strings.Contains(c, "--prompt") {
			t.Fatalf("unexpected call %q", c)
		}
	}
	var child sessions.IndexEntry
	if err := l.Index("opencode/ses_synth_child_0001", &child); err != nil {
		t.Fatal(err)
	}
	s, _ := sessions.Load(l, child)
	if s.Parent != "ses_synth_main_0001" {
		t.Fatalf("child parent %q", s.Parent)
	}
	again := Run(context.Background(), l, Options{Agents: []string{"opencode"}, Roots: []string{root}, Cmd: f})
	if again[0].Unchanged != 2 || again[0].New != 0 {
		t.Fatalf("second pass %+v", again)
	}
}

func TestOpenCodeMissingIsReported(t *testing.T) {
	l, root := project(t)
	rep := Run(context.Background(), l, Options{Agents: []string{"opencode"}, Roots: []string{root}, Cmd: missing{}})
	if rep[0].Reasons["agent_not_installed"] != 1 {
		t.Fatalf("%+v", rep)
	}
}

type missing struct{}

func (missing) Output(context.Context, string, string, ...string) ([]byte, error) {
	return nil, errors.New("not found")
}

func TestFileImportsClaudeAndCodex(t *testing.T) {
	l, root := project(t)
	home := t.TempDir()
	enc := filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(filepath.Clean(root), "-"))
	os.MkdirAll(filepath.Join(enc, "cc-synth-0001", "subagents"), 0o700)
	os.WriteFile(filepath.Join(enc, "cc-synth-0001.jsonl"), fixture(t, "claude-code-2.1.302.jsonl"), 0o600)
	os.WriteFile(filepath.Join(enc, "old.jsonl"), fixture(t, "claude-code-1.9.0.jsonl"), 0o600)
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "10", "11")
	os.MkdirAll(codexDir, 0o700)
	cx := strings.ReplaceAll(string(fixture(t, "codex-0.161.0.jsonl")), `"/work/shop"`, `"`+root+`"`)
	os.WriteFile(filepath.Join(codexDir, "rollout-a.jsonl"), []byte(cx), 0o600)
	os.WriteFile(filepath.Join(codexDir, "rollout-other.jsonl"), fixture(t, "codex-0.161.0.jsonl"), 0o600) // another project
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"token":"HINTCANARY_codex_auth"}`), 0o600)
	os.Symlink(filepath.Join(home, ".codex", "auth.json"), filepath.Join(codexDir, "rollout-link.jsonl"))

	for _, f := range codexFiles(Options{Home: home}) {
		if strings.Contains(f.path, "auth") || strings.HasSuffix(f.path, "rollout-link.jsonl") {
			t.Fatalf("codex candidate %s", f.path)
		}
	}
	rep := Run(context.Background(), l, Options{Agents: []string{"claude-code", "codex"}, Roots: []string{root}, Home: home})
	cc, cxr := rep[0], rep[1]
	if cc.New != 1 || cc.Reasons["parser_unsupported_version"] != 1 {
		t.Fatalf("claude %+v", cc)
	}
	if cxr.New != 1 || cxr.Skipped != 1 {
		t.Fatalf("codex %+v", cxr)
	}
	usage, _ := l.Usage()
	if usage == 0 {
		t.Fatal("nothing stored")
	}
	again := Run(context.Background(), l, Options{Agents: []string{"claude-code", "codex"}, Roots: []string{root}, Home: home})
	if again[0].New+again[1].New != 0 || again[0].Unchanged < 1 {
		t.Fatalf("second pass %+v", again)
	}
}

func TestLibraryHoldsNoAuthCanary(t *testing.T) {
	l, root := project(t)
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex", "sessions")
	os.MkdirAll(codexDir, 0o700)
	canary := "HINTCANARY_codex_auth"
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(canary), 0o600)
	Run(context.Background(), l, Options{Agents: []string{"codex"}, Roots: []string{root}, Home: home})
	hint := filepath.Join(root, ".hint")
	filepath.Walk(hint, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), canary) {
				t.Errorf("canary in %s", p)
			}
		}
		return nil
	})
}
