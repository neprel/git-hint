package mcpserver_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/testkit"
)

// BenchmarkHintContext measures one hint_context answer (seed and expansion) on synthetic
// repositories of a stated size: packages × files × functions, each package importing the
// previous one. Reported: time and allocations per answer, and the answer size.
// Run: go test ./internal/mcpserver -run '^$' -bench HintContext -benchtime 50x -benchmem
func BenchmarkHintContext(b *testing.B) {
	for _, size := range []struct{ pkgs, files, funcs int }{{10, 10, 5}, {50, 20, 5}} {
		b.Run(fmt.Sprintf("files=%d,decls=%d", size.pkgs*size.files, size.pkgs*size.files*size.funcs), func(b *testing.B) {
			shop := benchProject(b, size.pkgs, size.files, size.funcs)
			cs := benchSession(b, shop)
			var bytes float64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "hint_context", Arguments: map[string]any{"seed": "p007.F3x2"}})
				if err != nil || res.IsError {
					b.Fatalf("hint_context: %v %v", err, res)
				}
				used := res.StructuredContent.(map[string]any)["used"].(map[string]any)
				bytes = used["bytes"].(float64)
			}
			b.ReportMetric(bytes, "answer_bytes")
		})
	}
}

func benchProject(b *testing.B, pkgs, files, funcs int) string {
	b.Helper()
	dir := b.TempDir()
	b.Setenv("HINT_KEY_DIR", filepath.Join(dir, "keys"))
	b.Setenv("HINT_SERVER_URL", "")
	repo := filepath.Join(dir, "repo")
	testkit.WriteFile(b, repo, "go.mod", "module example.invalid/bench\n\ngo 1.22\n")
	for p := range pkgs {
		for f := range files {
			var s strings.Builder
			fmt.Fprintf(&s, "package p%03d\n\n", p)
			if p > 0 {
				fmt.Fprintf(&s, "import _ \"example.invalid/bench/p%03d\"\n\n", p-1)
			}
			for fn := range funcs {
				fmt.Fprintf(&s, "// F%dx%d does a thing.\nfunc F%dx%d(a int) int {\n\treturn a + %d\n}\n\n", f, fn, f, fn, fn)
			}
			testkit.WriteFile(b, repo, fmt.Sprintf("p%03d/f%03d.go", p, f), s.String())
		}
		testkit.WriteFile(b, repo, fmt.Sprintf("p%03d/f_test.go", p), fmt.Sprintf("package p%03d\n\nimport \"testing\"\n\nfunc TestF3x2(t *testing.T) { _ = F3x2(1) }\n", p))
	}
	testkit.Git(b, repo, "init", "-q")
	gh(b, repo, "init")
	gh(b, repo, "collect", "--full")
	return repo
}

func benchSession(b *testing.B, dir string) *mcp.ClientSession {
	b.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := mcpserver.New(mcpserver.Options{Dir: dir}).Connect(ctx, st, nil); err != nil {
		b.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { cs.Close() })
	return cs
}
