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

// C4 forgery probes (07B-I): wrong code under test tries to forge a pass from inside the
// check process. Each probe keeps the bug (Total ignores the discount), so the only honest
// result is C1 FAIL and reward 0. The verifier's stdout must carry each check line once.

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
	for name, hack := range map[string]string{"stdout-escape": goForgeOut, "reward-writer": goForgeReward, "test2json-framing": goForgeFraming, "raw-json-events": goForgeJSON} {
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
	ref := b.Record["environment"].(map[string]any)["image"].(string)
	image := "hint-verifier-test-python:" + regexp.MustCompile(`[^a-f0-9]`).ReplaceAllString(ref[strings.LastIndex(ref, ":")+1:], "")[:12]
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		ctx := t.TempDir()
		os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM "+ref+"\nRUN python -m pip install -q pytest==8.4.2\n"), 0o644)
		if out, err := exec.Command("docker", "build", "-q", "-t", image, ctx).CombinedOutput(); err != nil {
			t.Fatalf("docker build: %s", out)
		}
	}
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
