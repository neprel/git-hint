package build

import (
	_ "embed"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals/build/apicheck"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

//go:embed apicheck/apicheck.go
var apicheckSource string

// apicheckMain is the verifier program: the same source as package apicheck, run as a
// command with the repository root and the package directories as arguments.
func apicheckMain() string {
	src := strings.Replace(apicheckSource, "package apicheck", "package main", 1)
	src = strings.Replace(src, "import (\n", "import (\n\t\"os\"\n", 1)
	return src + `
func main() {
	lines, err := Lines(os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}
`
}

// refactorAPI computes the exported API of the packages that hold the changed Go files,
// in the base and in the result. Unexported structure is not checked: its names are the
// reference's free choice, and the plan forbids checks of incidental details.
func refactorAPI(l *library.Library, base, result string, changed []string) (dirs, before, after []string, err error) {
	pk := map[string]bool{}
	for _, p := range changed {
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			pk[path.Dir(p)] = true
		}
	}
	for d := range pk {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return nil, nil, nil, &Refusal{[]string{"verifier_missing"}}
	}
	api := func(tree string) ([]string, error) {
		tmp, err := os.MkdirTemp("", "hint-api-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if err := trees.Materialize(l, tree, tmp); err != nil {
			return nil, err
		}
		return apicheck.Lines(tmp, dirs)
	}
	if before, err = api(base); err != nil {
		return nil, nil, nil, &Refusal{[]string{"verifier_missing"}}
	}
	if after, err = api(result); err != nil {
		return nil, nil, nil, &Refusal{[]string{"verifier_missing"}}
	}
	if strings.Join(before, "\n") == strings.Join(after, "\n") {
		return nil, nil, nil, &Refusal{[]string{"verifier_missing"}} // no objective structural change
	}
	return dirs, before, after, nil
}

// refactorScript: restore evaluator-owned tests, C2 the existing suite (behavior kept),
// C1 the exported API of the changed packages equals the requested structure.
func refactorScript(dirs []string) string {
	return `#!/bin/sh
# Generated verifier (refactoring): fixed checks, no session command.
set -u
mkdir -p /logs/verifier
reward=0
trap 'echo "$reward" > /logs/verifier/reward.txt' EXIT
cd ` + Workdir + ` || exit 0
[ -d /tests/preserve ] && cp -R /tests/preserve/. ` + Workdir + `/
ok=1
if go test -count=1 ./... >/logs/verifier/c2.txt 2>&1; then echo "CHECK C2 PASS"; else echo "CHECK C2 FAIL"; ok=0; fi
mkdir -p /tmp/apicheck && cp /tests/apicheck/main.go /tmp/apicheck/main.go
printf 'module apicheck\n\ngo 1.21\n' > /tmp/apicheck/go.mod
if (cd /tmp/apicheck && GOFLAGS=-mod=mod go run . ` + Workdir + ` ` + strings.Join(quoteAll(dirs), " ") + `) >/logs/verifier/api.txt 2>/logs/verifier/c1.txt && diff -u /tests/api.expected /logs/verifier/api.txt >>/logs/verifier/c1.txt 2>&1; then echo "CHECK C1 PASS"; else echo "CHECK C1 FAIL"; ok=0; fi
[ "$ok" = 1 ] && reward=1
exit 0
`
}
