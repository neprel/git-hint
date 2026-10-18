package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
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

func kinds(s Session) map[string]int {
	out := map[string]int{}
	for _, it := range s.Items {
		out[it.Kind]++
		if it.Kind == KindTool {
			out["tool:"+it.Tool+":"+it.Status]++
		}
	}
	return out
}

func requests(r Result) []string {
	var out []string
	for _, it := range r.Session.Items {
		if it.Kind == KindRequest {
			out = append(out, string(r.Texts[it.Text]))
		}
	}
	return out
}

func TestOpenCodeExport(t *testing.T) {
	r, err := ParseOpenCode(fixture(t, "opencode-2.0.26-main.json"), OpenCodeOptions{Version: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session
	k := kinds(s)
	if k[KindRequest] != 2 || k[KindSystem] != 1 || k[KindAssistant] != 2 || k[KindTool] != 5 {
		t.Fatalf("kinds %v", k)
	}
	if k["tool:read:completed"] != 1 || k["tool:edit:completed"] != 1 || k["tool:shell:error"] != 1 || k["tool:shell:completed"] != 1 || k["tool:child:completed"] != 1 {
		t.Fatalf("tools %v", k)
	}
	var child, snaps, paths int
	for _, it := range s.Items {
		if it.Child == "ses_synth_child_0001" {
			child++
		}
		if it.SnapStart != "" && it.SnapEnd != "" {
			snaps++
		}
		if len(it.Paths) == 1 && it.Paths[0] == "/work/shop/cart.go" {
			paths++
		}
	}
	if child != 1 || snaps != 2 || paths != 2 {
		t.Fatalf("child %d snapshots %d paths %d", child, snaps, paths)
	}
	if got := requests(r); len(got) != 2 || !strings.Contains(got[1], "rounding") {
		t.Fatalf("requests %q", got)
	}
	if !contains(s.Missing, "parent_link") || s.Agent != "opencode" || s.AgentVersion != "2.0.26" {
		t.Fatalf("session %+v", s)
	}
}

func TestOpenCodeChildPromptIsNotARequest(t *testing.T) {
	r, err := ParseOpenCode(fixture(t, "opencode-2.0.26-child.json"), OpenCodeOptions{Version: "2.0.26", Parent: "ses_synth_main_0001"})
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(r.Session); k[KindRequest] != 0 || k[KindSystem] != 1 || r.Session.Parent != "ses_synth_main_0001" {
		t.Fatalf("child session %v %s", k, r.Session.Parent)
	}
}

func TestClaudeCode(t *testing.T) {
	r, err := ParseClaudeCode(fixture(t, "claude-code-2.1.302.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	k := kinds(r.Session)
	if k[KindRequest] != 2 || k[KindSystem] != 1 || k[KindCompaction] != 1 || k["tool:read:completed"] != 1 || k["tool:edit:completed"] != 1 || k["tool:shell:error"] != 1 {
		t.Fatalf("kinds %v", k)
	}
	var in int64
	for _, it := range r.Session.Items {
		if it.Usage != nil && it.Usage.Input != nil {
			in += *it.Usage.Input
		}
	}
	if in != 700 { // msg_1 spans two lines: its usage counts once
		t.Fatalf("input tokens %d", in)
	}
	for _, it := range r.Session.Items {
		if it.Kind == KindRequest && it.Line == 0 {
			t.Fatal("request without a line number")
		}
	}
}

func TestClaudeCodeTruncatedAndMalformed(t *testing.T) {
	r, err := ParseClaudeCode(fixture(t, "claude-code-2.1.302-truncated.jsonl"))
	if err != nil || !r.Session.Truncated {
		t.Fatalf("truncated: %v %v", err, r.Session.Truncated)
	}
	r, err = ParseClaudeCode(fixture(t, "claude-code-2.1.302-malformed.jsonl"))
	if err != nil || r.Session.Malformed != 1 || r.Session.Truncated {
		t.Fatalf("malformed: %v %+v", err, r.Session)
	}
}

func TestCodex(t *testing.T) {
	r, err := ParseCodex(fixture(t, "codex-0.161.0.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	k := kinds(r.Session)
	if k[KindRequest] != 1 || k[KindSystem] != 2 || k[KindCompaction] != 1 || k["tool:shell:completed"] != 1 || k["tool:edit:completed"] != 1 {
		t.Fatalf("kinds %v", k)
	}
	for _, it := range r.Session.Items {
		if it.Tool == ToolEdit && (len(it.Paths) != 1 || it.Paths[0] != "cart.go") {
			t.Fatalf("patch paths %v", it.Paths)
		}
	}
}

func TestUnknownVersionsAreRefused(t *testing.T) {
	if _, err := ParseClaudeCode(fixture(t, "claude-code-1.9.0.jsonl")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("claude-code 1.9.0: %v", err)
	}
	if _, err := ParseCodex(fixture(t, "codex-0.150.0.jsonl")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("codex 0.150.0: %v", err)
	}
	if _, err := ParseOpenCode(fixture(t, "opencode-2.0.26-main.json"), OpenCodeOptions{Version: "1.4.0"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("opencode 1.4.0: %v", err)
	}
	if _, err := ParseOpenCode(fixture(t, "opencode-2.0.26-main.json"), OpenCodeOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("opencode without a version: %v", err)
	}
}

func TestDiagnosticsHoldNoText(t *testing.T) {
	canary := "HINTCANARY_secret_token_value"
	bad := []byte(`{"info":{"id":"x"},"messages":"` + canary + `"`) // truncated JSON with a secret
	_, err := ParseOpenCode(bad, OpenCodeOptions{Version: "2.0.26"})
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("diagnostic: %v", err)
	}
	_, err = ParseClaudeCode([]byte(`{"type":"user","message":{"content":"` + canary + `"}}` + "\n"))
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("diagnostic: %v", err)
	}
	big := make([]byte, MaxSourceBytes+1)
	if _, err := ParseCodex(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize source: %v", err)
	}
}

func TestOversizeFieldIsNotStored(t *testing.T) {
	var doc map[string]any
	json.Unmarshal(fixture(t, "opencode-2.0.26-main.json"), &doc)
	doc["messages"].([]any)[0].(map[string]any)["text"] = strings.Repeat("x", MaxFieldBytes+1)
	b, _ := json.Marshal(doc)
	r, err := ParseOpenCode(b, OpenCodeOptions{Version: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	first := r.Session.Items[0]
	if !first.Oversize || first.Text != "" {
		t.Fatalf("oversize text stored: %+v", first)
	}
}

func project(t *testing.T) *library.Library {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return library.Open(s, 0)
}

func TestImportIsIdempotentAndIncremental(t *testing.T) {
	l := project(t)
	early := fixture(t, "opencode-2.0.26-main-early.json")
	full := fixture(t, "opencode-2.0.26-main.json")
	parse := func(b []byte) Result {
		r, err := ParseOpenCode(b, OpenCodeOptions{Version: "2.0.26"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	o, err := Import(l, early, parse(early))
	if err != nil || o.State != "new" {
		t.Fatalf("first import: %v %+v", err, o)
	}
	if o, _ = Import(l, early, parse(early)); o.State != "unchanged" {
		t.Fatalf("duplicate import: %+v", o)
	}
	o, err = Import(l, full, parse(full))
	if err != nil || o.State != "updated" || o.Items <= 4 {
		t.Fatalf("continuation: %v %+v", err, o)
	}
	var idx IndexEntry
	if err := l.Index("opencode/ses_synth_main_0001", &idx); err != nil || len(idx.Sources) != 2 {
		t.Fatalf("index: %v %+v", err, idx)
	}
	// re-importing the early copy later must not replace the longer form
	if o, _ = Import(l, early, parse(early)); o.State != "unchanged" || o.Normalized != idx.Normalized {
		t.Fatalf("stale copy: %+v", o)
	}
	s, err := Load(l, idx)
	if err != nil || len(s.Items) != idx.Items {
		t.Fatalf("load: %v", err)
	}
}

func TestTruncatedLaterCopyDoesNotReplace(t *testing.T) {
	l := project(t)
	full := fixture(t, "claude-code-2.1.302.jsonl")
	cut := fixture(t, "claude-code-2.1.302-truncated.jsonl")
	r1, _ := ParseClaudeCode(full)
	r2, _ := ParseClaudeCode(cut)
	o1, _ := Import(l, full, r1)
	o2, err := Import(l, cut, r2)
	if err != nil || o2.Normalized != o1.Normalized {
		t.Fatalf("a truncated copy replaced the full session: %v", err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A recorded OpenCode 2.0.22 export (fake local model, synthetic content): the tools are
// named write and shell. The live prospective check found that "shell" was not read as a
// command, so the session's passing check was lost (no_checkable_outcome).
func TestOpenCode2022ToolNames(t *testing.T) {
	r, err := ParseOpenCode(fixture(t, "opencode-2.0.22-tools.json"), OpenCodeOptions{Version: "2.0.22"})
	if err != nil {
		t.Fatal(err)
	}
	var shell, edit *Item
	for i, it := range r.Session.Items {
		if it.Kind != KindTool {
			continue
		}
		switch it.ToolName {
		case "shell":
			shell = &r.Session.Items[i]
		case "write":
			edit = &r.Session.Items[i]
		}
	}
	if shell == nil || shell.Tool != ToolShell || shell.Command == "" || shell.Status != "completed" {
		t.Fatalf("shell %+v", shell)
	}
	if edit == nil || edit.Tool != ToolEdit || len(edit.Paths) != 1 || !strings.HasSuffix(edit.Paths[0], "discount_test.go") {
		t.Fatalf("write %+v", edit)
	}
}

// OpenCode 2.0.22 marks a failing command "completed" and puts the exit code (3 here) into
// the metadata (recorded offline). A non-zero exit is a failed command, never a passing
// check.
func TestOpenCode2022FailedCommand(t *testing.T) {
	r, err := ParseOpenCode(fixture(t, "opencode-2.0.22-failed-command.json"), OpenCodeOptions{Version: "2.0.22"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range r.Session.Items {
		if it.Kind == KindTool && it.ToolName == "shell" {
			found = true
			if it.Status != "error" {
				t.Fatalf("a command with exit 3 has status %q", it.Status)
			}
		}
	}
	if !found {
		t.Fatal("no shell item")
	}
	// the passing recorded command (exit 0) stays completed
	ok, _ := ParseOpenCode(fixture(t, "opencode-2.0.22-tools.json"), OpenCodeOptions{Version: "2.0.22"})
	for _, it := range ok.Session.Items {
		if it.ToolName == "shell" && it.Status != "completed" {
			t.Fatalf("exit 0: %q", it.Status)
		}
	}
}
