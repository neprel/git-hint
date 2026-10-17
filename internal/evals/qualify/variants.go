package qualify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
