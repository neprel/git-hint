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
