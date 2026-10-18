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
	Module  string    // Go module path of the repository root
	Hidden  []string  // hidden test files, copied in before C1
	C1      []TestRef // hidden tests: each must pass
	C2      []TestRef // existing tests: each must pass or be skipped
	APIDirs []string  // refactoring: C1 is the API check of these packages
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

// ereQuote quotes a string for grep -E.
func ereQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`.[]()*+?{}|^$\`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// goImport is the import path of a package directory.
func goImport(module, dir string) string {
	if dir == "." {
		return module
	}
	return module + "/" + dir
}

// evidence returns the shell lines that print one TEST line per required test and set r
// to FAIL when one did not run with an accepted result. Go: fixed-string matches on
// "go test -json" events (a JSON string never holds an unescaped quote, so test output
// cannot form such an event). Python: pytest -v result lines.
func (v verifierPlan) evidence(check, out string, refs []TestRef, accepted []string) string {
	var b strings.Builder
	for _, t := range refs {
		var test string
		switch v.Lang {
		case "go":
			var args []string
			for _, a := range accepted {
				args = append(args, "-e "+shq(fmt.Sprintf(`"Action":%q,"Package":%q,"Test":%q,`, a, goImport(v.Module, path.Dir(t.File)), t.Name)))
			}
			test = "grep -Fq " + strings.Join(args, " ") + " " + out
		case "python":
			// the node ID is the path relative to pytest's root directory: any suffix of
			// the file path
			parts := strings.Split(t.File, "/")
			var alts []string
			for i := range parts {
				alts = append(alts, ereQuote(strings.Join(parts[i:], "/")))
			}
			pat := "^(" + strings.Join(alts, "|") + ")::([A-Za-z_][A-Za-z0-9_]*::)*" + ereQuote(t.Name) + `(\[.*\])? (` + strings.Join(accepted, "|") + `)( |$)`
			test = "grep -Eq " + shq(pat) + " " + out
		}
		fmt.Fprintf(&b, "if %s; then t=PASS; else t=FAIL; r=FAIL; fi; echo \"TEST %s %s $t\"\n", test, check, t.ID())
	}
	return b.String()
}

// runCheck returns the lines of one check: run cmd as nobody with its output in a
// root-only directory, stop every nobody process, copy the output to the logs, require
// exit status 0 and the per-test evidence, print the check line.
func (v verifierPlan) runCheck(check, cmd string, refs []TestRef, accepted []string) string {
	out := "/var/lib/hint-check/" + check + ".out"
	return "r=PASS\nrun_as " + shq(cmd) + " >" + out + " 2>&1 || r=FAIL\nkillnobody\ncp " + out + " /logs/verifier/" + strings.ToLower(check) + ".txt\n" +
		v.evidence(check, out, refs, accepted) +
		"[ \"$r\" = PASS ] || ok=0\necho \"CHECK " + check + " $r\"\n"
}

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
// A check passes only with exit status 0 and evidence that each required test ran.
// Code under test runs in the test process and could still print fake test events; the
// root-only output directory and the process kill stop later rewrites, not that.
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
# at the end stop every other process and write both reward files (Harbor reads
# reward.json before reward.txt)
finish() { killnobody; echo "$reward" > /logs/verifier/reward.txt; echo "{\"reward\": $reward}" > /logs/verifier/reward.json; }
trap finish EXIT
# the checks run as the unprivileged user nobody; this script (root) writes the results
run_as() { setpriv --reuid=65534 --regid=65534 --clear-groups env HOME=/tmp/hint-home GOCACHE=/tmp/hint-gocache GOFLAGS=-mod=mod PYTHONDONTWRITEBYTECODE=1 sh -c "$1"; }
mkdir -p /tmp/hint-home /tmp/hint-gocache && chown -R 65534:65534 /tmp/hint-home /tmp/hint-gocache
mkdir -p /var/lib/hint-check && chmod 700 /var/lib/hint-check
# remove test files that the evaluator does not own
prune() { find . -path ./.git -prune -o -type f ` + testPatterns[v.Lang] + ` -print | while IFS= read -r f; do grep -Fxq "${f#./}" /tests/allowed.txt || rm -f "$f"; done; }
cd ` + Workdir + ` || exit 0
[ -d /tests/preserve ] && cp -R /tests/preserve/. ` + Workdir + `/
prune
chown -R 65534:65534 ` + Workdir + `
ok=1
`)
	switch v.Lang {
	case "go":
		b.WriteString(v.runCheck("C2", "go test -count=1 -json ./...", v.C2, []string{"pass", "skip"}))
	case "python":
		if len(v.C2) == 0 {
			b.WriteString("echo \"CHECK C2 PASS\"\n") // no existing test to keep
		} else {
			files := map[string]bool{}
			for _, t := range v.C2 {
				files[t.File] = true
			}
			b.WriteString(v.runCheck("C2", "python -m pytest -v -p no:cacheprovider "+strings.Join(quoteAll(keys(files)), " "), v.C2, []string{"PASSED", "SKIPPED", "XFAIL"}))
		}
	}
	if v.APIDirs != nil {
		b.WriteString(`mkdir -p /tmp/apicheck && cp /tests/apicheck/main.go /tmp/apicheck/main.go
printf 'module apicheck\n\ngo 1.21\n' > /tmp/apicheck/go.mod && chown -R 65534:65534 /tmp/apicheck
if run_as "cd /tmp/apicheck && go run . ` + Workdir + ` ` + strings.Join(quoteAll(v.APIDirs), " ") + `" >/var/lib/hint-check/api.txt 2>/logs/verifier/c1.txt && diff -u /tests/api.expected /var/lib/hint-check/api.txt >>/logs/verifier/c1.txt 2>&1; then r=PASS; else r=FAIL; ok=0; fi; killnobody; echo "CHECK C1 $r"
`)
	} else {
		b.WriteString("cp -R /tests/hidden/. " + Workdir + "/ || exit 0\nprune\nchown -R 65534:65534 " + Workdir + "\n")
		switch v.Lang {
		case "go":
			pk, names := map[string]bool{}, map[string]bool{}
			for _, t := range v.C1 {
				pk["./"+path.Dir(t.File)] = true
				names[t.Name] = true
			}
			b.WriteString(v.runCheck("C1", "go test -count=1 -json -run '^("+strings.Join(keys(names), "|")+")$' "+strings.Join(keys(pk), " "), v.C1, []string{"pass"}))
		case "python":
			b.WriteString(v.runCheck("C1", "python -m pytest -v -p no:cacheprovider "+strings.Join(quoteAll(v.Hidden), " "), v.C1, []string{"PASSED"}))
		}
	}
	b.WriteString("[ \"$ok\" = 1 ] && reward=1\nexit 0\n")
	return b.String()
}
