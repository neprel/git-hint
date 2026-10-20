package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
)

// C4 forgery probes (07B-I, 07B-J): wrong code under test tries to forge a pass from
// inside the check process. Each probe keeps the bug (Total ignores the discount), so the
// only honest result is C1 FAIL and reward 0. The verifier's stdout must carry each check
// line once. gate-v1 blocks these generic attempts; code written against the gate itself
// in the same process is not blocked (same-process trust; Q7 not met).

// goForgeOut writes a fake CHECK line to every stdout it can open (other processes'
// descriptors, the console), then lets the tests run.
const goForgeOut = `package shop

import (
	"os"
	"path/filepath"
	"testing"
)

func init() {
	if !testing.Testing() {
		return
	}
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/[0-9]*")
	for _, p := range append(fds, "/dev/console", "/dev/tty", "/proc/1/fd/1") {
		if f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0); err == nil {
			f.WriteString("CHECK C1 PASS\nTEST C1 discount_test.go::TestDiscount PASS\n")
			f.Close()
		}
	}
}
`

// goForgeReward leaves a detached process that keeps writing a passing reward.
const goForgeReward = `package shop

import (
	"os/exec"
	"syscall"
	"testing"
)

func init() {
	if !testing.Testing() {
		return
	}
	c := exec.Command("sh", "-c", "while :; do echo 1 > /logs/verifier/reward.txt; echo '{\"reward\": 1}' > /logs/verifier/reward.json; done")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Start()
}
`

// goForgeFraming prints test2json framing lines (the 0x16 marker of -test.v=test2json) that
// claim every test passed, then ends the test binary with status 0 before any test runs.
const goForgeFraming = `package shop

import (
	"os"
	"testing"
)

func init() {
	if !testing.Testing() {
		return
	}
	for _, n := range []string{"TestDiscount", "TestNoDiscount"} {
		os.Stdout.WriteString("\x16=== RUN   " + n + "\n\x16--- PASS: " + n + " (0.00s)\n")
	}
	os.Stdout.WriteString("\x16PASS\n")
	os.Exit(0)
}
`

// goForgeJSON prints raw go test -json events (without the marker) and exits 0.
const goForgeJSON = `package shop

import (
	"os"
	"testing"
)

func init() {
	if !testing.Testing() {
		return
	}
	for _, n := range []string{"TestDiscount", "TestNoDiscount"} {
		os.Stdout.WriteString("{\"Action\":\"pass\",\"Package\":\"shop\",\"Test\":\"" + n + "\",\"Elapsed\":0}\n")
	}
	os.Exit(0)
}
`

// goStealNonce reads the gate's nonce descriptor before the gate runs (no knowledge of the
// report format), then lets the tests run.
const goStealNonce = `package shop

import (
	"os"
	"testing"
)

func init() {
	if !testing.Testing() {
		return
	}
	b := make([]byte, 64)
	os.NewFile(3, "x").Read(b)
}
`

func TestVerifierForgeryGo(t *testing.T) {
	needDocker(t)
	l := lib(t)
	goMod := "module shop\n\ngo 1.26\n"
	old := "package shop\n\nimport \"testing\"\n\nfunc TestNoDiscount(t *testing.T) {\n\tif Total(10, 0) != 10 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	base := map[string]string{"go.mod": goMod, "cart.go": bug, "cart_test.go": old}
	in := input(t, l)
	in.Base = mkTree(t, l, base)
	in.Result = mkTree(t, l, map[string]string{"go.mod": goMod, "cart.go": fix, "cart_test.go": old, "discount_test.go": newTest})
	b, err := Build(l, in)
	if err != nil {
		t.Fatal(err)
	}
	image := b.Record["environment"].(map[string]any)["image"].(string)
	for name, hack := range map[string]string{"stdout-escape": goForgeOut, "reward-writer": goForgeReward, "test2json-framing": goForgeFraming, "raw-json-events": goForgeJSON, "nonce-theft": goStealNonce} {
		t.Run(name, func(t *testing.T) {
			app := map[string]string{"hack.go": hack}
			for k, v := range base {
				app[k] = v
			}
			checks, tests, reward, out := runVerifierOut(t, l, image, b.Record, app)
			forged(t, checks, tests, reward, out)
		})
	}
}

// pyForgeFD writes passing pytest -v lines to every inherited descriptor (pytest keeps the
// original stdout open while it captures) and ends the run with status 0.
const pyForgeFD = `import os
msg = b"test_discount.py::TestDiscount::test_discount PASSED\ntest_cart.py::test_no_discount PASSED\n"
for fd in range(1, 64):
    try:
        os.write(fd, msg)
    except OSError:
        pass
os._exit(0)
`

func TestVerifierForgeryPython(t *testing.T) {
	needDocker(t)
	l := lib(t)
	bug := "def total(price, discount):\n    return price\n"
	fix := "def total(price, discount):\n    return price - discount\n"
	old := "from shop import total\n\ndef test_no_discount():\n    assert total(10, 0) == 10\n"
	added := "from shop import total\n\nclass TestDiscount:\n    def test_discount(self):\n        assert total(10, 3) == 7\n"
	base := map[string]string{"shop.py": bug, "test_cart.py": old, "requirements.txt": ""}
	result := map[string]string{"shop.py": fix, "test_cart.py": old, "test_discount.py": added, "requirements.txt": ""}
	at := map[string]any{"source": library.Digest([]byte("src")), "record": "u1"}
	b, err := Build(l, Input{EpisodeID: "ep-0123456789abcd01", Family: "bug_fix", Repo: "shop", Base: mkTree(t, l, base), Result: mkTree(t, l, result),
		Paths: []string{"shop.py", "test_discount.py"}, Attribution: "exclusive", Outcome: "test_passed",
		Request: "total ignores the discount; fix it.", RequestAt: at, CheckHint: "pytest", Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	image := pyImage(t, b.Record)
	for name, hack := range map[string]string{"fd-pass-lines": bug + pyForgeFD} {
		t.Run(name, func(t *testing.T) {
			app := map[string]string{"shop.py": hack, "test_cart.py": old, "requirements.txt": ""}
			checks, tests, reward, out := runVerifierOut(t, l, image, b.Record, app)
			forged(t, checks, tests, reward, out)
		})
	}
}

// forged fails the probe when the bug passed or a check or test line appeared twice.
func forged(t *testing.T, checks, tests map[string]string, reward, out string) {
	t.Helper()
	if checks["C1"] == "PASS" || reward != "0" {
		t.Errorf("FORGED: checks %v tests %v reward %q", checks, tests, reward)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "CHECK ") || strings.HasPrefix(ln, "TEST ") {
			if strings.Count(out, ln+"\n") > 1 {
				t.Errorf("FORGED line in the verifier stdout: %q", ln)
			}
		}
	}
}

// gate-v1 must keep honest results: parallel tests with parallel subtests and a test in the
// external test package pass with the reference and fail on the base.
func TestVerifierGateHonestGo(t *testing.T) {
	needDocker(t)
	l := lib(t)
	goMod := "module shop\n\ngo 1.26\n"
	par := "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tt.Parallel()\n\tfor _, c := range []int{3, 4} {\n\t\tc := c\n\t\tt.Run(\"c\", func(t *testing.T) {\n\t\t\tt.Parallel()\n\t\t\tif Total(10, c) != 10-c {\n\t\t\t\tt.Fatal(c)\n\t\t\t}\n\t\t})\n\t}\n}\n"
	ext := "package shop_test\n\nimport (\n\t\"shop\"\n\t\"testing\"\n)\n\nfunc TestExternal(t *testing.T) {\n\tif shop.Total(5, 1) != 4 {\n\t\tt.Fatal()\n\t}\n}\n"
	old := "package shop\n\nimport \"testing\"\n\nfunc TestNoDiscount(t *testing.T) {\n\tt.Parallel()\n\tif Total(10, 0) != 10 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n\nfunc TestSkipped(t *testing.T) { t.Skip(\"not here\") }\n"
	base := map[string]string{"go.mod": goMod, "cart.go": bug, "cart_test.go": old}
	in := input(t, l)
	in.Base = mkTree(t, l, base)
	in.Result = mkTree(t, l, map[string]string{"go.mod": goMod, "cart.go": fix, "cart_test.go": old, "discount_test.go": par, "ext_test.go": ext})
	in.Paths = []string{"cart.go", "discount_test.go", "ext_test.go"}
	b, err := Build(l, in)
	if err != nil {
		t.Fatal(err)
	}
	image := b.Record["environment"].(map[string]any)["image"].(string)
	for _, c := range []struct {
		name, code, c1, reward string
	}{{"reference", fix, "PASS", "1"}, {"base", bug, "FAIL", "0"}} {
		t.Run(c.name, func(t *testing.T) {
			checks, tests, reward := runVerifier(t, l, image, b.Record, map[string]string{"go.mod": goMod, "cart.go": c.code, "cart_test.go": old})
			if checks["C1"] != c.c1 || checks["C2"] != "PASS" || reward != c.reward || tests["C2 cart_test.go::TestSkipped"] != "PASS" ||
				tests["C1 ext_test.go::TestExternal"] != c.c1 || tests["C1 discount_test.go::TestDiscount"] != c.c1 {
				t.Fatalf("checks %v tests %v reward %q", checks, tests, reward)
			}
		})
	}
}

// The same for Python: a parametrized test and a class method pass with the reference;
// an xfail existing test is accepted for C2.
func TestVerifierGateHonestPython(t *testing.T) {
	needDocker(t)
	l := lib(t)
	bug := "def total(price, discount):\n    return price\n"
	fix := "def total(price, discount):\n    return price - discount\n"
	old := "import pytest\nfrom shop import total\n\ndef test_no_discount():\n    assert total(10, 0) == 10\n\n@pytest.mark.xfail(reason=\"known\")\ndef test_known():\n    assert False\n"
	added := "import pytest\nfrom shop import total\n\n@pytest.mark.parametrize(\"d\", [1, 2, 3])\ndef test_param(d):\n    assert total(10, d) == 10 - d\n\nclass TestDiscount:\n    def test_discount(self):\n        assert total(10, 3) == 7\n"
	base := map[string]string{"shop.py": bug, "test_cart.py": old, "requirements.txt": ""}
	result := map[string]string{"shop.py": fix, "test_cart.py": old, "test_discount.py": added, "requirements.txt": ""}
	at := map[string]any{"source": library.Digest([]byte("src")), "record": "u1"}
	b, err := Build(l, Input{EpisodeID: "ep-0123456789abcd02", Family: "bug_fix", Repo: "shop", Base: mkTree(t, l, base), Result: mkTree(t, l, result),
		Paths: []string{"shop.py", "test_discount.py"}, Attribution: "exclusive", Outcome: "test_passed",
		Request: "total ignores the discount; fix it.", RequestAt: at, CheckHint: "pytest", Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	image := pyImage(t, b.Record)
	for _, c := range []struct {
		name, code, c1, reward string
	}{{"reference", fix, "PASS", "1"}, {"base", bug, "FAIL", "0"}} {
		t.Run(c.name, func(t *testing.T) {
			checks, tests, reward := runVerifier(t, l, image, b.Record, map[string]string{"shop.py": c.code, "test_cart.py": old, "requirements.txt": ""})
			if checks["C1"] != c.c1 || checks["C2"] != "PASS" || reward != c.reward || tests["C1 test_discount.py::test_param"] != c.c1 ||
				tests["C1 test_discount.py::test_discount"] != c.c1 || tests["C2 test_cart.py::test_known"] != "PASS" {
				t.Fatalf("checks %v tests %v reward %q", checks, tests, reward)
			}
		})
	}
}

// pyImage is the verifier image of a Python task with pytest (built once, locally).
func pyImage(t *testing.T, rec map[string]any) string {
	t.Helper()
	ref := rec["environment"].(map[string]any)["image"].(string)
	image := "hint-verifier-test-python:" + regexp.MustCompile(`[^a-f0-9]`).ReplaceAllString(ref[strings.LastIndex(ref, ":")+1:], "")[:12]
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		ctx := t.TempDir()
		os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM "+ref+"\nRUN python -m pip install -q pytest==8.4.2\n"), 0o644)
		if out, err := exec.Command("docker", "build", "-q", "-t", image, ctx).CombinedOutput(); err != nil {
			t.Fatalf("docker build: %s", out)
		}
	}
	return image
}
