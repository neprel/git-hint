package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// caseFlags collects repeated --case ID=FILE flags.
type caseFlags []string

func (c *caseFlags) String() string     { return strings.Join(*c, ",") }
func (c *caseFlags) Set(v string) error { *c = append(*c, v); return nil }

// defaultCaseSeconds bounds one case run when the author sets no limit; every run also
// stays within the shared verifier deadline of the trial.
const defaultCaseSeconds = 10

// evalDeclareIO packages a declared cli-file-v1 eval (07B-J §19): the base and reference
// states from Git revisions (read only; no checkout, no change of the work tree), a command
// given as a JSON argv array (no shell), and the author's own cases read from local files.
// Nothing is extracted from sessions or tests and nothing leaves this machine.
func evalDeclareIO(e *Env, args []string) int {
	fs := flags(e, "eval declare-io", "reads Git objects and the named files; writes the local eval library; no network")
	base := fs.String("base", "", "Git revision of the starting state")
	ref := fs.String("reference", "", "Git revision of the stored reference (a correct solution)")
	entry := fs.String("entry", "", `the command as a JSON argv array, for example '["python","tool.py"]' (run in the repository root, input on stdin)`)
	var cases caseFlags
	fs.Var(&cases, "case", "a case as ID=FILE (repeat; the file is the exact stdin; 1 to 64 cases)")
	secs := fs.Int("case-seconds", defaultCaseSeconds, "time limit of one case run (1 to 30)")
	request := fs.String("request", "", "file with the request text for the agent")
	family := fs.String("family", "bug_fix", "task family: bug_fix, feature, compatibility or refactor")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *base == "" || *ref == "" || *entry == "" || len(cases) == 0 || *request == "" {
		fmt.Fprintln(e.Stderr, "usage: git hint eval declare-io --base REV --reference REV --entry JSON --case ID=FILE... --request FILE")
		return 2
	}
	var argv []string
	dec := json.NewDecoder(strings.NewReader(*entry))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&argv); err != nil || dec.More() {
		return fail(e, errors.New("--entry must be a JSON array of strings, for example '[\"python\",\"tool.py\"]'"))
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	ctx := context.Background()
	l := library.Open(s, 0)
	baseTree, err := revTree(ctx, l, s.Root, *base)
	if err != nil {
		return fail(e, fmt.Errorf("--base: %w", err))
	}
	refTree, err := revTree(ctx, l, s.Root, *ref)
	if err != nil {
		return fail(e, fmt.Errorf("--reference: %w", err))
	}
	bt, err := trees.Load(l, baseTree)
	if err != nil {
		return fail(e, err)
	}
	rt, err := trees.Load(l, refTree)
	if err != nil {
		return fail(e, err)
	}
	inBase, inRef := map[string]bool{}, map[string]bool{}
	for _, f := range bt.Files {
		inBase[f.Path] = true
	}
	for _, f := range rt.Files {
		inRef[f.Path] = true
	}
	var ioCases []build.IOCase
	for _, c := range cases {
		id, file, ok := strings.Cut(c, "=")
		if !ok || id == "" || file == "" {
			return fail(e, fmt.Errorf("--case %q: use ID=FILE", c))
		}
		in, err := readCaseFile(s.Root, file, inBase, inRef)
		if err != nil {
			return fail(e, fmt.Errorf("--case %s: %w", id, err))
		}
		ioCases = append(ioCases, build.IOCase{ID: id, Input: in})
	}
	req, err := readSmall(*request, 64<<10)
	if err != nil || strings.TrimSpace(string(req)) == "" {
		return fail(e, errors.New("--request: a readable, nonempty file of at most 64 KiB is needed"))
	}
	in := build.InputIO{Family: *family, Repo: filepath.Base(s.Root), Base: baseTree, Result: refTree, Request: string(req),
		RequestAt: map[string]any{"source": library.Digest(req), "record": "declare-io"}, Entry: argv, Cases: ioCases, CaseSeconds: *secs, Now: e.Now()}
	in.EpisodeID = "ep-" + strings.TrimPrefix(build.IOEvalID(in), "ev-") // the declaration is its own episode
	// an identical declaration (same canonical identity) is the existing eval: nothing is
	// written again and its later versions (mapped, qualified) stay as they are (C7BJ-10)
	id := build.IOEvalID(in)
	for _, r := range latestEvals(l) {
		if strings.HasPrefix(r.Ref, id+"@") {
			var rec map[string]any
			if err := l.Manifest("eval", r.Ref, &rec); err != nil {
				return fail(e, err)
			}
			fmt.Fprintf(e.Stdout, "This declaration exists already: eval %s (%v); nothing was written.\n", r.Ref, rec["state"])
			if unmappedRequest(rec) {
				fmt.Fprintln(e.Stdout, "The request (R1) is not linked to a check yet: link it with git hint eval map before the eval can be validated.")
			}
			return 0
		}
	}
	b, err := build.BuildIO(l, in)
	if err != nil {
		var r *build.Refusal
		if errors.As(err, &r) {
			return fail(e, fmt.Errorf("the declaration is refused: %s", strings.Join(r.Reasons, ", ")))
		}
		return fail(e, err)
	}
	evRef, err := l.PutManifest("eval", b.Record)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Eval %s packaged (cli-file-v1, %d cases).\n", evRef, len(ioCases))
	fmt.Fprintf(e.Stdout, "Its check B1 compares the program's stdout and exit status with the stored reference's on the %d declared cases:\n", len(ioCases))
	fmt.Fprintln(e.Stdout, "finite reference-equivalence, not universal correctness. The cases stay on this machine and never reach the agent.")
	fmt.Fprintln(e.Stdout, "The request (R1) is not linked to a check yet: link it with git hint eval map before the eval can be validated.")
	fmt.Fprintln(e.Stdout, "Only the container runner qualifies this eval; until it replaces the current runner, the eval stays packaged (runner_unsupported).")
	return 0
}

// unmappedRequest says whether a request or correction requirement of rec has no check.
func unmappedRequest(rec map[string]any) bool {
	reqs, _ := rec["requirements"].([]any)
	for _, r := range reqs {
		if m, _ := r.(map[string]any); m != nil && m["mapping"] == "none" && (m["origin"] == "request" || m["origin"] == "correction") {
			return true
		}
	}
	return false
}

// revTree resolves a revision of the repository to its tree read-only and stores the
// tree in the library.
func revTree(ctx context.Context, l *library.Library, root, rev string) (string, error) {
	if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n") {
		return "", errors.New("not a revision")
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{tree}").Output()
	if err != nil {
		return "", errors.New("unknown revision")
	}
	gitDir, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", errors.New("not a Git repository")
	}
	return trees.FromGitTree(ctx, l, []string{strings.TrimSpace(string(gitDir))}, strings.TrimSpace(string(out)), trees.Limits{})
}

// readCaseFile reads one case input: the path and every symlink are resolved, the target
// must be a regular file of at most 1 MiB (checked before reading), and a file of the
// base snapshot is refused because the agent would see it. A file of the reference tree is
// refused too, as a deliberate conservative authoring policy (the reference is not in the
// agent context). An ignored local file that no snapshot holds may be used.
func readCaseFile(root, file string, inBase, inRef map[string]bool) ([]byte, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, errors.New("the file cannot be resolved")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{abs, real} {
		for _, r := range []string{root, rootReal} {
			rel, err := filepath.Rel(r, p)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			rel = filepath.ToSlash(rel)
			if inBase[rel] {
				return nil, errors.New("the file is part of the base snapshot, so the agent would see it")
			}
			if inRef[rel] {
				return nil, errors.New("the file is part of the reference tree (refused by the conservative authoring policy)")
			}
		}
	}
	fi, err := os.Lstat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > iodecl.MaxBytes {
		return nil, fmt.Errorf("larger than %d bytes", iodecl.MaxBytes)
	}
	b, err := readSmall(real, iodecl.MaxBytes)
	if err != nil || int64(len(b)) != fi.Size() {
		return nil, errors.New("the file changed while it was read")
	}
	return b, nil
}

// readSmall reads at most max bytes of a regular file; a larger file is an error.
func readSmall(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too large")
	}
	return b, nil
}
