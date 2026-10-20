package build

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// TestRef is one test function that a check must actually run. Its ID ("file::Name")
// links requirements to per-test results.
type TestRef struct {
	File string `json:"file"`
	Name string `json:"name"`
}

// ID is the test identity printed by the verifier.
func (t TestRef) ID() string { return t.File + "::" + t.Name }

// verifierPlan is everything the generated verifier script needs.
type verifierPlan struct {
	Lang    string
	Module  string            // Go module path of the repository root
	Hidden  []string          // hidden test files, copied in before C1
	C1      []TestRef         // hidden tests: each must pass
	C2      []TestRef         // existing tests: each must pass or be skipped
	APIDirs []string          // refactoring: C1 is the API check of these packages
	Pkgs    map[string]string // Go: package clause of each required test file
}

var testIDSafe = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

var goModule = regexp.MustCompile(`(?m)^module\s+("?)([^\s"]+)("?)\s*$`)

// goModulePath returns the module path of the root go.mod, or "".
func goModulePath(l *library.Library, base trees.Tree) string {
	for _, f := range base.Files {
		if f.Path == "go.mod" {
			b, err := l.GetObject(f.Digest)
			if err != nil {
				return ""
			}
			if m := goModule.FindSubmatch(b); m != nil {
				return string(m[2])
			}
		}
	}
	return ""
}

var goBuildConstraint = regexp.MustCompile(`(?m)^//(go:build | \+build )`)

var goOSArch = map[string]bool{}

func init() {
	for _, s := range strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos " +
		"386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm") {
		goOSArch[s] = true
	}
}

// goRunsByDefault reports whether "go test ./..." from the repository root runs the test
// file on linux: not under testdata, vendor, "_" or "." directories, not in a nested
// module, and without build constraints (in the file or its name).
func goRunsByDefault(p string, data []byte, nested map[string]bool) bool {
	parts := strings.Split(p, "/")
	for _, d := range parts[:len(parts)-1] {
		if d == "testdata" || d == "vendor" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".") {
			return false
		}
	}
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if nested[d] {
			return false
		}
	}
	name := strings.TrimSuffix(parts[len(parts)-1], "_test.go")
	if f := strings.Split(name, "_"); len(f) > 1 && goOSArch[f[len(f)-1]] {
		return false
	}
	head := data
	if i := bytes.Index(data, []byte("\npackage ")); i >= 0 {
		head = data[:i]
	}
	return !goBuildConstraint.Match(head)
}

var pyClass = regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`)
var pyTop = regexp.MustCompile(`^(async\s+)?def\s+(test[A-Za-z0-9_]*)\s*\(`)
var pyMethod = regexp.MustCompile(`^\s+(async\s+)?def\s+(test[A-Za-z0-9_]*)\s*\(`)

// pyTestNames returns the test functions that pytest collects by default: module-level
// test* functions and test* methods of Test* classes.
func pyTestNames(src []byte) []string {
	var out []string
	cls := ""
	for _, ln := range strings.Split(string(src), "\n") {
		switch {
		case pyClass.MatchString(ln):
			cls = pyClass.FindStringSubmatch(ln)[1]
		case pyTop.MatchString(ln):
			cls = ""
			out = append(out, pyTop.FindStringSubmatch(ln)[2])
		case pyMethod.MatchString(ln):
			if strings.HasPrefix(cls, "Test") {
				out = append(out, pyMethod.FindStringSubmatch(ln)[2])
			}
		case ln != "" && ln[0] != ' ' && ln[0] != '\t' && ln[0] != '#' && ln[0] != '@' && ln[0] != ')':
			cls = ""
		}
	}
	return out
}

// testRefs lists the test functions of files. With strict set, a file whose path cannot be
// printed safely is an error; otherwise it is left out.
func testRefs(l *library.Library, lang string, files []trees.File, keep func(trees.File, []byte) bool) ([]TestRef, error) {
	var out []TestRef
	seen := map[string]bool{}
	for _, f := range files {
		if !testIDSafe.MatchString(f.Path) {
			continue
		}
		b, err := l.GetObject(f.Digest)
		if err != nil {
			return nil, err
		}
		if keep != nil && !keep(f, b) {
			continue
		}
		var names []string
		switch lang {
		case "go":
			for _, m := range goTestFunc.FindAllSubmatch(b, -1) {
				names = append(names, string(m[1]))
			}
		case "python":
			names = pyTestNames(b)
		}
		for _, n := range names {
			r := TestRef{File: f.Path, Name: n}
			if !seen[r.ID()] {
				seen[r.ID()] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// gate-v1 (07B-J): the result of a check never comes from the test process's output. An
// evaluator-owned gate inside the test process (Go: a generated test that runs each
// required test function; Python: a pytest plugin) reads a per-check nonce from inherited
// fd 3 and writes one report through inherited fd 4; root opens both descriptors on
// root-only files and accepts the check only with exit status 0 and exactly the expected
// report lines. The test process's stdout and stderr are logs only. This removes the
// printed-framing forgeries (test2json markers, pytest result lines); code that targets
// the gate in its own process can still forge (same-process trust, Q7 not met).

// gate is one gate of a check: Go, one per package directory and package clause; Python,
// one per check.
type gate struct {
	ID    string    // report identity: check, directory, package kind
	Dir   string    // Go: package directory
	Pkg   string    // Go: package clause
	Func  string    // Go: the gate test function
	Tests []TestRef // required tests, in order
}

// gates groups the required tests of a check.
func (v verifierPlan) gates(check string, refs []TestRef) []gate {
	if v.Lang == "python" {
		if len(refs) == 0 {
			return nil
		}
		return []gate{{ID: check, Tests: refs}}
	}
	by := map[string]*gate{}
	var order []string
	for _, t := range refs {
		pkg := v.Pkgs[t.File]
		dir := path.Dir(t.File)
		k := dir + "\x00" + pkg
		if by[k] == nil {
			g := &gate{ID: check + "@" + dir, Dir: dir, Pkg: pkg, Func: "TestHintGate" + check}
			if strings.HasSuffix(pkg, "_test") {
				g.ID, g.Func = g.ID+"+ext", g.Func+"Ext"
			}
			by[k] = g
			order = append(order, k)
		}
		by[k].Tests = append(by[k].Tests, t)
	}
	sort.Strings(order)
	out := make([]gate, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out
}

// gateFile is the Go gate test of one gate. It runs every required test function as a
// subtest (parallel subtests finish inside the group) and reports each one's final state.
func (g gate) gateFile() string {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by the HINT verifier (gate-v1). DO NOT EDIT.

package %s

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func %s(t *testing.T) {
	in := os.NewFile(3, "nonce")
	buf := make([]byte, 64)
	n, _ := in.Read(buf)
	in.Close()
	nonce := strings.TrimSpace(string(buf[:n]))
	if nonce == "" {
		t.Fatal("no nonce")
	}
	tests := []struct {
		name string
		fn   func(*testing.T)
	}{
`, g.Pkg, g.Func)
	for _, t := range g.Tests {
		fmt.Fprintf(&b, "\t\t{%q, %s},\n", t.Name, t.Name)
	}
	fmt.Fprintf(&b, `	}
	var mu sync.Mutex
	res := map[string]string{}
	t.Run("g", func(t *testing.T) {
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Cleanup(func() {
					st := "pass"
					if t.Failed() {
						st = "fail"
					} else if t.Skipped() {
						st = "skip"
					}
					mu.Lock()
					res[tc.name] = st
					mu.Unlock()
				})
				tc.fn(t)
			})
		}
	})
	var out strings.Builder
	for _, tc := range tests {
		st := res[tc.name]
		if st == "" {
			st = "notrun"
		}
		out.WriteString("HINTGATE " + nonce + " %s " + tc.name + " " + st + "\n")
	}
	out.WriteString("HINTGATE " + nonce + " %s END\n")
	f := os.NewFile(4, "report")
	f.WriteString(out.String())
	f.Close()
}
`, g.ID, g.ID)
	return b.String()
}

// pyGate is the pytest plugin of gate-v1 (loaded with -p hint_gate from /tests/gate). A
// required test passes when every collected item of it passed; skip when none failed and
// one was skipped (xfail counts as skip); notrun when nothing of it ran.
const pyGate = `"""HINT verifier gate (gate-v1). Generated; do not edit."""
import os

_nonce = ""
_refs = []
_paths = {}
_state = {}


def pytest_configure(config):
    global _nonce, _refs
    try:
        _nonce = os.read(3, 64).decode("ascii", "replace").strip()
        os.close(3)
    except OSError:
        _nonce = ""
    with open(os.environ["HINT_GATE_REFS"]) as f:
        _refs = [ln.strip() for ln in f if ln.strip()]


def pytest_collection_modifyitems(session, config, items):
    for it in items:
        _paths[it.nodeid] = os.path.relpath(str(it.path), os.environ["HINT_GATE_ROOT"])


def _ref(nodeid):
    name = nodeid.split("::")[-1].split("[")[0]
    return _paths.get(nodeid, nodeid.split("::")[0]) + "::" + name


def pytest_runtest_logreport(report):
    key = _ref(report.nodeid)
    items = _state.setdefault(key, {})
    st = items.get(report.nodeid, "pass")
    if report.failed:
        st = "fail"
    elif report.skipped and st != "fail":
        st = "skip"
    items[report.nodeid] = st


def pytest_sessionfinish(session, exitstatus):
    check = os.environ["HINT_GATE_CHECK"]
    out = []
    for r in _refs:
        items = _state.get(r)
        if not items:
            st = "notrun"
        elif "fail" in items.values():
            st = "fail"
        elif "skip" in items.values():
            st = "skip"
        else:
            st = "pass"
        out.append("HINTGATE %s %s %s %s\n" % (_nonce, check, r, st))
    out.append("HINTGATE %s %s END\n" % (_nonce, check))
    try:
        os.write(4, "".join(out).encode())
        os.close(4)
    except OSError:
        pass
`

// gateExpect lists the expected report entries of a check: gate, name, test ID.
func (v verifierPlan) gateExpect(check string, refs []TestRef) string {
	var b strings.Builder
	for _, g := range v.gates(check, refs) {
		for _, t := range g.Tests {
			name := t.Name
			if v.Lang == "python" {
				name = t.ID()
			}
			fmt.Fprintf(&b, "%s %s %s\n", g.ID, name, t.ID())
		}
	}
	return b.String()
}

// gateArchive returns the gate files of the verifier archive: the expected report entries
// per check, the Go gate tests (gate/<check>/<dir>/...) or the Python plugin and refs.
func (v verifierPlan) gateArchive() []extra {
	var out []extra
	for _, c := range []struct {
		check string
		refs  []TestRef
	}{{"C1", v.C1}, {"C2", v.C2}} {
		out = append(out, extra{"gate/" + c.check + ".expect", []byte(v.gateExpect(c.check, c.refs)), 0o644})
		switch v.Lang {
		case "go":
			for _, g := range v.gates(c.check, c.refs) {
				name := "zz_hint_gate_" + strings.ToLower(c.check) + "_test.go"
				if strings.HasSuffix(g.Pkg, "_test") {
					name = "zz_hint_gate_" + strings.ToLower(c.check) + "_ext_test.go"
				}
				out = append(out, extra{path.Join("gate", c.check, g.Dir, name), []byte(g.gateFile()), 0o644})
			}
		case "python":
			var ids []string
			for _, t := range c.refs {
				ids = append(ids, t.ID())
			}
			out = append(out, extra{"gate/" + c.check + ".refs", []byte(strings.Join(ids, "\n") + "\n"), 0o644})
		}
	}
	if v.Lang == "python" {
		out = append(out, extra{"gate/hint_gate.py", []byte(pyGate), 0o644})
	}
	return out
}

// runCheck returns the lines of one gated check: a fresh nonce and an empty report on
// root-only files; the gate files copied in; the tests run as nobody with the nonce on fd
// 3 and the report on fd 4 (output to the logs only); every nobody process stopped; then
// the check passes only with exit status 0 and exactly the expected report: one END line
// per gate, one line per required test with an accepted state, nothing else.
//
// Go C2 keeps the whole existing suite as a veto first (C7BJ-3): "go test ./..." of the
// repository, before any gate file exists, must exit 0 (also with no required test), so a
// build failure, an unlisted failing test or a failing example still fails C2; its output
// is never evidence of a pass.
func (v verifierPlan) runCheck(check string, refs []TestRef, accepted []string) string {
	hc := "/var/lib/hint-check/" + check
	out := hc + ".out"
	veto := v.Lang == "go" && check == "C2"
	if len(refs) == 0 && !veto {
		return "echo \"CHECK " + check + " PASS\"\n" // no required test
	}
	var b strings.Builder
	fmt.Fprintf(&b, "r=PASS\n: > %s\n", out)
	if veto {
		fmt.Fprintf(&b, "run_as %s >>%s 2>&1 || r=FAIL\nkillnobody\n", shq("cd "+Workdir+" && go test -count=1 ./..."), out)
	}
	if len(refs) == 0 {
		fmt.Fprintf(&b, "cp %s /logs/verifier/%s.txt\n[ \"$r\" = PASS ] || ok=0\necho \"CHECK %s $r\"\n", out, strings.ToLower(check), check)
		return b.String()
	}
	fmt.Fprintf(&b, ": > %s.report\nchmod 600 %s.report\nhead -c 16 /dev/urandom | od -An -tx1 | tr -d ' \\n' > %s.nonce\nchmod 600 %s.nonce\n", hc, hc, hc, hc)
	switch v.Lang {
	case "go":
		fmt.Fprintf(&b, "cp -R /tests/gate/%s/. %s/ && chown -R 65534:65534 %s || r=FAIL\n", check, Workdir, Workdir)
		dirs := map[string][]string{}
		for _, g := range v.gates(check, refs) {
			dirs[g.Dir] = append(dirs[g.Dir], g.Func)
		}
		for i, d := range keys(boolSet(dirs)) {
			bin := fmt.Sprintf("/tmp/hint-bin/%s-%d.test", check, i)
			fmt.Fprintf(&b, "run_as %s >>%s 2>&1 || r=FAIL\n", shq("cd "+Workdir+" && go test -c -o "+bin+" ./"+d), out)
			for _, fn := range dirs[d] { // one run per gate: each reads the nonce once
				fmt.Fprintf(&b, "if [ -x %s ]; then run_as %s 3<%s.nonce 4>>%s.report >>%s 2>&1 || r=FAIL; else r=FAIL; fi\n", bin,
					shq("cd "+path.Join(Workdir, d)+" && "+bin+" -test.run '^"+fn+"$' -test.count=1 -test.v"), hc, hc, out)
			}
		}
	case "python":
		files := map[string]bool{}
		for _, t := range refs {
			files[t.File] = true
		}
		cmd := "env PYTHONPATH=/tests/gate HINT_GATE_CHECK=" + check + " HINT_GATE_REFS=/tests/gate/" + check + ".refs HINT_GATE_ROOT=" + Workdir +
			" python -m pytest -v -p no:cacheprovider -p hint_gate " + strings.Join(quoteAll(keys(files)), " ")
		fmt.Fprintf(&b, "run_as %s 3<%s.nonce 4>>%s.report >>%s 2>&1 || r=FAIL\n", shq(cmd), hc, hc, out)
	}
	fmt.Fprintf(&b, "killnobody\ncp %s /logs/verifier/%s.txt\ngate_check %s %s\n[ \"$r\" = PASS ] || ok=0\necho \"CHECK %s $r\"\n",
		out, strings.ToLower(check), check, shq(strings.Join(accepted, " ")), check)
	return b.String()
}

func boolSet[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// gateCheck is the root-side report check (shell): gate_check CHECK "ACCEPTED STATES".
// It prints one TEST line per required test and sets r=FAIL on any deviation.
const gateCheck = `gate_check() {
	c=$1; acc=" $2 "; rep=/var/lib/hint-check/$c.report; exp=/tests/gate/$c.expect; n=$(cat /var/lib/hint-check/$c.nonce)
	gs=$(cut -d' ' -f1 "$exp" | sort -u)
	want=$(( $(wc -l < "$exp") + $(printf '%s
' $gs | grep -c .) ))
	[ "$(wc -l < "$rep")" -eq "$want" ] || r=FAIL
	for g in $gs; do [ "$(grep -cFx "HINTGATE $n $g END" "$rep")" = 1 ] || r=FAIL; done
	while read -r g name id; do
		k=$(grep -cF "HINTGATE $n $g $name " "$rep")
		st=$(grep -F "HINTGATE $n $g $name " "$rep" | head -1)
		st=${st##* }
		case "$acc" in *" $st "*) t=PASS ;; *) t=FAIL ;; esac
		[ "$k" = 1 ] || t=FAIL
		[ "$t" = PASS ] || r=FAIL
		echo "TEST $c $id $t"
	done < "$exp"
}
`

// testPatterns are the file names that count as tests: files of these names that the
// evaluator does not own are removed before the checks.
var testPatterns = map[string]string{
	"go":     `-name '*_test.go'`,
	"python": `\( -name 'test_*.py' -o -name '*_test.py' -o -name conftest.py -o -name pytest.ini \)`,
}

// script is the generated verifier. It runs in the separate verifier container (no
// network) as root; the checks run as nobody:
//   - restore evaluator-owned copies of the existing tests and remove every other test
//     file (tests/allowed.txt lists the evaluator's), so the agent cannot add a TestMain,
//     a conftest.py or a pytest.ini that skips or fakes the checks;
//   - C2: the existing suite; every existing test that runs by default must pass or skip;
//   - copy the hidden tests in; C1: the hidden tests only, each must pass (or, for a
//     refactoring, the exported API must equal the requested structure).
//
// A check passes only with exit status 0 and the gate-v1 report of each required test
// (runCheck). Printed test events count for nothing; code under test that targets the
// gate inside its own process is not stopped (same-process trust). The root-only output
// directory and the process kill stop later rewrites.
func (v verifierPlan) script() string {
	var b strings.Builder
	b.WriteString(`#!/bin/sh
# Generated verifier: fixed checks, no session command. Writes /logs/verifier/reward.txt.
set -u
mkdir -p /logs/verifier
reward=0
# kill every process of the check user (kernel-enforced, also where the mounted log
# directory does not enforce file permissions); repeat until none is left
killnobody() {
	for i in 1 2 3 4 5; do
		found=0
		for d in /proc/[0-9]*; do
			u=$(awk '/^Uid:/{print $2; exit}' "$d/status" 2>/dev/null)
			if [ "$u" = 65534 ]; then kill -9 "${d#/proc/}" 2>/dev/null; found=1; fi
		done
		[ "$found" = 0 ] && return 0
		sleep 0.2
	done
}
# the verifier must have no network: record whether a TCP connection leaves this
# container (DNS answers alone do not count; written at the end, after the code under
# test stopped)
net=blocked
for target in 1.1.1.1 example.com; do
	if command -v curl >/dev/null 2>&1; then
		curl -s -o /dev/null --connect-timeout 5 "https://$target" && net=reachable
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c "import socket,sys; socket.create_connection((sys.argv[1], 443), 5)" "$target" 2>/dev/null && net=reachable
	fi
done
# at the end stop every other process and write both reward files (Harbor reads
# reward.json before reward.txt)
finish() { killnobody; echo "$reward" > /logs/verifier/reward.txt; echo "{\"reward\": $reward}" > /logs/verifier/reward.json; echo "HINT_VERIFIER_NET=$net" > /logs/verifier/network.txt; }
trap finish EXIT
# the checks run as the unprivileged user nobody; this script (root) writes the results
run_as() { setpriv --reuid=65534 --regid=65534 --clear-groups env HOME=/tmp/hint-home GOCACHE=/tmp/hint-gocache GOFLAGS=-mod=mod PYTHONDONTWRITEBYTECODE=1 sh -c "$1"; }
mkdir -p /tmp/hint-home /tmp/hint-gocache && chown -R 65534:65534 /tmp/hint-home /tmp/hint-gocache
mkdir -p /var/lib/hint-check && chmod 700 /var/lib/hint-check
mkdir -p /tmp/hint-bin && chown 65534:65534 /tmp/hint-bin
` + gateCheck + `# remove test files that the evaluator does not own
prune() { find . -path ./.git -prune -o -type f ` + testPatterns[v.Lang] + ` -print | while IFS= read -r f; do grep -Fxq "${f#./}" /tests/allowed.txt || rm -f "$f"; done; }
cd ` + Workdir + ` || exit 0
[ -d /tests/preserve ] && cp -R /tests/preserve/. ` + Workdir + `/
prune
chown -R 65534:65534 ` + Workdir + `
ok=1
`)
	switch v.Lang {
	case "go":
		b.WriteString(v.runCheck("C2", v.C2, []string{"pass", "skip"}))
	case "python":
		b.WriteString(v.runCheck("C2", v.C2, []string{"pass", "skip"}))
	}
	if v.APIDirs != nil {
		b.WriteString(`mkdir -p /tmp/apicheck && cp /tests/apicheck/main.go /tmp/apicheck/main.go
printf 'module apicheck\n\ngo 1.21\n' > /tmp/apicheck/go.mod && chown -R 65534:65534 /tmp/apicheck
if run_as "cd /tmp/apicheck && go run . ` + Workdir + ` ` + strings.Join(quoteAll(v.APIDirs), " ") + `" >/var/lib/hint-check/api.txt 2>/logs/verifier/c1.txt && diff -u /tests/api.expected /var/lib/hint-check/api.txt >>/logs/verifier/c1.txt 2>&1; then r=PASS; else r=FAIL; ok=0; fi; killnobody; echo "CHECK C1 $r"
`)
	} else {
		b.WriteString("cp -R /tests/hidden/. " + Workdir + "/ || exit 0\nprune\nchown -R 65534:65534 " + Workdir + "\n")
		b.WriteString(v.runCheck("C1", v.C1, []string{"pass"}))
	}
	b.WriteString("[ \"$ok\" = 1 ] && reward=1\nexit 0\n")
	return b.String()
}
