package shellclass

import (
	"path"
	"strings"
)

type handler func(w *walker, name string, args []arg)

var handlers map[string]handler

// simple classifies one simple command (name plus arguments).
func (w *walker) simple(args []arg) {
	if len(args) == 0 {
		w.a.add(OTHER)
		return
	}
	if !args[0].static {
		w.a.add(EXECUTE)
		w.a.unknown("dynamic_command_name")
		return
	}
	name := args[0].val
	if w.callFunc(name) {
		return
	}
	if h, ok := handlers[name]; ok {
		h(w, name, args[1:])
		return
	}
	if strings.HasPrefix(name, "python") && isPythonName(name) {
		pythonH(w, name, args[1:])
		return
	}
	if strings.Contains(name, "/") {
		w.a.add(EXECUTE)
		w.a.unknown("opaque_script")
		return
	}
	w.a.unknown("unknown_command")
}

// simpleOpaque is simple() for a tool started by a runner (npx, uv run).
// An unknown tool name is EXECUTE with unknown effects.
func (w *walker) simpleOpaque(args []arg) {
	if len(args) == 0 {
		w.a.add(OTHER)
		return
	}
	if args[0].static {
		if _, ok := handlers[args[0].val]; ok || isPythonName(args[0].val) {
			w.simple(args)
			return
		}
	}
	w.a.add(EXECUTE)
	w.a.unknown("opaque_tool")
}

func isPythonName(n string) bool {
	if !strings.HasPrefix(n, "python") {
		return false
	}
	for _, c := range n[len("python"):] {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

func (w *walker) readOps(ops []arg) []string {
	t, dyn := staticOperands(ops)
	if dyn {
		w.a.dynamic = true
	}
	return t
}

// ---- generic families -------------------------------------------------

func otherH(w *walker, _ string, _ []arg) { w.a.add(OTHER) }

func listH(w *walker, _ string, _ []arg) { w.a.add(LIST) }

func interactiveH(w *walker, _ string, _ []arg) { w.a.unknown("interactive_or_gui") }

func opaqueH(w *walker, _ string, _ []arg) {
	w.a.add(EXECUTE)
	w.a.unknown("opaque_runner")
}

func testH(w *walker, _ string, _ []arg)  { w.a.add(TEST) }
func buildH(w *walker, _ string, _ []arg) { w.a.add(BUILD) }

func netH(w *walker, _ string, _ []arg) { w.a.add(NETWORK) }

func netUnknownH(w *walker, _ string, _ []arg) {
	w.a.add(NETWORK)
	w.a.unknown("remote_or_package_effects")
}

// readerH: every operand is a file that the tool reads.
func readerH(sp optSpec) handler {
	return func(w *walker, name string, args []arg) {
		p := parseArgs(args, sp)
		ops := p.operands
		if name == "uniq" && len(ops) == 2 {
			if t := w.readOps(ops[1:]); len(t) > 0 {
				w.a.add(WRITE)
				w.a.write(t...)
			}
			ops = ops[:1]
		}
		if name == "sort" {
			if o, ok := p.flags["-o"]; ok && isPathLike(o) {
				w.a.add(WRITE)
				w.a.write(o)
			}
		}
		t, dyn := staticOperands(ops)
		if dyn {
			w.a.dynamic = true
		}
		if len(t) > 0 || dyn || w.inXargs {
			w.a.add(READ)
			w.a.read(t...)
			return
		}
		w.a.add(OTHER)
	}
}

// filterH: the first operand is a program or filter; the rest are files.
func filterH(sp optSpec, progFlags ...string) handler {
	return func(w *walker, name string, args []arg) {
		p := parseArgs(args, sp)
		ops := p.operands
		if !p.has(progFlags...) && len(ops) > 0 {
			prog := ops[0].val
			if name == "awk" || name == "gawk" || name == "mawk" {
				if strings.Contains(prog, "system(") || strings.Contains(prog, "| \"") || strings.Contains(prog, "> \"") {
					w.a.add(EXECUTE)
					w.a.unknown("awk_side_effects")
				}
			}
			ops = ops[1:]
		}
		t := w.readOps(ops)
		if (name == "yq") && p.has("-i", "--inplace") {
			w.a.add(READ, WRITE)
			w.a.read(t...)
			w.a.write(t...)
			return
		}
		if len(t) > 0 || len(ops) > 0 || w.inXargs {
			w.a.add(READ)
			w.a.read(t...)
			return
		}
		w.a.add(OTHER)
	}
}

// ---- wrappers -----------------------------------------------------------

func skipOpts(args []arg, sp optSpec) []arg {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.static || len(a.val) < 2 || a.val[0] != '-' {
			return args[i:]
		}
		if a.val == "--" {
			return args[i+1:]
		}
		if sp.withArg[a.val] {
			i++
		}
	}
	return nil
}

func wrapper(sp optSpec) handler {
	return func(w *walker, _ string, args []arg) {
		rest := skipOpts(args, sp)
		if len(rest) == 0 {
			w.a.add(OTHER)
			return
		}
		w.simple(rest)
	}
}

func envH(w *walker, _ string, args []arg) {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a.static && (a.val == "-u" || a.val == "--unset" || a.val == "-C" || a.val == "--chdir"):
			i += 2
		case a.static && (a.val == "-S" || strings.HasPrefix(a.val, "--split-string")):
			w.a.add(EXECUTE)
			w.a.unknown("env_split_string")
			return
		case a.static && strings.HasPrefix(a.val, "-"):
			i++
		case strings.Contains(a.val, "=") && !strings.HasPrefix(a.val, "="):
			i++
		default:
			w.simple(args[i:])
			return
		}
	}
	w.a.add(OTHER)
}

func timeoutH(w *walker, _ string, args []arg) {
	rest := skipOpts(args, spec("-s", "--signal", "-k", "--kill-after"))
	if len(rest) <= 1 {
		w.a.add(OTHER)
		return
	}
	w.simple(rest[1:]) // rest[0] is the duration
}

func commandH(w *walker, _ string, args []arg) {
	for _, a := range args {
		if a.val == "-v" || a.val == "-V" {
			w.a.add(OTHER)
			return
		}
	}
	wrapper(spec())(w, "command", args)
}

func xargsH(w *walker, _ string, args []arg) {
	sp := spec("-I", "-n", "-P", "-L", "-l", "-d", "-s", "-E", "-e", "-a",
		"--max-args", "--max-procs", "--delimiter", "--arg-file", "--replace", "--max-lines")
	repl := ""
	var cmd []arg
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.static || len(a.val) < 2 || a.val[0] != '-' {
			cmd = args[i:]
			break
		}
		if a.val == "--" {
			cmd = args[i+1:]
			break
		}
		if strings.HasPrefix(a.val, "-I") && len(a.val) > 2 {
			repl = a.val[2:]
			continue
		}
		if sp.withArg[a.val] && i+1 < len(args) {
			switch a.val {
			case "-I", "--replace":
				repl = args[i+1].val
			case "-a", "--arg-file":
				w.a.add(READ)
				w.a.read(args[i+1].val)
			}
			i++
		}
	}
	w.a.dynamic = true
	if len(cmd) == 0 {
		w.a.add(OTHER)
		return
	}
	if repl != "" {
		cmd = append([]arg(nil), cmd...)
		for i := range cmd {
			if cmd[i].val == repl {
				cmd[i] = arg{val: "{}", static: true}
			}
		}
	}
	saved := w.inXargs
	w.inXargs = true
	w.simple(cmd)
	w.inXargs = saved
}

func npxH(w *walker, _ string, args []arg) {
	w.simpleOpaque(skipOpts(args, spec("-p", "--package", "-c", "--call")))
}

func runnerH(sub string, sp optSpec, other map[string][]string) handler {
	return func(w *walker, name string, args []arg) {
		if len(args) == 0 {
			w.a.unknown("runner_no_subcommand")
			return
		}
		s := args[0].val
		if s == sub || (name == "uv" && s == "tool" && len(args) > 1 && args[1].val == "run") {
			rest := args[1:]
			if s == "tool" {
				rest = args[2:]
			}
			w.simpleOpaque(skipOpts(rest, sp))
			return
		}
		if ls, ok := other[s]; ok {
			w.a.add(ls...)
			return
		}
		if s == "--version" || s == "-V" || s == "version" {
			w.a.add(OTHER)
			return
		}
		w.a.unknown("runner_subcommand")
	}
}

// ---- shells and interpreters ---------------------------------------------

func shellH(w *walker, name string, args []arg) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.static {
			break
		}
		if a.val == "-o" || a.val == "+o" || a.val == "-O" || a.val == "+O" {
			i++
			continue
		}
		if len(a.val) > 1 && a.val[0] == '-' && a.val[1] != '-' && strings.Contains(a.val[1:], "c") {
			if i+1 >= len(args) {
				w.a.unknown("shell_c_without_script")
				return
			}
			s := args[i+1]
			if !s.static {
				w.a.add(EXECUTE)
				w.a.unknown("dynamic_shell_script")
				return
			}
			if name == "fish" {
				w.a.add(EXECUTE)
				w.a.unknown("fish_syntax")
				return
			}
			w.nested(s.val)
			return
		}
		if strings.HasPrefix(a.val, "-") {
			continue
		}
		break
	}
	w.a.add(EXECUTE)
	w.a.unknown("opaque_script")
}

func pythonH(w *walker, _ string, args []arg) {
	for i := 0; i < len(args); i++ {
		a := args[i].val
		switch {
		case a == "-V" || a == "--version":
			w.a.add(OTHER)
			return
		case a == "-m" && i+1 < len(args):
			mod := args[i+1].val
			rest := args[i+2:]
			switch mod {
			case "pytest", "unittest", "nose2", "tox":
				w.a.add(TEST)
			case "mypy", "pyright", "flake8", "pylint", "ruff":
				if mod == "ruff" {
					ruffH(w, "ruff", rest)
					return
				}
				w.a.add(BUILD)
			case "black", "isort":
				checkFormatterH(w, mod, rest)
			case "pip":
				pipH(w, "pip", rest)
			default:
				w.a.add(EXECUTE)
				w.a.unknown("python_module")
			}
			return
		case strings.HasPrefix(a, "-m") && len(a) > 2:
			w.a.add(EXECUTE)
			w.a.unknown("python_module")
			return
		}
	}
	w.a.add(EXECUTE)
	w.a.unknown("interpreter_code")
}

func nodeH(w *walker, _ string, args []arg) {
	for _, a := range args {
		switch a.val {
		case "-v", "--version":
			w.a.add(OTHER)
			return
		case "--test":
			w.a.add(TEST)
			return
		}
	}
	w.a.add(EXECUTE)
	w.a.unknown("interpreter_code")
}

func interpH(w *walker, _ string, args []arg) {
	for _, a := range args {
		if a.val == "-v" || a.val == "--version" || a.val == "-V" {
			w.a.add(OTHER)
			return
		}
	}
	w.a.add(EXECUTE)
	w.a.unknown("interpreter_code")
}

func perlH(w *walker, _ string, args []arg) {
	inplace, hasE := false, false
	var ops []arg
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.static && len(a.val) > 1 && a.val[0] == '-' && a.val[1] != '-' {
			cl := a.val[1:]
			if strings.HasPrefix(cl, "M") || strings.HasPrefix(cl, "I") {
				continue
			}
			if strings.Contains(cl, "i") {
				inplace = true
			}
			if strings.HasSuffix(cl, "e") || strings.HasSuffix(cl, "E") {
				hasE = true
				i++ // script
			}
			continue
		}
		ops = append(ops, a)
	}
	if inplace && hasE {
		t := w.readOps(ops)
		w.a.add(READ, WRITE)
		w.a.read(t...)
		w.a.write(t...)
		return
	}
	w.a.add(EXECUTE)
	w.a.unknown("interpreter_code")
}

func evalH(w *walker, _ string, _ []arg) {
	w.a.add(EXECUTE)
	w.a.unknown("eval_or_source")
}

// ---- search and list ------------------------------------------------------

var (
	grepSpec = spec("-e", "-f", "-A", "-B", "-C", "-m", "-d", "-D", "--regexp", "--file",
		"--context", "--max-count", "--include", "--exclude", "--exclude-dir", "--label")
	rgSpec = spec("-e", "-f", "-g", "--glob", "--iglob", "-t", "--type", "-T", "--type-not",
		"-A", "-B", "-C", "-m", "--max-count", "-M", "--max-columns", "-j", "--threads", "-d",
		"--max-depth", "-E", "--encoding", "-r", "--replace", "--sort", "--sortr", "--type-add",
		"--pre", "--ignore-file", "--path-separator", "--colors", "--context-separator")
	agSpec  = spec("-G", "--ignore", "-A", "-B", "-C", "-m", "--depth", "-g", "--file-search-regex")
	ackSpec = spec("--type", "-A", "-B", "-C", "-m", "--ignore-dir", "-g")
)

func grepFamily(sp optSpec) handler {
	return func(w *walker, name string, args []arg) {
		p := parseArgs(args, sp)
		if name == "rg" && p.has("--files") {
			w.a.add(LIST)
			return
		}
		if name == "rg" && p.has("--type-list") {
			w.a.add(OTHER)
			return
		}
		ops := p.operands
		if !p.has("-e", "--regexp", "-f", "--file") && len(ops) > 0 {
			ops = ops[1:]
		}
		w.a.add(SEARCH)
		w.a.read(w.readOps(ops)...)
	}
}

func findH(w *walker, _ string, args []arg) {
	w.a.add(LIST)
	for i := 0; i < len(args); i++ {
		v := args[i].val
		switch v {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j].val != ";" && args[j].val != "+" {
				j++
			}
			w.a.dynamic = true
			saved := w.stdin
			w.stdin = ""
			w.simple(args[i+1 : j])
			w.stdin = saved
			i = j
		case "-delete":
			w.a.add(DELETE)
			w.a.dynamic = true
		case "-fprint", "-fprint0", "-fprintf", "-fls":
			if i+1 < len(args) {
				w.a.add(WRITE)
				w.a.write(args[i+1].val)
				i++
			}
		}
	}
}

func fdH(w *walker, _ string, args []arg) {
	w.a.add(LIST)
	for i, a := range args {
		if a.val == "-x" || a.val == "--exec" || a.val == "-X" || a.val == "--exec-batch" {
			w.a.dynamic = true
			w.simple(args[i+1:])
			return
		}
	}
}

// ---- writers and deleters -------------------------------------------------

func writerH(sp optSpec, dropFirst bool) handler {
	return func(w *walker, _ string, args []arg) {
		p := parseArgs(args, sp)
		ops := p.operands
		if dropFirst && !p.has("--reference") && len(ops) > 0 {
			ops = ops[1:]
		}
		w.a.add(WRITE)
		w.a.write(w.readOps(ops)...)
	}
}

func teeH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec())
	t := w.readOps(p.operands)
	if len(t) == 0 && len(p.operands) == 0 {
		w.a.add(OTHER)
		return
	}
	w.a.add(WRITE)
	w.a.write(t...)
}

func deleteH(sp optSpec) handler {
	return func(w *walker, _ string, args []arg) {
		p := parseArgs(args, sp)
		w.a.add(DELETE)
		t := w.readOps(p.operands)
		if len(p.operands) == 0 {
			w.a.dynamic = true
		}
		w.a.write(t...)
	}
}

func lnH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-t", "-S", "--suffix", "--target-directory"))
	w.a.add(WRITE)
	t := w.readOps(p.operands)
	switch {
	case p.has("-t", "--target-directory"):
		w.a.write(p.flags["-t"] + p.flags["--target-directory"])
	case len(t) == 1:
		w.a.write(path.Base(t[0]))
	case len(t) > 1:
		w.a.write(t[len(t)-1])
	}
}

func copyH(sp optSpec) handler {
	return func(w *walker, name string, args []arg) {
		p := parseArgs(args, sp)
		if name == "install" && p.has("-d", "--directory") {
			w.a.add(WRITE)
			w.a.write(w.readOps(p.operands)...)
			return
		}
		w.a.add(READ, WRITE)
		dst := p.flags["-t"] + p.flags["--target-directory"]
		ops := p.operands
		if dst == "" && len(ops) > 0 {
			last := ops[len(ops)-1]
			ops = ops[:len(ops)-1]
			if last.static && isPathLike(last.val) {
				dst = last.val
			} else {
				w.a.dynamic = true
			}
		}
		if dst != "" {
			w.a.write(dst)
		}
		w.a.read(w.readOps(ops)...)
	}
}

func mvH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-t", "-S", "--suffix", "--target-directory"))
	w.a.add(WRITE)
	w.a.write(w.readOps(p.operands)...)
	if t := p.flags["-t"] + p.flags["--target-directory"]; t != "" {
		w.a.write(t)
	}
}

func ddH(w *walker, _ string, args []arg) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a.val, "if="); ok && isPathLike(v) && !strings.HasPrefix(v, "/dev/") {
			w.a.add(READ)
			w.a.read(v)
		}
		if v, ok := strings.CutPrefix(a.val, "of="); ok && isPathLike(v) && !strings.HasPrefix(v, "/dev/") {
			w.a.add(WRITE)
			w.a.write(v)
		}
	}
	w.a.add(OTHER)
}

func tarH(w *walker, _ string, args []arg) {
	mode, archive := byte(0), ""
	var ops []arg
	cluster := func(cl string, i *int) {
		for k := 0; k < len(cl); k++ {
			switch c := cl[k]; c {
			case 'c', 'x', 't', 'r', 'u':
				mode = c
			case 'f':
				if k+1 < len(cl) {
					archive = cl[k+1:]
				} else if *i+1 < len(args) {
					*i++
					archive = args[*i].val
				}
				return
			case 'C':
				*i++
				return
			}
		}
	}
	for i := 0; i < len(args); i++ {
		v := args[i].val
		switch {
		case strings.HasPrefix(v, "--file="):
			archive = strings.TrimPrefix(v, "--file=")
		case v == "--file":
			if i+1 < len(args) {
				i++
				archive = args[i].val
			}
		case v == "--create":
			mode = 'c'
		case v == "--extract" || v == "--get":
			mode = 'x'
		case v == "--list":
			mode = 't'
		case v == "--directory" || v == "-C":
			i++
		case strings.HasPrefix(v, "--"):
		case strings.HasPrefix(v, "-") && len(v) > 1:
			cluster(v[1:], &i)
		case i == 0 && args[i].static:
			cluster(v, &i)
		default:
			ops = append(ops, args[i])
		}
	}
	hasArchive := archive != "" && archive != "-"
	switch mode {
	case 'c', 'r', 'u':
		w.a.add(READ, WRITE)
		w.a.read(w.readOps(ops)...)
		if hasArchive {
			w.a.write(archive)
		} else {
			w.a.add(OTHER)
		}
	case 'x':
		w.a.add(WRITE)
		w.a.dynamic = true
		if hasArchive {
			w.a.add(READ)
			w.a.read(archive)
		}
	case 't':
		if hasArchive {
			w.a.add(READ)
			w.a.read(archive)
		} else {
			w.a.add(OTHER)
		}
	default:
		w.a.unknown("tar_mode")
	}
}

func unzipH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-d", "-x", "-P"))
	t := w.readOps(p.operands)
	if len(t) > 0 {
		w.a.add(READ)
		w.a.read(t[0])
	}
	if p.has("-l", "-t", "-v", "-Z", "-z") {
		return
	}
	w.a.add(WRITE)
	if d := p.flags["-d"]; d != "" {
		w.a.write(d)
	} else {
		w.a.dynamic = true
	}
}

// ---- edits and formatters --------------------------------------------------

func sedH(w *walker, _ string, args []arg) {
	clean := make([]arg, 0, len(args))
	inplace := false
	for i := 0; i < len(args); i++ {
		v := args[i].val
		if args[i].static && (v == "-i" || strings.HasPrefix(v, "--in-place") ||
			(len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.Contains(v[1:], "i") && !strings.ContainsAny(v[1:], "ef"))) {
			inplace = true
			if v == "-i" && i+1 < len(args) && args[i+1].static && args[i+1].val == "" {
				i++ // BSD "sed -i ''"
			}
			continue
		}
		clean = append(clean, args[i])
	}
	p := parseArgs(clean, spec("-e", "--expression", "-f", "--file", "-l", "--line-length"))
	ops := p.operands
	if !p.has("-e", "--expression", "-f", "--file") && len(ops) > 0 {
		ops = ops[1:]
	}
	t := w.readOps(ops)
	if inplace {
		w.a.add(READ, WRITE)
		w.a.read(t...)
		w.a.write(t...)
		if len(ops) == 0 {
			w.a.dynamic = true
		}
		return
	}
	if len(ops) > 0 {
		w.a.add(READ)
		w.a.read(t...)
		return
	}
	w.a.add(OTHER)
}

// formatterH: writes when one of writeFlags is set (or always when
// writeFlags is nil and no checkFlags is set).
func formatterH(sp optSpec, writeFlags, checkFlags []string) handler {
	return func(w *walker, _ string, args []arg) {
		p := parseArgs(args, sp)
		t := w.readOps(p.operands)
		writes := false
		if writeFlags == nil {
			writes = !p.has(checkFlags...)
		} else {
			writes = p.has(writeFlags...)
		}
		w.a.add(READ)
		w.a.read(t...)
		if writes {
			w.a.add(WRITE)
			w.a.write(t...)
			if len(p.operands) == 0 {
				w.a.dynamic = true
			}
		}
	}
}

func checkFormatterH(w *walker, name string, args []arg) {
	formatterH(spec("-l", "--line-length", "-t", "--target-version", "--config", "--settings-path",
		"--include", "--exclude", "--extend-exclude", "--profile", "-p"),
		nil, []string{"--check", "--diff", "--check-only", "-c"})(w, name, args)
}

func ruffH(w *walker, _ string, args []arg) {
	if len(args) == 0 {
		w.a.unknown("ruff_no_subcommand")
		return
	}
	sub, rest := args[0].val, args[1:]
	sp := spec("--config", "--select", "--ignore", "--extend-select", "--line-length", "--target-version", "--output-format", "--exclude")
	switch sub {
	case "format":
		formatterH(sp, nil, []string{"--check", "--diff"})(w, "ruff", rest)
	case "check":
		w.a.add(BUILD)
		p := parseArgs(rest, sp)
		if p.has("--fix") {
			t := w.readOps(p.operands)
			w.a.add(READ, WRITE)
			w.a.read(t...)
			w.a.write(t...)
		}
	case "--version", "version":
		w.a.add(OTHER)
	default:
		w.a.add(BUILD)
	}
}

func eslintH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-c", "--config", "--ext", "--ignore-path", "--format", "-f", "-o",
		"--output-file", "--rulesdir", "--parser", "--max-warnings", "--cache-location", "--env",
		"--global", "--plugin", "--rule"))
	w.a.add(BUILD)
	if p.has("--fix") {
		t := w.readOps(p.operands)
		w.a.add(READ, WRITE)
		w.a.read(t...)
		w.a.write(t...)
	}
	if o := p.flags["-o"] + p.flags["--output-file"]; o != "" {
		w.a.add(WRITE)
		w.a.write(o)
	}
}

// ---- language toolchains ----------------------------------------------------

// goFlagWrites returns targets of go flags that name output files.
func goFlagWrites(args []arg) (out []string) {
	outFlags := map[string]bool{"o": true, "coverprofile": true, "cpuprofile": true,
		"memprofile": true, "blockprofile": true, "mutexprofile": true, "trace": true}
	for i, a := range args {
		if !strings.HasPrefix(a.val, "-") {
			continue
		}
		n, v, eq := strings.Cut(strings.TrimLeft(a.val, "-"), "=")
		if !outFlags[n] {
			continue
		}
		if !eq && i+1 < len(args) {
			v = args[i+1].val
		}
		if isPathLike(v) {
			out = append(out, v)
		}
	}
	return out
}

func goH(w *walker, _ string, args []arg) {
	if len(args) == 0 {
		w.a.add(OTHER)
		return
	}
	sub, rest := args[0].val, args[1:]
	switch sub {
	case "test":
		w.a.add(TEST)
	case "build", "install", "vet":
		w.a.add(BUILD)
	case "run", "generate", "tool":
		w.a.add(EXECUTE)
		w.a.unknown("opaque_runner")
		return
	case "fmt":
		w.a.add(READ, WRITE)
		return
	case "get":
		w.a.add(NETWORK, WRITE)
		w.a.write("go.mod", "go.sum")
		return
	case "mod":
		s := ""
		if len(rest) > 0 {
			s = rest[0].val
		}
		switch s {
		case "tidy", "vendor":
			w.a.add(NETWORK, WRITE)
			w.a.write("go.mod", "go.sum")
		case "download":
			w.a.add(NETWORK)
		case "init", "edit":
			w.a.add(WRITE)
			w.a.write("go.mod")
		default:
			w.a.add(OTHER)
		}
		return
	case "list":
		w.a.add(LIST)
		return
	case "version", "env", "help", "doc":
		w.a.add(OTHER)
		return
	default:
		w.a.unknown("go_subcommand")
		return
	}
	if t := goFlagWrites(rest); len(t) > 0 {
		w.a.add(WRITE)
		w.a.write(t...)
	}
}

func cargoH(w *walker, _ string, args []arg) {
	rest := skipOpts(args, spec("-C", "--manifest-path", "-Z"))
	if len(rest) == 0 {
		w.a.add(OTHER)
		return
	}
	switch rest[0].val {
	case "test", "nextest", "bench":
		w.a.add(TEST)
	case "build", "check", "clippy", "doc", "b", "c":
		w.a.add(BUILD)
	case "run", "r":
		w.a.add(EXECUTE)
		w.a.unknown("opaque_runner")
	case "fmt":
		w.a.add(READ)
		if !parseArgs(rest[1:], spec()).has("--check") {
			w.a.add(WRITE)
		}
	case "add", "remove", "update", "fetch", "generate-lockfile":
		w.a.add(NETWORK, WRITE)
	case "install":
		w.a.add(NETWORK)
	case "clean":
		w.a.add(DELETE)
	case "--version", "-V", "version":
		w.a.add(OTHER)
	default:
		w.a.unknown("cargo_subcommand")
	}
}

func npmH(w *walker, name string, args []arg) {
	rest := skipOpts(args, spec("--prefix", "-C", "--dir", "--filter", "-F", "--cwd", "-w", "--workspace"))
	if len(rest) == 0 {
		if name == "yarn" {
			w.a.add(NETWORK, WRITE)
			return
		}
		if len(args) > 0 && (args[0].val == "--version" || args[0].val == "-v") {
			w.a.add(OTHER)
			return
		}
		w.a.unknown("npm_no_subcommand")
		return
	}
	switch rest[0].val {
	case "test", "t", "tst":
		w.a.add(TEST)
	case "install", "i", "ci", "add", "update", "upgrade", "up", "uninstall", "remove", "rm", "un":
		w.a.add(NETWORK, WRITE)
	case "exec", "x", "dlx":
		w.simpleOpaque(skipOpts(rest[1:], spec("-p", "--package", "-c")))
	case "--version", "-v", "version":
		w.a.add(OTHER)
	case "ls", "list", "outdated", "view", "info", "why":
		w.a.add(OTHER)
	case "publish":
		w.a.add(NETWORK)
	default:
		// "run x", "start" and bare yarn/pnpm script names are opaque.
		if name == "npm" && rest[0].val != "run" && rest[0].val != "run-script" && rest[0].val != "start" {
			w.a.unknown("npm_subcommand")
			return
		}
		w.a.add(EXECUTE)
		w.a.unknown("package_script")
	}
}

func bunH(w *walker, _ string, args []arg) {
	if len(args) == 0 {
		w.a.unknown("bun_no_subcommand")
		return
	}
	switch args[0].val {
	case "test":
		w.a.add(TEST)
	case "install", "add", "remove", "i":
		w.a.add(NETWORK, WRITE)
	case "x":
		w.simpleOpaque(args[1:])
	default:
		w.a.add(EXECUTE)
		w.a.unknown("package_script")
	}
}

func subcmdH(m map[string][]string, fallback string) handler {
	return func(w *walker, _ string, args []arg) {
		for _, a := range args {
			if ls, ok := m[a.val]; ok {
				w.a.add(ls...)
				return
			}
		}
		w.a.unknown(fallback)
	}
}

func dotnetH(w *walker, _ string, args []arg) {
	subcmdH(map[string][]string{
		"test": {TEST}, "build": {BUILD}, "publish": {BUILD}, "restore": {NETWORK},
		"format": {READ, WRITE}, "--version": {OTHER}, "--info": {OTHER},
	}, "dotnet_subcommand")(w, "dotnet", args)
	if len(args) > 0 && args[0].val == "run" {
		w.a.add(EXECUTE)
	}
}

func ccH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-o", "-I", "-L", "-l", "-D", "-include", "-x", "-MF"))
	w.a.add(BUILD)
	if o := p.flags["-o"]; isPathLike(o) {
		w.a.add(WRITE)
		w.a.write(o)
	}
	var src []string
	for _, t := range w.readOps(p.operands) {
		switch path.Ext(t) {
		case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".s", ".S", ".m":
			src = append(src, t)
		}
	}
	if len(src) > 0 {
		w.a.add(READ)
		w.a.read(src...)
	}
}

func denoH(w *walker, _ string, args []arg) {
	if len(args) == 0 {
		w.a.unknown("interactive_or_gui")
		return
	}
	switch args[0].val {
	case "test", "bench":
		w.a.add(TEST)
	case "fmt":
		w.a.add(READ)
		if !parseArgs(args[1:], spec()).has("--check") {
			w.a.add(WRITE)
		}
	case "lint", "check":
		w.a.add(BUILD)
	case "--version", "-V":
		w.a.add(OTHER)
	default:
		w.a.add(EXECUTE)
		w.a.unknown("interpreter_code")
	}
}

func pipH(w *walker, _ string, args []arg) {
	if len(args) == 0 {
		w.a.unknown("pip_no_subcommand")
		return
	}
	switch args[0].val {
	case "install", "download":
		w.a.add(NETWORK)
		p := parseArgs(args[1:], spec("-r", "--requirement", "-c", "--constraint", "-e", "--editable",
			"-t", "--target", "-i", "--index-url"))
		for _, f := range []string{"-r", "--requirement", "-c", "--constraint"} {
			if v, ok := p.flags[f]; ok && isPathLike(v) {
				w.a.add(READ)
				w.a.read(v)
			}
		}
	case "uninstall":
		w.a.unknown("pip_uninstall")
	case "freeze", "list", "show", "--version", "-V", "check":
		w.a.add(OTHER)
	default:
		w.a.unknown("pip_subcommand")
	}
}

// ---- network ------------------------------------------------------------

func curlH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-o", "--output", "-d", "--data", "--data-binary", "--data-raw",
		"--data-urlencode", "--data-ascii", "-F", "--form", "-T", "--upload-file", "-H", "--header",
		"-X", "--request", "-u", "--user", "-A", "--user-agent", "-e", "--referer", "-b", "--cookie",
		"-c", "--cookie-jar", "-K", "--config", "-w", "--write-out", "-m", "--max-time",
		"--connect-timeout", "-x", "--proxy", "--retry", "--retry-delay", "--cacert", "--cert",
		"--key", "-E", "--resolve", "-D", "--dump-header", "--url", "-r", "--range"))
	w.a.add(NETWORK)
	for _, f := range []string{"-o", "--output", "-D", "--dump-header", "-c", "--cookie-jar"} {
		if v, ok := p.flags[f]; ok && isPathLike(v) {
			w.a.add(WRITE)
			w.a.write(v)
		}
	}
	if p.has("-O", "--remote-name", "--remote-name-all") || p.shortCluster('O') {
		w.a.add(WRITE)
		w.a.dynamic = true
	}
	for _, f := range []string{"-d", "--data", "--data-binary", "--data-raw", "--data-urlencode", "--data-ascii", "-F", "--form"} {
		v := p.flags[f]
		if i := strings.Index(v, "@"); i >= 0 && isPathLike(v[i+1:]) {
			w.a.add(READ)
			w.a.read(v[i+1:])
		}
	}
	for _, f := range []string{"-T", "--upload-file", "-K", "--config"} {
		if v, ok := p.flags[f]; ok && isPathLike(v) {
			w.a.add(READ)
			w.a.read(v)
		}
	}
}

func wgetH(w *walker, _ string, args []arg) {
	w.a.add(NETWORK)
	out, set := "", false
	for i := 0; i < len(args); i++ {
		v := args[i].val
		switch {
		case v == "--spider":
			return
		case strings.HasPrefix(v, "--output-document="):
			out, set = strings.TrimPrefix(v, "--output-document="), true
		case v == "--output-document":
			if i+1 < len(args) {
				i++
				out, set = args[i].val, true
			}
		case v == "-i" || v == "--input-file":
			if i+1 < len(args) {
				i++
				w.a.add(READ)
				w.a.read(args[i].val)
			}
		case v == "-P" || v == "--directory-prefix" || v == "-o" || v == "-a" || v == "-t" || v == "-T" || v == "-U" || v == "--header" || v == "-e":
			i++
		case len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.Contains(v, "O"):
			k := strings.IndexByte(v, 'O')
			if k+1 < len(v) {
				out = v[k+1:]
			} else if i+1 < len(args) {
				i++
				out = args[i].val
			}
			set = true
		}
	}
	if set && (out == "-" || !isPathLike(out)) {
		return
	}
	w.a.add(WRITE)
	if set {
		w.a.write(out)
	} else {
		w.a.dynamic = true
	}
}

func copyRemoteH(w *walker, name string, args []arg) {
	p := parseArgs(args, spec("-e", "-i", "-P", "-p", "-o", "-F", "--exclude", "--include", "-l"))
	remote := false
	var local []string
	ops := p.operands
	for _, o := range ops {
		if strings.Contains(o.val, ":") {
			remote = true
		}
	}
	if remote {
		w.a.add(NETWORK)
	}
	if len(ops) < 2 {
		w.a.unknown(name + "_operands")
		return
	}
	for _, o := range ops[:len(ops)-1] {
		if o.static && !strings.Contains(o.val, ":") {
			local = append(local, o.val)
		}
	}
	if len(local) > 0 {
		w.a.add(READ)
		w.a.read(local...)
	}
	if d := ops[len(ops)-1]; d.static && !strings.Contains(d.val, ":") {
		w.a.add(WRITE)
		w.a.write(d.val)
	}
}

func kubectlH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-f", "--filename", "-n", "--namespace", "-o", "--output", "-l", "--context", "-c"))
	w.a.add(NETWORK)
	for _, f := range []string{"-f", "--filename"} {
		if v, ok := p.flags[f]; ok && isPathLike(v) {
			w.a.add(READ)
			w.a.read(v)
		}
	}
}

func dbH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-f", "--file", "-c", "--command", "-h", "-U", "-d", "-p", "-u", "-e"))
	w.a.add(NETWORK)
	w.a.unknown("database_effects")
	for _, f := range []string{"-f", "--file"} {
		if v, ok := p.flags[f]; ok && isPathLike(v) {
			w.a.add(READ)
			w.a.read(v)
		}
	}
}

func pkgMgrH(w *walker, _ string, args []arg) {
	for _, a := range args {
		switch a.val {
		case "install", "upgrade", "update", "remove", "uninstall", "add", "reinstall", "purge":
			w.a.add(NETWORK)
			w.a.unknown("system_package_effects")
			return
		case "list", "search", "info", "show", "--version":
			w.a.add(OTHER)
			return
		}
	}
	w.a.unknown("package_manager_subcommand")
}

func protocH(w *walker, _ string, args []arg) {
	var src []string
	out := false
	for _, a := range args {
		if strings.HasPrefix(a.val, "--") && strings.Contains(a.val, "_out") {
			out = true
		} else if strings.HasSuffix(a.val, ".proto") {
			src = append(src, a.val)
		}
	}
	if len(src) > 0 {
		w.a.add(READ)
		w.a.read(src...)
	}
	if out {
		w.a.add(WRITE)
		w.a.dynamic = true
	}
}

// ---- VCS and patches --------------------------------------------------------

func hasAny(args []arg, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a.val == n || strings.HasPrefix(a.val, n+"=") {
				return true
			}
		}
	}
	return false
}

func afterDashDash(args []arg) ([]arg, bool) {
	for i, a := range args {
		if a.val == "--" {
			return args[i+1:], true
		}
	}
	return nil, false
}

// gitOpts splits the arguments of a git subcommand at the "--" boundary: the options and
// operands before it (everything after it is a path, even when it looks like an option).
func gitOpts(rest []arg) (opts, operands []string) {
	for _, a := range rest {
		if a.val == "--" {
			break
		}
		if strings.HasPrefix(a.val, "-") && a.val != "-" {
			opts = append(opts, a.val)
		} else {
			operands = append(operands, a.val)
		}
	}
	return opts, operands
}

// gitPatchState reads the patch switches in order; the last one wins: 1 a patch was requested
// (-p, -u, --patch*, or a short cluster with p or u such as -pw; the pickaxe options -S and -G
// take a string and are no cluster), -1 it was switched off (--no-patch, -s), 0 neither.
func gitPatchState(opts []string) int {
	st := 0
	for _, v := range opts {
		switch {
		case strings.HasPrefix(v, "--patch") || v == "-p" || v == "-u":
			st = 1
		case v == "--no-patch" || v == "-s":
			st = -1
		case len(v) > 1 && v[0] == '-' && v[1] != '-' && v[1] != 'S' && v[1] != 'G' && strings.ContainsAny(v[1:], "pu"):
			st = 1
		}
	}
	return st
}

// gitPrintsContent says whether a git subcommand prints repository content (a patch, a blob or
// annotated lines). A suppression is accepted only when a listed option before "--" proves it;
// an unknown option combination keeps the read.
func gitPrintsContent(sub string, rest []arg) bool {
	opts, operands := gitOpts(rest)
	has := func(names ...string) bool {
		for _, o := range opts {
			for _, n := range names {
				if o == n || strings.HasPrefix(o, n+"=") {
					return true
				}
			}
		}
		return false
	}
	patch := gitPatchState(opts)
	summary := has("--stat", "--shortstat", "--numstat", "--name-only", "--name-status", "--summary", "--raw", "--dirstat", "--compact-summary")
	switch sub {
	case "show":
		for _, o := range operands {
			if strings.Contains(o, ":") {
				return true // REV:PATH prints a file
			}
		}
		// show --quiet suppresses the patch only when no patch is requested (checked with git 2.56)
		return patch == 1 || (patch == 0 && !summary && !has("--quiet"))
	case "diff":
		if has("--quiet") {
			return false // no output, only the exit status
		}
		return patch == 1 || (patch == 0 && !summary)
	case "log", "whatchanged":
		return patch == 1
	case "blame", "annotate":
		return true
	case "cat-file":
		if has("-p") {
			return true
		}
		return len(operands) > 0 && (operands[0] == "blob" || operands[0] == "tree" || operands[0] == "commit" || operands[0] == "tag")
	case "range-diff":
		return patch == 1 || (patch == 0 && !has("--stat"))
	}
	return false
}

func gitH(w *walker, _ string, args []arg) {
	w.a.add(VCS)
	i := 0
	for i < len(args) {
		v := args[i].val
		if v == "-C" || v == "-c" || v == "--git-dir" || v == "--work-tree" || v == "--namespace" {
			i += 2
			continue
		}
		if strings.HasPrefix(v, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return
	}
	sub, rest := args[i].val, args[i+1:]
	if gitPrintsContent(sub, rest) {
		w.a.add(READ) // repository content reaches the output; the paths and revisions are not resolved
	}
	switch sub {
	case "status", "diff", "show", "blame", "branch", "rev-parse", "describe", "remote", "config",
		"tag", "shortlog", "reflog", "cat-file", "show-ref", "rev-list", "merge-base", "name-rev",
		"help", "version", "fsck", "count-objects", "add", "commit", "notes", "gc", "prune",
		"update-index", "range-diff", "for-each-ref", "symbolic-ref", "whatchanged", "difftool", "format-patch":
		if sub == "format-patch" {
			w.a.add(WRITE)
			w.a.dynamic = true
		}
	case "log":
		for _, a := range rest {
			if strings.HasPrefix(a.val, "-S") || strings.HasPrefix(a.val, "-G") {
				w.a.add(SEARCH)
			}
		}
	case "grep":
		w.a.add(SEARCH)
	case "ls-files", "ls-tree":
		w.a.add(LIST)
	case "reset":
		if hasAny(rest, "--hard", "--merge", "--keep") {
			w.a.add(WRITE)
		}
	case "checkout":
		if hasAny(rest, "-b", "-B", "--orphan") {
			return
		}
		if paths, ok := afterDashDash(rest); ok {
			w.a.add(WRITE)
			w.a.write(w.readOps(paths)...)
			return
		}
		ops := parseArgs(rest, spec()).operands
		w.a.add(WRITE)
		if len(ops) >= 2 {
			w.a.write(w.readOps(ops[1:])...)
		} else if len(ops) == 1 && ops[0].val == "." {
			w.a.write(".")
		}
	case "switch":
		if !hasAny(rest, "-c", "-C", "--create", "--force-create") {
			w.a.add(WRITE)
		}
	case "restore":
		p := parseArgs(rest, spec("-s", "--source"))
		if p.has("--staged", "-S") && !p.has("--worktree", "-W") {
			return
		}
		w.a.add(WRITE)
		w.a.write(w.readOps(p.operands)...)
	case "stash":
		s := ""
		if len(rest) > 0 {
			s = rest[0].val
		}
		switch s {
		case "list", "show", "drop", "clear", "create", "store":
		default:
			w.a.add(WRITE)
		}
	case "merge", "rebase", "cherry-pick", "revert", "am", "bisect":
		w.a.add(WRITE)
	case "pull":
		w.a.add(NETWORK, WRITE)
	case "fetch", "push", "ls-remote":
		w.a.add(NETWORK)
	case "clone":
		w.a.add(NETWORK, WRITE)
		ops := parseArgs(rest, spec("-b", "--branch", "--depth", "-o", "--origin", "--reference", "-c", "--config", "--filter"))
		if len(ops.operands) >= 2 {
			w.a.write(w.readOps(ops.operands[1:2])...)
		} else {
			w.a.dynamic = true
		}
	case "rm":
		if hasAny(rest, "--cached", "-n", "--dry-run") {
			return
		}
		w.a.add(DELETE)
		w.a.write(w.readOps(parseArgs(rest, spec()).operands)...)
	case "mv":
		w.a.add(WRITE)
		w.a.write(w.readOps(parseArgs(rest, spec()).operands)...)
	case "clean":
		p := parseArgs(rest, spec("-e", "--exclude"))
		if p.has("-n", "--dry-run") || p.shortCluster('n') {
			return
		}
		w.a.add(DELETE)
		w.a.dynamic = true
	case "worktree":
		if len(rest) == 0 {
			return
		}
		p := parseArgs(rest[1:], spec("-b", "-B", "--reason"))
		switch rest[0].val {
		case "add":
			w.a.add(WRITE)
			if len(p.operands) > 0 {
				w.a.write(w.readOps(p.operands[:1])...)
			}
		case "remove":
			w.a.add(DELETE)
			w.a.write(w.readOps(p.operands)...)
		}
	case "apply":
		w.patchLike(rest, true)
	case "submodule":
		w.a.add(NETWORK, WRITE)
	default:
		w.a.unknown("git_subcommand")
	}
}

// patchLike handles "git apply" (git=true) and "patch".
func (w *walker) patchLike(args []arg, git bool) {
	sp := spec("-p", "-i", "--input", "-d", "--directory", "-o", "--output", "-F", "-B", "-z", "-r",
		"-V", "-D", "--exclude", "--include", "-C")
	p := parseArgs(args, sp)
	strip := 0
	if git {
		strip = 1
	}
	if v, ok := p.flags["-p"]; ok && isDigits(v) {
		strip = int(v[0] - '0')
	}
	checkOnly := p.has("--dry-run", "--check", "--stat", "--numstat", "--summary") && !p.has("--apply")
	if git && p.has("--cached") && !p.has("--index") {
		checkOnly = true
	}
	ops := w.readOps(p.operands)
	var patchFiles []string
	writes, deletes := []string{}, []string{}
	if git {
		patchFiles = ops
	} else {
		if len(ops) > 0 {
			writes = append(writes, ops[0])
		}
		if len(ops) > 1 {
			patchFiles = append(patchFiles, ops[1])
		}
		for _, f := range []string{"-i", "--input"} {
			if v, ok := p.flags[f]; ok && isPathLike(v) {
				patchFiles = append(patchFiles, v)
			}
		}
	}
	if len(patchFiles) > 0 {
		w.a.add(READ)
		w.a.read(patchFiles...)
	}
	if len(patchFiles) == 0 && w.stdin != "" && len(writes) == 0 {
		writes, deletes = unifiedDiffTargets(w.stdin, strip)
	}
	if checkOnly {
		return
	}
	w.a.add(WRITE)
	w.a.write(writes...)
	if len(deletes) > 0 {
		w.a.add(DELETE)
		w.a.write(deletes...)
	}
	if len(writes)+len(deletes) == 0 {
		w.a.dynamic = true
	}
}

func patchH(w *walker, _ string, args []arg) { w.patchLike(args, false) }

func applyPatchH(w *walker, _ string, args []arg) {
	body := w.stdin
	if body == "" && len(args) > 0 && args[0].static {
		body = args[0].val
	}
	writes, deletes, ok := codexPatchTargets(body)
	if !ok {
		w.a.add(WRITE)
		w.a.dynamic = true
		return
	}
	if len(writes) > 0 {
		w.a.add(WRITE)
		w.a.write(writes...)
	}
	if len(deletes) > 0 {
		w.a.add(DELETE)
		w.a.write(deletes...)
	}
	if len(writes)+len(deletes) == 0 {
		w.a.add(WRITE)
		w.a.dynamic = true
	}
}

func diffH(w *walker, _ string, args []arg) {
	p := parseArgs(args, spec("-U", "-C", "-x", "-X", "-I", "--label", "-F", "-L", "-W"))
	w.a.add(READ)
	w.a.read(w.readOps(p.operands)...)
}

func init() {
	handlers = map[string]handler{}
	reg := func(h handler, names ...string) {
		for _, n := range names {
			handlers[n] = h
		}
	}
	reg(otherH, "echo", "printf", "pwd", "which", "whereis", "type", "date", "whoami", "id",
		"uname", "hostname", "sleep", "true", "false", ":", "cd", "pushd", "popd", "export",
		"unset", "set", "alias", "unalias", "read", "test", "[", "basename", "dirname", "realpath",
		"readlink", "ps", "df", "free", "lsof", "kill", "pkill", "killall", "uptime", "seq", "yes",
		"clear", "history", "jobs", "wait", "trap", "shift", "return", "exit", "umask", "ulimit",
		"hash", "printenv", "tty", "nproc", "getconf", "locale", "shopt", "disown", "fg", "bg", "tput")
	reg(listH, "ls", "tree", "du", "stat", "exa", "eza", "lsd", "locate")
	reg(interactiveH, "vim", "vi", "nvim", "nano", "emacs", "code", "subl", "open", "xdg-open", "top", "htop")
	reg(readerH(spec()), "cat", "tac", "more", "wc", "md5sum", "md5", "sha1sum", "sha256sum",
		"base64", "rev", "fold", "fmt", "expand", "comm", "zcat")
	reg(readerH(spec("-p", "-x", "-y", "-b", "-h", "-j", "-k", "-o", "-O", "-P", "-T", "-z")), "less")
	reg(readerH(spec("-l", "--language", "-r", "--line-range", "-H", "--highlight-line", "--style",
		"--theme", "--paging", "-m", "--map-syntax")), "bat", "batcat")
	reg(readerH(spec("-n", "-c")), "head")
	reg(readerH(spec("-n", "-c", "-s", "--pid")), "tail")
	reg(readerH(spec("-b", "-s", "-v", "-w", "-i", "-n", "-d")), "nl")
	reg(readerH(spec("-A", "-j", "-N", "-t", "-w")), "od")
	reg(readerH(spec("-l", "-s", "-c", "-g", "-o")), "xxd")
	reg(readerH(spec("-n", "-s", "-e")), "hexdump")
	reg(readerH(spec("-m", "-F")), "file")
	reg(readerH(spec("-a")), "shasum")
	reg(readerH(spec("-n", "-t", "-e")), "strings")
	reg(readerH(spec("-s", "-c", "-o", "-N", "-R", "-H", "-W", "-E", "-O")), "column")
	reg(readerH(spec("-k", "-t", "-o", "-S", "-T", "--parallel")), "sort")
	reg(readerH(spec("-f", "-s", "-w")), "uniq")
	reg(readerH(spec("-d", "-f", "-c", "-b")), "cut")
	reg(readerH(spec("-n", "-i")), "cmp")
	reg(readerH(spec("-d")), "paste")
	reg(readerH(spec("-f", "-t")), "iconv")
	reg(diffH, "diff", "colordiff")
	reg(filterH(spec("-F", "-v", "-f"), "-f"), "awk", "gawk", "mawk")
	reg(func(w *walker, n string, a []arg) {
		filterH(optSpec{withArg: map[string]bool{"-f": true, "--from-file": true, "--indent": true},
			twoArgs: map[string]bool{"--arg": true, "--argjson": true, "--slurpfile": true, "--rawfile": true}},
			"-f", "--from-file")(w, n, a)
	}, "jq")
	reg(filterH(spec("-o", "--output-format", "-I", "-p", "--input-format"), "--from-file"), "yq")
	reg(grepFamily(grepSpec), "grep", "egrep", "fgrep", "zgrep")
	reg(grepFamily(rgSpec), "rg")
	reg(grepFamily(agSpec), "ag")
	reg(grepFamily(ackSpec), "ack")
	reg(findH, "find")
	reg(fdH, "fd", "fdfind")
	reg(writerH(spec("-d", "-r", "-t", "-A"), false), "touch")
	reg(writerH(spec("-m"), false), "mkdir")
	reg(writerH(spec("--reference"), true), "chmod", "chown", "chgrp")
	reg(writerH(spec("-s", "-r", "--size", "--reference"), false), "truncate")
	reg(teeH, "tee")
	reg(deleteH(spec()), "rm", "rmdir", "unlink")
	reg(deleteH(spec("-n", "-s", "--iterations", "--size")), "shred")
	reg(lnH, "ln")
	reg(copyH(spec("-t", "--target-directory", "-S", "--suffix")), "cp")
	reg(copyH(spec("-m", "-o", "-g", "-t", "-S", "--mode", "--owner", "--group", "--target-directory")), "install")
	reg(mvH, "mv")
	reg(ddH, "dd")
	reg(tarH, "tar", "bsdtar", "gtar")
	reg(unzipH, "unzip")
	reg(sedH, "sed", "gsed")
	reg(perlH, "perl")
	reg(formatterH(spec("-r"), []string{"-w"}, nil), "gofmt", "goimports", "gofumpt")
	reg(formatterH(spec("--config", "--ignore-path", "--parser", "--plugin", "--log-level",
		"--stdin-filepath", "--cache-location", "--tab-width", "--print-width"),
		[]string{"--write", "-w"}, nil), "prettier")
	reg(checkFormatterH, "black", "isort", "autopep8", "yapf")
	reg(formatterH(spec("--edition", "--config-path", "--emit", "--config"), nil, []string{"--check"}), "rustfmt")
	reg(formatterH(spec("--style", "-style", "--assume-filename", "--lines", "--offset", "--length"),
		[]string{"-i"}, nil), "clang-format")
	reg(formatterH(spec("-i", "--indent", "-ln"), []string{"-w"}, nil), "shfmt")
	reg(ruffH, "ruff")
	reg(eslintH, "eslint")
	// Wrappers. The wrapped command is classified as if it ran alone.
	reg(wrapper(spec("-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-U", "--user", "--group")), "sudo", "doas")
	reg(envH, "env")
	reg(timeoutH, "timeout", "gtimeout")
	reg(wrapper(spec("-n", "--adjustment")), "nice")
	reg(wrapper(spec()), "nohup", "time", "builtin", "caffeinate")
	reg(wrapper(spec("-i", "-o", "-e")), "stdbuf")
	reg(wrapper(spec("-a")), "exec")
	reg(wrapper(spec("-n", "--interval")), "watch")
	reg(wrapper(spec("-c", "-n", "-p")), "ionice")
	reg(commandH, "command")
	reg(xargsH, "xargs")
	reg(npxH, "npx", "bunx", "pnpx", "uvx")
	reg(runnerH("run", spec("--with", "--python", "-p", "--project", "--directory", "--package",
		"--extra", "--group", "--env-file", "--from"), map[string][]string{
		"sync": {NETWORK, WRITE}, "add": {NETWORK, WRITE}, "remove": {NETWORK, WRITE},
		"lock": {NETWORK, WRITE}, "venv": {WRITE}}), "uv")
	reg(runnerH("run", spec(), map[string][]string{
		"install": {NETWORK, WRITE}, "add": {NETWORK, WRITE}, "lock": {NETWORK, WRITE},
		"update": {NETWORK, WRITE}}), "poetry")
	reg(runnerH("exec", spec(), map[string][]string{
		"install": {NETWORK, WRITE}, "update": {NETWORK, WRITE}}), "bundle")
	reg(shellH, "sh", "bash", "zsh", "dash", "ksh", "fish")
	reg(evalH, "eval", "source", ".")
	reg(nodeH, "node", "nodejs")
	reg(interpH, "ruby", "php", "lua", "Rscript", "tsx", "ts-node", "java", "osascript", "pwsh")
	reg(denoH, "deno")
	reg(goH, "go")
	reg(cargoH, "cargo")
	reg(npmH, "npm", "yarn", "pnpm")
	reg(bunH, "bun")
	reg(dotnetH, "dotnet")
	reg(subcmdH(map[string][]string{"test": {TEST}, "package": {BUILD}, "compile": {BUILD},
		"install": {BUILD}, "verify": {BUILD}, "build": {BUILD}, "check": {TEST}}, "build_tool_goal"),
		"mvn", "gradle")
	reg(testH, "pytest", "py.test", "jest", "vitest", "mocha", "rspec", "ava", "phpunit", "ctest", "tox", "nox")
	reg(buildH, "tsc", "mypy", "pyright", "golangci-lint", "staticcheck", "flake8", "pylint",
		"shellcheck", "hadolint", "tflint", "javac", "rustc")
	reg(ccH, "gcc", "cc", "clang", "g++", "clang++")
	reg(opaqueH, "make", "gmake", "just", "task", "cmake", "ninja", "bazel", "docker",
		"docker-compose", "podman", "ansible-playbook", "nix", "vagrant")
	reg(func(w *walker, n string, a []arg) { opaqueH(w, n, a); w.a.add(NETWORK) }, "terraform", "tofu")
	reg(pipH, "pip", "pip3")
	reg(curlH, "curl")
	reg(wgetH, "wget")
	reg(netH, "http", "https", "xh", "ping", "nc", "telnet", "dig", "nslookup", "host", "traceroute")
	reg(netUnknownH, "ssh", "helm", "gh")
	reg(copyRemoteH, "scp", "rsync")
	reg(kubectlH, "kubectl", "oc")
	reg(dbH, "psql", "mysql", "mongosh", "redis-cli")
	reg(func(w *walker, _ string, _ []arg) { w.a.unknown("database_effects") }, "sqlite3", "duckdb")
	reg(pkgMgrH, "apt", "apt-get", "brew", "yum", "dnf", "apk", "port", "snap", "pacman")
	reg(protocH, "protoc", "buf")
	reg(gitH, "git")
	reg(patchH, "patch")
	reg(applyPatchH, "apply_patch", "applypatch")
}
