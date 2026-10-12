package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/version"
)

func TestVersionAndCapabilities(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"--version"}, {"version"}} {
		r := must(t, run(t, dir, "", args...))
		if !strings.HasPrefix(r.stdout, "git-hint "+version.Version+" (HINT data standard "+version.SpecVersion+")") {
			t.Fatalf("%v: %q", args, r.stdout)
		}
	}
	var b version.Build
	if err := json.Unmarshal([]byte(must(t, run(t, dir, "", "version", "--json")).stdout), &b); err != nil || b.Version != version.Version || b.Commit == "" {
		t.Fatalf("version --json: %v %+v", err, b)
	}
	var c Capabilities
	if err := json.Unmarshal([]byte(must(t, run(t, dir, "", "version", "--capabilities", "--json")).stdout), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.MCP.Tools) != len(mcpserver.ToolNames) || len(c.MCP.ProtocolVersions) == 0 || c.MCP.Transport != "stdio" {
		t.Fatalf("mcp: %+v", c.MCP)
	}
	if len(c.Structure.EmbeddedGrammars) != 0 || len(c.Structure.Declarations) != 1 || c.Structure.Declarations[0] != "go" {
		t.Fatalf("structure: %+v", c.Structure)
	}
	if len(c.Adapters) != 3 || c.Adapters[0].Agent != "OpenCode" || c.Adapters[1].Status != "not available" {
		t.Fatalf("adapters: %+v", c.Adapters)
	}
	if len(c.OptionalTools) != len(optionalTools) {
		t.Fatalf("optional tools: %+v", c.OptionalTools)
	}
	txt := must(t, run(t, dir, "", "version", "--capabilities")).stdout
	for _, want := range []string{"embedded grammars: none", "optional scip-go:", "adapter OpenCode: available"} {
		if !strings.Contains(txt, want) {
			t.Errorf("capabilities text lacks %q", want)
		}
	}
}

func TestPlatformSupport(t *testing.T) {
	for _, c := range []struct{ os, arch, want string }{
		{"darwin", "arm64", "supported"}, {"darwin", "amd64", "supported"}, {"linux", "amd64", "supported"}, {"linux", "arm64", "supported"},
		{"windows", "amd64", "deferred (not supported, not tested)"}, {"linux", "riscv64", "not tested"}, {"freebsd", "amd64", "not tested"},
	} {
		if got := platformSupport(c.os, c.arch); got != c.want {
			t.Errorf("%s/%s: %q, want %q", c.os, c.arch, got, c.want)
		}
	}
}
