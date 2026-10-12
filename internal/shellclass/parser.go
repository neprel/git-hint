package shellclass

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// MaxNestedDepth is the number of "sh -c" levels that the walker parses.
// The plan asks for one level. A deeper string stays EXECUTE+UNKNOWN.
const MaxNestedDepth = 1

// MaxInputBytes bounds the input. Longer commands return UNKNOWN.
const MaxInputBytes = 64 << 10

type walker struct {
	a     *acc
	depth int
	funcs map[string]*syntax.Stmt
	inFn  map[string]bool
	// inXargs is true while the walker classifies the command that xargs
	// starts. Its file operands come from stdin.
	inXargs bool
	// stdin is the here-document or here-string body that feeds the
	// current simple command, if any.
	stdin string
}

// Classify returns the labels and targets of cmd. It parses cmd as Bash.
// It returns UNKNOWN for syntax that it does not parse or support.
func Classify(cmd string) Result {
	a := newAcc()
	if len(cmd) > MaxInputBytes {
		a.unknown("input_too_long")
		return a.result()
	}
	w := &walker{a: a, funcs: map[string]*syntax.Stmt{}, inFn: map[string]bool{}}
	w.script(cmd)
	return a.result()
}

func (w *walker) script(src string) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		w.a.unknown("parse_error")
		return
	}
	if len(f.Stmts) == 0 {
		w.a.add(OTHER)
		return
	}
	w.stmts(f.Stmts)
}

func (w *walker) stmts(ss []*syntax.Stmt) {
	for _, s := range ss {
		w.stmt(s)
	}
}

func (w *walker) stmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	if s.Coprocess || s.Disown {
		w.a.unknown("coprocess_or_disown")
	}
	saved := w.stdin
	w.stdin = ""
	for _, r := range s.Redirs {
		w.redirect(r)
	}
	if s.Cmd == nil {
		// Bare redirection such as "> file".
		w.a.add(OTHER)
	} else {
		w.command(s.Cmd)
	}
	w.stdin = saved
}

func (w *walker) redirect(r *syntax.Redirect) {
	if r.Word != nil {
		w.wordSubst(r.Word)
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if r.Hdoc != nil {
			if v, ok := staticWord(r.Hdoc); ok {
				w.stdin = v
			} else {
				w.stdin = hdocRaw(r.Hdoc)
			}
		}
		return
	case syntax.WordHdoc:
		if v, ok := staticWord(r.Word); ok {
			w.stdin = v
		}
		return
	case syntax.DplIn:
		return
	case syntax.DplOut:
		// ">&2" or "2>&1" duplicates a descriptor. ">& file" writes.
		v, ok := staticWord(r.Word)
		if ok && (v == "-" || isDigits(v)) {
			return
		}
	}
	v, ok := staticWord(r.Word)
	switch r.Op {
	case syntax.RdrIn:
		if !ok {
			w.a.add(READ)
			w.a.dynamic = true
		} else if isPathLike(v) {
			w.a.add(READ)
			w.a.read(v)
		}
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll,
		syntax.RdrAllClob, syntax.AppAllClob, syntax.DplOut:
		if !ok {
			w.a.add(WRITE)
			w.a.dynamic = true
		} else if isPathLike(v) {
			w.a.add(WRITE)
			w.a.write(v)
		}
	case syntax.RdrInOut:
		if ok && isPathLike(v) {
			w.a.add(READ, WRITE)
			w.a.read(v)
			w.a.write(v)
		}
	default:
		w.a.unknown("unsupported_redirect")
	}
}

func hdocRaw(wd *syntax.Word) string {
	var b strings.Builder
	for _, p := range wd.Parts {
		if l, ok := p.(*syntax.Lit); ok {
			b.WriteString(l.Value)
		}
	}
	return b.String()
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// wordSubst classifies command and process substitutions inside a word.
func (w *walker) wordSubst(wd *syntax.Word) {
	if wd == nil {
		return
	}
	syntax.Walk(wd, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			saved := w.stdin
			w.stdin = ""
			w.stmts(x.Stmts)
			w.stdin = saved
			return false
		case *syntax.ProcSubst:
			w.stmts(x.Stmts)
			return false
		}
		return true
	})
}

func (w *walker) command(c syntax.Command) {
	switch x := c.(type) {
	case *syntax.CallExpr:
		for _, as := range x.Assigns {
			if as.Value != nil {
				w.wordSubst(as.Value)
			}
			if as.Array != nil {
				for _, el := range as.Array.Elems {
					w.wordSubst(el.Value)
				}
			}
		}
		if len(x.Args) == 0 {
			w.a.add(OTHER)
			return
		}
		args := make([]arg, 0, len(x.Args))
		for _, wd := range x.Args {
			w.wordSubst(wd)
			v, ok := staticWord(wd)
			args = append(args, arg{val: v, static: ok})
		}
		w.simple(args)
	case *syntax.BinaryCmd:
		w.stmt(x.X)
		w.stmt(x.Y)
	case *syntax.Subshell:
		w.stmts(x.Stmts)
	case *syntax.Block:
		w.stmts(x.Stmts)
	case *syntax.IfClause:
		for ic := x; ic != nil; ic = ic.Else {
			w.stmts(ic.Cond)
			w.stmts(ic.Then)
		}
	case *syntax.WhileClause:
		w.stmts(x.Cond)
		w.stmts(x.Do)
	case *syntax.ForClause:
		if wi, ok := x.Loop.(*syntax.WordIter); ok {
			for _, it := range wi.Items {
				w.wordSubst(it)
			}
		}
		w.stmts(x.Do)
	case *syntax.CaseClause:
		w.wordSubst(x.Word)
		for _, it := range x.Items {
			w.stmts(it.Stmts)
		}
	case *syntax.FuncDecl:
		if x.Name != nil {
			w.funcs[x.Name.Value] = x.Body
		} else {
			w.a.unknown("anonymous_function")
		}
	case *syntax.DeclClause:
		for _, as := range x.Args {
			if as.Value != nil {
				w.wordSubst(as.Value)
			}
		}
		w.a.add(OTHER)
	case *syntax.TimeClause:
		w.stmt(x.Stmt)
	case *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause:
		syntax.Walk(x, func(n syntax.Node) bool {
			if wd, ok := n.(*syntax.Word); ok {
				w.wordSubst(wd)
				return false
			}
			return true
		})
		w.a.add(OTHER)
	default:
		w.a.unknown("unsupported_syntax")
	}
}

// nested parses a static "sh -c" script one level deep.
func (w *walker) nested(script string) {
	if w.depth >= MaxNestedDepth {
		w.a.add(EXECUTE)
		w.a.unknown("nested_shell_depth")
		return
	}
	w.depth++
	saved := w.stdin
	w.stdin = ""
	w.script(script)
	w.stdin = saved
	w.depth--
}

// callFunc classifies the body of a function declared in the same input.
func (w *walker) callFunc(name string) bool {
	body, ok := w.funcs[name]
	if !ok {
		return false
	}
	if w.inFn[name] {
		w.a.unknown("recursive_function")
		return true
	}
	w.inFn[name] = true
	w.stmt(body)
	delete(w.inFn, name)
	return true
}
