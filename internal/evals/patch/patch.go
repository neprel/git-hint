// Package patch computes line hunks between two texts and applies them to another text.
// Apply is strict: each hunk must find its old lines with their context exactly once;
// otherwise the patch does not apply (nothing is guessed).
package patch

import "strings"

// Hunk replaces base lines [Start, End) with Lines.
type Hunk struct {
	Start, End int
	Lines      []string
}

// Hunks computes line hunks with a longest-common-subsequence table. It returns nil when
// the texts are too large for the table.
func Hunks(a, b string) []Hunk {
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
	var hs []Hunk
	i, j := 0, 0
	var cur *Hunk
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
				cur = &Hunk{Start: i, End: i}
			}
			cur.Lines = append(cur.Lines, y[j])
			j++
		default:
			if cur == nil {
				cur = &Hunk{Start: i, End: i}
			}
			cur.End = i + 1
			i++
		}
	}
	flush()
	return hs
}

// ApplyExcept applies every hunk to a except hunk skip (-1 applies all).
func ApplyExcept(a string, hs []Hunk, skip int) string {
	x := strings.SplitAfter(a, "\n")
	var b strings.Builder
	pos := 0
	for k, h := range hs {
		for ; pos < h.Start; pos++ {
			b.WriteString(x[pos])
		}
		if k == skip {
			for ; pos < h.End; pos++ {
				b.WriteString(x[pos])
			}
			continue
		}
		for _, ln := range h.Lines {
			b.WriteString(ln)
		}
		pos = h.End
	}
	for ; pos < len(x); pos++ {
		b.WriteString(x[pos])
	}
	return b.String()
}

// Context is the number of unchanged lines around a hunk that Port requires.
const Context = 2

// Port applies the change from base to result onto target, a changed version of base.
// Each hunk, with Context lines of base on each side, must occur exactly once in target.
// It reports false when a hunk does not apply.
func Port(base, result, target string) (string, bool) {
	x := strings.SplitAfter(base, "\n")
	out := strings.SplitAfter(target, "\n")
	for _, h := range Hunks(base, result) {
		lo, hi := max(0, h.Start-Context), min(len(x), h.End+Context)
		old := x[lo:hi]
		repl := append(append(append([]string{}, x[lo:h.Start]...), h.Lines...), x[h.End:hi]...)
		at, n := -1, 0
		for i := 0; i+len(old) <= len(out); i++ {
			if equal(out[i:i+len(old)], old) {
				at, n = i, n+1
			}
		}
		if n != 1 {
			return "", false
		}
		out = append(append(append([]string{}, out[:at]...), repl...), out[at+len(old):]...)
	}
	return strings.Join(out, ""), true
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
