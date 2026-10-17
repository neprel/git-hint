package build_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
)

func lib(t *testing.T) *library.Library { return evaltest.Library(t) }

func untar(t *testing.T, l *library.Library, digest string) map[string]string {
	t.Helper()
	b, err := l.GetObject(digest)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = string(c)
	}
	return out
}

func refused(err error, code string) bool {
	var r *build.Refusal
	if !errors.As(err, &r) {
		return false
	}
	for _, c := range r.Reasons {
		if c == code {
			return true
		}
	}
	return false
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEveryFamilyPackages(t *testing.T) {
	for _, c := range evaltest.Families() {
		l := lib(t)
		b, err := evaltest.Package(t, l, c, "go test ./...")
		if err != nil {
			t.Fatalf("%s: %v", c.Family, err)
		}
		if err := evals.ValidateValue("eval", b.Record); err != nil {
			t.Fatalf("%s: %v", c.Family, err)
		}
		v := untar(t, l, b.Record["verifier"].(map[string]any)["archive"].(string))
		switch c.Family {
		case "refactor":
			if len(b.Hidden) != 0 || !strings.Contains(v["api.expected"], ". shop func Total(price, discount int) int") || !strings.Contains(v["apicheck/main.go"], "package main") {
				t.Fatalf("refactor verifier %v", keysOf(v))
			}
			if c1 := b.Record["verifier"].(map[string]any)["checks"].([]any)[0].(map[string]any); c1["origin"] != "generated" || c1["generator"] == nil {
				t.Fatalf("refactor check provenance %v", c1)
			}
		default:
			if len(b.Hidden) != 1 {
				t.Fatalf("%s hidden %v", c.Family, b.Hidden)
			}
		}
	}
}

func TestRefactorWithoutAPIChangeIsRefused(t *testing.T) {
	l := lib(t)
	c := evaltest.RefactorExtract()
	c.Result = map[string]string{"go.mod": evaltest.GoMod, "cart_test.go": evaltest.GoOld,
		"cart.go": "package shop\n\nfunc discounted(p, d int) int { return p - d }\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return discounted(price, discount) }\n"}
	if _, err := evaltest.Package(t, l, c, "go test ./..."); !refused(err, "verifier_missing") {
		t.Fatalf("unexported-only refactor: %v", err)
	}
}

// The embedded verifier program and the builder compute the same API lines.
func TestAPICheckProgramMatchesTheBuilder(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("needs go")
	}
	l := lib(t)
	c := evaltest.Families()[3]
	b, err := evaltest.Package(t, l, c, "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	v := untar(t, l, b.Record["verifier"].(map[string]any)["archive"].(string))
	prog := t.TempDir()
	os.WriteFile(filepath.Join(prog, "main.go"), []byte(v["apicheck/main.go"]), 0o644)
	os.WriteFile(filepath.Join(prog, "go.mod"), []byte("module apicheck\n\ngo 1.21\n"), 0o644)
	repo := t.TempDir()
	for p, s := range c.Result {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755)
		os.WriteFile(filepath.Join(repo, p), []byte(s), 0o644)
	}
	cmd := exec.Command("go", "run", ".", repo, ".")
	cmd.Dir = prog
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if string(out) != v["api.expected"] {
		t.Fatalf("program %q\nbuilder %q", out, v["api.expected"])
	}
}
