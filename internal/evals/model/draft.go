package model

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
)

// GoTestDraft asks the local model for one Go test file that checks the requested change.
// The answer is untrusted: it must parse, declare the given package, define at least one
// TestXxx function, and import only the standard library or the module's own packages.
// It is never run on the host; qualification runs it in a container.
type GoTestDraft struct {
	Package string // package of the directory that the test file goes into
	Module  string // module path from go.mod
	Request string
	Before  string // source of the changed file before the change
	After   string // source after the change (the reference; evaluator-side only)
}

const system = `You write one Go test file. Answer with the file only, no explanation.
The test must fail on the code before the change and pass on the code after the change.
Test only behavior that the request states. Use the standard library "testing" package.`

var fence = regexp.MustCompile("(?s)```(?:go)?\\s*\\n(.*?)```")

// Generate returns the validated test source.
func (d GoTestDraft) Generate(ctx context.Context, c *Client) (string, error) {
	user := fmt.Sprintf("Package: %s\nModule: %s\n\nRequest:\n%s\n\nCode before the change:\n%s\n\nCode after the change:\n%s\n",
		d.Package, d.Module, d.Request, d.Before, d.After)
	if len(user) > 64<<10 {
		return "", fmt.Errorf("the change is too large for a draft")
	}
	out, err := c.Chat(ctx, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, 2000)
	if err != nil {
		return "", err
	}
	if m := fence.FindStringSubmatch(out); m != nil {
		out = m[1]
	}
	if err := d.Validate(out); err != nil {
		return "", err
	}
	return out, nil
}

// Validate checks a draft without running it.
func (d GoTestDraft) Validate(src string) error {
	if len(src) > 64<<10 {
		return fmt.Errorf("draft too large")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "draft_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("draft does not parse")
	}
	if f.Name.Name != d.Package && f.Name.Name != d.Package+"_test" {
		return fmt.Errorf("draft declares another package")
	}
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		std := !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
		if !std && !(d.Module != "" && (p == d.Module || strings.HasPrefix(p, d.Module+"/"))) {
			return fmt.Errorf("draft imports a package outside the standard library and the module")
		}
		if p == "os/exec" || p == "net" || p == "net/http" || p == "syscall" || p == "unsafe" || p == "plugin" {
			return fmt.Errorf("draft imports a package that tests of a change do not need")
		}
	}
	tests := 0
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
			tests++
		}
	}
	if tests == 0 {
		return fmt.Errorf("draft has no test function")
	}
	return nil
}
