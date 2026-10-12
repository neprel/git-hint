package conformance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

// TestClientExportsPassReferenceChecker builds the git-hint client, runs it on a
// synthetic repository with agent events and a change, and validates every exported
// document with this reference checker (schema, semantic rules, export policy). The client
// has its own implementation of the contract; this test compares the two.
func TestClientExportsPassReferenceChecker(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the client")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "git-hint")
	build := exec.Command("go", "build", "-o", bin, "./cmd/git-hint")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build client: %v\n%s", err, out)
	}
	repo := filepath.Join(tmp, "repo")
	files := map[string]string{
		"go.mod":            "module example.invalid/r\n\ngo 1.22\n",
		"svc/rate.go":       "package svc\n\nfunc Rate(a, b int) int {\n\tif a > b {\n\t\treturn a\n\t}\n\treturn b\n}\n\ntype L struct{}\n\nfunc (L) Apply() {}\n",
		"main.go":           "package main\n\nimport \"example.invalid/r/svc\"\n\nfunc main() { _ = svc.Rate(1, 2) }\n",
		"web/app.ts":        "export const canarySymbolQ7 = 1\n",
		"docs/README.md":    "# HINTCANARY_DOC\n",
		"canary_dir_e1d2/x": "HINTCANARY_DATA\n",
	}
	for p, c := range files {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "HINT_KEY_DIR="+filepath.Join(tmp, "keys"), "GIT_CONFIG_NOSYSTEM=1")
	sh := func(stdin string, name string, args ...string) string {
		t.Helper()
		c := exec.Command(name, args...)
		c.Dir, c.Env = repo, env
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, b)
		}
		return string(b)
	}
	sh("", "git", "init", "-q")
	sh("", "git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "add", "-A")
	sh("", "git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "HINTCANARY_MSG")
	sh("", bin, "init")
	sh("", bin, "collect", "--full")
	ev := func(hook, tool, call string, out map[string]any) string {
		b, _ := json.Marshal(map[string]any{"hook": hook, "t_wall_ms": 1791700000000,
			"input": map[string]any{"tool": tool, "sessionID": "ses_HINTCANARY", "callID": call}, "output": out})
		return string(b) + "\n"
	}
	events := ev("tool.execute.before", "read", "c1", map[string]any{"args": map[string]any{"filePath": filepath.Join(repo, "svc/rate.go")}}) +
		ev("tool.execute.after", "read", "c1", map[string]any{"metadata": map[string]any{}, "output": "[redacted:len=10]"}) +
		ev("tool.execute.before", "bash", "c2", map[string]any{"args": map[string]any{"command": "go test ./... && cat canary_dir_e1d2/x"}}) +
		ev("tool.execute.after", "bash", "c2", map[string]any{"metadata": map[string]any{"exit": 1}, "output": "[redacted:len=99]"}) +
		ev("tool.execute.before", "task", "c3", map[string]any{"args": map[string]any{}})
	sh(events, bin, "adapter", "opencode", "--project", repo)
	if err := os.WriteFile(filepath.Join(repo, "svc/rate.go"), []byte("package svc\n\nfunc Rate(a, b int) int { return b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh("", bin, "collect")
	out := filepath.Join(tmp, "out")
	sh("", bin, "export", "--out", out)
	docs, _ := filepath.Glob(filepath.Join(out, "*.hint.json"))
	if len(docs) != 4 {
		t.Fatalf("want 4 documents, got %v", docs)
	}
	var policy map[string]any
	for _, f := range docs {
		if strings.Contains(filepath.Base(f), "export_policy-") {
			_, d := load(t, f)
			policy = d["payload"].(map[string]any)
		}
	}
	for _, f := range docs {
		dec, doc := load(t, f)
		if err := schemas.Validate(dec); err != nil {
			t.Errorf("%s: schema: %v", filepath.Base(f), err)
			continue
		}
		if fs := hintconf.Check(doc, policy); len(fs) > 0 {
			t.Errorf("%s: semantic: %v", filepath.Base(f), fs)
		}
		raw, _ := os.ReadFile(f)
		for _, bad := range append(hintconf.Canaries, repo, "rate.go", "svc", `"ses_`) {
			if strings.Contains(string(raw), bad) {
				t.Errorf("%s contains %q", filepath.Base(f), bad)
			}
		}
	}
}
