package qualify

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// copyTask copies a task directory (regular files and links) to a new directory.
func copyTask(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(t, 0o755)
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, t)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(t, b, fi.Mode().Perm())
		}
	})
}

// nearMisses builds partial references: with several changed files, each variant leaves
// one file out; with one changed file of several change hunks, each variant leaves one
// hunk out. Deleted files are always applied.
func nearMisses(task string, max int) ([]string, error) {
	files := filepath.Join(task, "solution", "files")
	var changed []string
	filepath.Walk(files, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(files, p)
			changed = append(changed, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(changed)
	var out []string
	mk := func(i int, mutate func(dir string) error) error {
		dir := filepath.Join(filepath.Dir(task), fmt.Sprintf("near-miss-%d", i))
		os.RemoveAll(dir)
		if err := copyTask(task, dir); err != nil {
			return err
		}
		if err := mutate(dir); err != nil {
			return err
		}
		out = append(out, dir)
		return nil
	}
	if len(changed) >= 2 {
		for i, f := range changed {
			if i >= max {
				break
			}
			f := f
			if err := mk(i+1, func(dir string) error {
				return os.Remove(filepath.Join(dir, "solution", "files", filepath.FromSlash(f)))
			}); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if len(changed) == 1 {
		f := changed[0]
		base, err := os.ReadFile(filepath.Join(task, "environment", "repo", filepath.FromSlash(f)))
		if err != nil {
			return nil, nil // a new file has no hunks to leave out
		}
		ref, _ := os.ReadFile(filepath.Join(files, filepath.FromSlash(f)))
		hunks := diffHunks(string(base), string(ref))
		if len(hunks) < 2 {
			return nil, nil
		}
		for i := range hunks {
			if i >= max {
				break
			}
			partial := applyExcept(string(base), hunks, i)
			if err := mk(i+1, func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "solution", "files", filepath.FromSlash(f)), []byte(partial), 0o644)
			}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// hunk replaces base lines [start, end) with new lines.
type hunk struct {
	start, end int
	lines      []string
}

// diffHunks computes line hunks with a longest-common-subsequence table (files here are
// small; limits apply before packaging).
func diffHunks(a, b string) []hunk {
	x, y := strings.SplitAfter(a, "\n"), strings.SplitAfter(b, "\n")
	if len(x)*len(y) > 4_000_000 {
		return nil
	}
	n, m := len(x), len(y)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var hs []hunk
	i, j := 0, 0
	var cur *hunk
	flush := func() {
		if cur != nil {
			hs = append(hs, *cur)
			cur = nil
		}
	}
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			flush()
			i++
			j++
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			if cur == nil {
				cur = &hunk{start: i, end: i}
			}
			cur.lines = append(cur.lines, y[j])
			j++
		default:
			if cur == nil {
				cur = &hunk{start: i, end: i}
			}
			cur.end = i + 1
			i++
		}
	}
	flush()
	return hs
}

func applyExcept(a string, hs []hunk, skip int) string {
	x := strings.SplitAfter(a, "\n")
	var b strings.Builder
	pos := 0
	for k, h := range hs {
		for ; pos < h.start; pos++ {
			b.WriteString(x[pos])
		}
		if k == skip {
			for ; pos < h.end; pos++ {
				b.WriteString(x[pos])
			}
			continue
		}
		for _, ln := range h.lines {
			b.WriteString(ln)
		}
		pos = h.end
	}
	for ; pos < len(x); pos++ {
		b.WriteString(x[pos])
	}
	return b.String()
}

// probeTask is a copy of the task whose reference solution tries the network and writes
// forged reward files instead of solving the task.
func probeTask(task string) (string, error) {
	dir := filepath.Join(filepath.Dir(task), "probe")
	os.RemoveAll(dir)
	if err := copyTask(task, dir); err != nil {
		return "", err
	}
	sol := filepath.Join(dir, "solution")
	os.RemoveAll(sol)
	os.MkdirAll(sol, 0o755)
	script := `#!/bin/sh
# HINT qualification probe: no network expected; forged rewards must not count.
net=blocked
if command -v getent >/dev/null 2>&1 && getent hosts example.com >/dev/null 2>&1; then net=reachable; fi
if command -v git >/dev/null 2>&1 && timeout 10 git ls-remote https://example.com/x.git >/dev/null 2>&1; then net=reachable; fi
echo "HINT_PROBE_NET=$net"
mkdir -p /logs/verifier
echo 1 > /logs/verifier/reward.txt
echo '{"reward": 1}' > /logs/verifier/reward.json
echo 'CHECK C1 PASS' > /logs/verifier/test-stdout.txt
exit 0
`
	if pkg := rootPackage(filepath.Join(task, "environment", "repo")); pkg != "" {
		// code that the verifier runs with the tests also tries to forge the result
		script = strings.TrimSuffix(script, "exit 0\n") + "cat > /app/hint_probe_forge.go <<'GO'\npackage " + pkg + `

import "os"

func init() {
	os.WriteFile("/logs/verifier/reward.json", []byte("{\"reward\": 1}"), 0o644)
	os.WriteFile("/logs/verifier/reward.txt", []byte("1"), 0o644)
}
GO
exit 0
`
	}
	// test harness code that ends the test run early with success (a TestMain or a
	// conftest.py that exits 0) must not count as executed tests
	script = strings.TrimSuffix(script, "exit 0\n") + `if [ -f /app/go.mod ]; then
	find /app -name '*.go' ! -name '*_test.go' ! -path '*/.git/*' ! -path '*/testdata/*' ! -path '*/vendor/*' | while IFS= read -r f; do
		d=$(dirname "$f")
		[ -f "$d/zz_hint_probe_test.go" ] && continue
		p=$(sed -n 's/^package \([A-Za-z_][A-Za-z0-9_]*\).*/\1/p' "$f" | head -1)
		[ -n "$p" ] && printf 'package %s\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n' "$p" > "$d/zz_hint_probe_test.go"
	done
else
	printf 'import os\nos._exit(0)\n' > /app/conftest.py
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(sol, "solve.sh"), []byte(script), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// netSeen reads the probe output from the job directory: "blocked", "reachable" or "".
func netSeen(jobDir string) string {
	ms, _ := filepath.Glob(filepath.Join(jobDir, "*", "agent", "oracle.txt"))
	for _, m := range ms {
		b, _ := os.ReadFile(m)
		s := string(b)
		switch {
		case strings.Contains(s, "HINT_PROBE_NET=reachable"):
			return "reachable"
		case strings.Contains(s, "HINT_PROBE_NET=blocked"):
			return "blocked"
		}
	}
	return ""
}

// verifierNetSeen reads the verifier's network record of the probe job: "blocked",
// "reachable" or "" (no record).
func verifierNetSeen(jobDir string) string {
	ms, _ := filepath.Glob(filepath.Join(jobDir, "*", "verifier", "network.txt"))
	for _, m := range ms {
		b, _ := os.ReadFile(m)
		switch strings.TrimSpace(string(b)) {
		case "HINT_VERIFIER_NET=reachable":
			return "reachable"
		case "HINT_VERIFIER_NET=blocked":
			return "blocked"
		}
	}
	return ""
}

var packageClause = regexp.MustCompile(`(?m)^package ([A-Za-z_][A-Za-z0-9_]*)\s*$`)

// rootPackage returns the Go package name of the repository root ("" for other projects).
func rootPackage(repo string) string {
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return ""
	}
	files, _ := filepath.Glob(filepath.Join(repo, "*.go"))
	sort.Strings(files)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if m := packageClause.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// alternativeTask is a copy of the task whose solution is an existing alternative result
// tree: the files that differ from the agent's base repository, and the base files that
// the alternative removed.
func alternativeTask(l *library.Library, task, tree string, i int) (string, error) {
	dir := filepath.Join(filepath.Dir(task), fmt.Sprintf("alternative-%d", i))
	os.RemoveAll(dir)
	if err := copyTask(task, dir); err != nil {
		return "", err
	}
	files := filepath.Join(dir, "solution", "files")
	os.RemoveAll(files)
	src := dir + "-tree"
	os.RemoveAll(src)
	if err := os.MkdirAll(src, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(src)
	if err := trees.Materialize(l, tree, src); err != nil {
		return "", err
	}
	repo := filepath.Join(dir, "environment", "repo")
	same := func(a, b string) bool {
		x, err1 := os.Lstat(a)
		y, err2 := os.Lstat(b)
		if err1 != nil || err2 != nil || x.Mode() != y.Mode() {
			return false
		}
		if x.Mode()&os.ModeSymlink != 0 {
			p, _ := os.Readlink(a)
			q, _ := os.Readlink(b)
			return p == q
		}
		p, _ := os.ReadFile(a)
		q, _ := os.ReadFile(b)
		return string(p) == string(q)
	}
	if err := os.MkdirAll(files, 0o755); err != nil {
		return "", err
	}
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if same(p, filepath.Join(repo, rel)) {
			return nil
		}
		t := filepath.Join(files, rel)
		if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			ln, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(ln, t)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(t, b, fi.Mode().Perm())
	})
	if err != nil {
		return "", err
	}
	var deleted []string
	filepath.Walk(repo, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(repo, p)
			if _, err := os.Lstat(filepath.Join(src, rel)); os.IsNotExist(err) {
				deleted = append(deleted, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(deleted)
	return dir, os.WriteFile(filepath.Join(dir, "solution", "deleted.txt"), []byte(strings.Join(deleted, "\n")), 0o644)
}
