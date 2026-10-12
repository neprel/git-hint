package shellclass

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// arg is one command argument after static evaluation.
type arg struct {
	val    string
	static bool // false when the word has an expansion or substitution
}

// staticWord returns the literal value of w. ok is false when the value
// depends on parameter expansion, command substitution, arithmetic,
// process substitution or brace expansion. Globs stay literal.
func staticWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeLit(x.Value))
		case *syntax.SglQuoted:
			if x.Dollar {
				b.WriteString(ansiC(x.Value))
			} else {
				b.WriteString(x.Value)
			}
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(unescapeDbl(lit.Value))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

func unescapeLit(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unescapeDbl(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func ansiC(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\\`, `\`, `\'`, `'`)
	return r.Replace(s)
}

// isPathLike reports whether a static operand can name a file target.
func isPathLike(s string) bool {
	if s == "" || s == "-" || s == "{}" {
		return false
	}
	switch s {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin", "/dev/tty":
		return false
	}
	return true
}

// optSpec describes the options of one tool for operand extraction.
type optSpec struct {
	withArg map[string]bool // options that take the next argument
	twoArgs map[string]bool // options that take the next two arguments
}

func spec(withArg ...string) optSpec {
	m := map[string]bool{}
	for _, s := range withArg {
		m[s] = true
	}
	return optSpec{withArg: m}
}

// parsed holds the options and operands of a simple command.
type parsed struct {
	flags    map[string]string // option -> value ("" when none)
	operands []arg
}

func (p parsed) has(names ...string) bool {
	for _, n := range names {
		if _, ok := p.flags[n]; ok {
			return true
		}
	}
	return false
}

// shortCluster reports whether any single-dash short-option cluster
// contains letter c (for example "-rf" contains 'f').
func (p parsed) shortCluster(c byte) bool {
	for f := range p.flags {
		if len(f) > 1 && f[0] == '-' && f[1] != '-' && strings.IndexByte(f[1:], c) >= 0 {
			return true
		}
	}
	return false
}

// parseArgs splits args into options and operands. It understands "--",
// "--long=value", options listed in sp.withArg and attached short values.
func parseArgs(args []arg, sp optSpec) parsed {
	p := parsed{flags: map[string]string{}}
	endOpts := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if endOpts || !a.static || len(a.val) < 2 || a.val[0] != '-' {
			p.operands = append(p.operands, a)
			continue
		}
		if a.val == "--" {
			endOpts = true
			continue
		}
		name, val, hasEq := strings.Cut(a.val, "=")
		if hasEq && strings.HasPrefix(name, "--") {
			p.flags[name] = val
			continue
		}
		if sp.twoArgs[a.val] {
			i += 2
			p.flags[a.val] = ""
			continue
		}
		if sp.withArg[a.val] {
			if i+1 < len(args) {
				p.flags[a.val] = args[i+1].val
				i++
			} else {
				p.flags[a.val] = ""
			}
			continue
		}
		// Attached short value such as -n40 or -C3.
		if len(a.val) > 2 && a.val[1] != '-' && sp.withArg[a.val[:2]] {
			p.flags[a.val[:2]] = a.val[2:]
			continue
		}
		p.flags[a.val] = ""
	}
	return p
}

// staticOperands returns the path-like static operands and reports whether
// any operand is dynamic.
func staticOperands(ops []arg) (out []string, dynamic bool) {
	for _, o := range ops {
		if !o.static {
			dynamic = true
			continue
		}
		if o.val == "{}" {
			dynamic = true
			continue
		}
		if isPathLike(o.val) {
			out = append(out, o.val)
		}
	}
	return out, dynamic
}
