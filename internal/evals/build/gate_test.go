package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func goIn(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	return c.CombinedOutput()
}

// C7BJ-3 (reviewer repro): an existing test with another parameter name is required by the
// C2 gate, so its regression fails the gate.
func TestGateRequiresEveryExistingTest(t *testing.T) {
	d := t.TempDir()
	src := "package shop\n\nimport \"testing\"\n\nfunc TestRequired(t *testing.T) {}\n\nfunc TestRegression(x *testing.T) {\n\tif Value() != 0 {\n\t\tx.Fatal(\"regression still present\")\n\t}\n}\n"
	os.WriteFile(filepath.Join(d, "go.mod"), []byte("module shop\n\ngo 1.26\n"), 0o600)
	os.WriteFile(filepath.Join(d, "cart_test.go"), []byte(src), 0o600)
	os.WriteFile(filepath.Join(d, "value.go"), []byte("package shop\n\nfunc Value() int { return 1 }\n"), 0o600)
	var refs []TestRef
	for _, m := range goTestFunc.FindAllStringSubmatch(src, -1) {
		refs = append(refs, TestRef{File: "cart_test.go", Name: m[1]})
	}
	if len(refs) != 2 {
		t.Fatalf("refs %v", refs)
	}
	v := verifierPlan{Lang: "go", C2: refs, Pkgs: map[string]string{"cart_test.go": "shop"}}
	g := v.gates("C2", refs)[0]
	os.WriteFile(filepath.Join(d, "zz_hint_gate_test.go"), []byte(g.gateFile()), 0o600)
	bin := filepath.Join(d, "gate.test")
	if out, err := goIn(t, d, "test", "-c", "-o", bin, "."); err != nil {
		t.Fatalf("compile: %s %v", out, err)
	}
	n, _ := os.CreateTemp(d, "nonce")
	defer n.Close()
	n.WriteString("0123456789abcdef0123456789abcdef")
	n.Seek(0, 0)
	r, _ := os.CreateTemp(d, "report")
	defer r.Close()
	c := exec.Command(bin, "-test.run", "^"+g.Func+"$", "-test.count=1")
	c.Dir, c.ExtraFiles = d, []*os.File{n, r}
	if out, err := c.CombinedOutput(); err == nil {
		t.Fatalf("the gate passed a failing existing test: %s", out)
	}
	if b, _ := os.ReadFile(r.Name()); !strings.Contains(string(b), " TestRegression fail\n") {
		t.Fatalf("report %q", b)
	}
}

// C7BJ-3 (reviewer repro): Go C2 without a required test still runs the whole suite as a
// veto, so a repository that does not build fails C2.
func TestGoC2WithoutTestsKeepsTheVeto(t *testing.T) {
	s := verifierPlan{Lang: "go"}.runCheck("C2", nil, []string{"pass", "skip"})
	if !strings.Contains(s, "go test -count=1 ./...") || strings.Contains(s, `echo "CHECK C2 PASS"`) {
		t.Fatalf("C2 lines:\n%s", s)
	}
	d := t.TempDir() // the root-only paths of the container, here in a temporary directory
	s = strings.NewReplacer("/var/lib/hint-check/", d+"/", "/logs/verifier/", d+"/").Replace(s)
	c := exec.Command("sh", "-c", "killnobody() { :; }; run_as() { false; }; ok=1\n"+s)
	if out, _ := c.CombinedOutput(); !strings.Contains(string(out), "CHECK C2 FAIL") {
		t.Fatalf("a failing suite passed C2: %s", out)
	}
}
