package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
)

// runVerifier runs the generated verifier of rec in a fresh container of its image (no
// network) against the files of app, as Harbor's separate verifier does. It returns the
// check lines, the test lines and the reward.
func runVerifier(t *testing.T, l *library.Library, image string, rec map[string]any, app map[string]string) (map[string]string, map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(root string, files map[string]string) {
		for p, c := range files {
			full := filepath.Join(dir, root, filepath.FromSlash(p))
			os.MkdirAll(filepath.Dir(full), 0o755)
			mode := os.FileMode(0o644)
			if strings.HasSuffix(p, ".sh") {
				mode = 0o755
			}
			if err := os.WriteFile(full, []byte(c), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("app", app)
	write("tests", untar(t, l, rec["verifier"].(map[string]any)["archive"].(string)))
	out, err := exec.Command("docker", "create", "--network", "none", image, "sh", "-c", "sh /tests/test.sh; echo REWARD $(cat /logs/verifier/reward.txt)").Output()
	if err != nil {
		t.Fatalf("docker create: %v", err)
	}
	id := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", id).Run()
	for _, cp := range [][2]string{{"app", "/app"}, {"tests", "/tests"}} {
		if b, err := exec.Command("docker", "cp", filepath.Join(dir, cp[0])+"/.", id+":"+cp[1]).CombinedOutput(); err != nil {
			t.Fatalf("docker cp: %s", b)
		}
	}
	var stdout bytes.Buffer
	c := exec.Command("docker", "start", "-a", id)
	c.Stdout = &stdout
	if err := c.Run(); err != nil {
		t.Fatalf("docker start: %v", err)
	}
	checks, tests, reward := map[string]string{}, map[string]string{}, ""
	for _, ln := range strings.Split(stdout.String(), "\n") {
		f := strings.Fields(ln)
		switch {
		case len(f) == 3 && f[0] == "CHECK":
			checks[f[1]] = f[2]
		case len(f) == 4 && f[0] == "TEST":
			tests[f[1]+" "+f[2]] = f[3]
		case len(f) == 2 && f[0] == "REWARD":
			reward = f[1]
		}
	}
	return checks, tests, reward
}

func needDocker(t *testing.T) {
	if os.Getenv("HINT_DOCKER") == "" {
		t.Skip("set HINT_DOCKER=1 to run generated verifiers in Docker")
	}
}

const goTestMainExit = "package shop\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n"

const goInitExit = "package shop\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc init() {\n\tif testing.Testing() {\n\t\tos.Exit(0)\n\t}\n}\n"

// G10-2: wrong code with a TestMain that calls os.Exit(0) passed C1 and C2. Each required
// test must have run and passed.
func TestVerifierRequiresTestExecutionGo(t *testing.T) {
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
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name   string
		app    map[string]string
		c1, c2 string
		reward string
	}{
		{"reference", with(map[string]string{"cart.go": fix}), "PASS", "PASS", "1"},
		{"base", base, "FAIL", "PASS", "0"},
		// the reviewer's case: an added test file with TestMain exiting 0
		{"testmain-exit", with(map[string]string{"zz_test.go": goTestMainExit}), "FAIL", "PASS", "0"},
		// a TestMain inside an existing test file (restored from the evaluator's copy)
		{"testmain-in-existing", with(map[string]string{"cart_test.go": goTestMainExit}), "FAIL", "PASS", "0"},
		// code under test that ends every test binary early
		{"init-exit", with(map[string]string{"hack.go": goInitExit}), "FAIL", "FAIL", "0"},
		// the hidden test name inside a skipped test is not a pass
		{"agent-test-same-name", with(map[string]string{"discount_test.go": "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {}\n"}), "FAIL", "PASS", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checks, tests, reward := runVerifier(t, l, image, b.Record, c.app)
			if checks["C1"] != c.c1 || checks["C2"] != c.c2 || reward != c.reward {
				t.Fatalf("checks %v tests %v reward %q", checks, tests, reward)
			}
			want := "PASS"
			if c.c1 == "FAIL" {
				want = "FAIL"
			}
			if tests["C1 discount_test.go::TestDiscount"] != want {
				t.Fatalf("tests %v", tests)
			}
		})
	}
}

// The same rule for Python: a conftest.py or code under test that exits early is not a
// pass.
func TestVerifierRequiresTestExecutionPython(t *testing.T) {
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
	// the verifier image of a Python task holds pytest (installed at build time)
	ref := b.Record["environment"].(map[string]any)["image"].(string)
	image := "hint-verifier-test-python:" + regexp.MustCompile(`[^a-f0-9]`).ReplaceAllString(ref[strings.LastIndex(ref, ":")+1:], "")[:12]
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		ctx := t.TempDir()
		os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM "+ref+"\nRUN python -m pip install -q pytest==8.4.2\n"), 0o644)
		if out, err := exec.Command("docker", "build", "-q", "-t", image, ctx).CombinedOutput(); err != nil {
			t.Fatalf("docker build: %s", out)
		}
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name   string
		app    map[string]string
		c1, c2 string
		reward string
	}{
		{"reference", with(map[string]string{"shop.py": fix}), "PASS", "PASS", "1"},
		{"base", base, "FAIL", "PASS", "0"},
		{"conftest-exit", with(map[string]string{"conftest.py": "import os\nos._exit(0)\n"}), "FAIL", "PASS", "0"},
		{"pytest-ini-deselect", with(map[string]string{"pytest.ini": "[pytest]\naddopts = -k nothing\n"}), "FAIL", "PASS", "0"},
		{"code-exit", with(map[string]string{"shop.py": "import os\nos._exit(0)\n"}), "FAIL", "FAIL", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checks, tests, reward := runVerifier(t, l, image, b.Record, c.app)
			if checks["C1"] != c.c1 || checks["C2"] != c.c2 || reward != c.reward {
				t.Fatalf("checks %v tests %v reward %q", checks, tests, reward)
			}
			if c.c1 == "PASS" && tests["C1 test_discount.py::test_discount"] != "PASS" {
				t.Fatalf("tests %v", tests)
			}
		})
	}
}
