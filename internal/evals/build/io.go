package build

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// IOCase is one declared B1 case: its ID and the exact stdin bytes.
type IOCase struct {
	ID    string
	Input []byte
}

// InputIO declares one cli-file-v1 eval (07B-J §17.2): a base tree, the stored reference
// (the result tree), a declared command that reads stdin and writes stdout, and the
// evaluator's own cases. Nothing is extracted from tests and no existing task is
// converted: every part is declared by its author (synthetic inputs first).
type InputIO struct {
	EpisodeID   string
	Family      string
	Repo        string
	Base        string // HINT tree digest
	Result      string // HINT tree digest of the reference result
	Request     string
	RequestAt   map[string]any
	Entry       []string // run in Workdir, for example ["python", "tool.py"]
	Cases       []IOCase
	CaseSeconds int
	Now         time.Time
}

// BuildIO packages a declared cli-file-v1 eval. Its only check is B1: the command's stdout
// and exit status on each case equal the stored reference's (finite reference-equivalence,
// not universal correctness). The verifier archive holds only io/spec.json and io/inputs;
// the cases and their inputs never reach the agent image.
func BuildIO(l *library.Library, in InputIO) (*Built, error) {
	var reasons []string
	if in.Base == "" {
		reasons = append(reasons, "baseline_missing")
	}
	if in.Result == "" {
		reasons = append(reasons, "result_state_missing")
	}
	if strings.TrimSpace(in.Request) == "" {
		reasons = append(reasons, "requirements_missing")
	}
	if len(reasons) > 0 {
		return nil, &Refusal{reasons}
	}
	base, err := trees.Load(l, in.Base)
	if err != nil {
		return nil, &Refusal{[]string{"baseline_missing"}}
	}
	result, err := trees.Load(l, in.Result)
	if err != nil {
		return nil, &Refusal{[]string{"result_state_missing"}}
	}
	lang := Language(base)
	if lang == "" {
		return nil, &Refusal{[]string{"verifier_missing"}}
	}
	img, err := pickImage(l, lang, base)
	if err != nil {
		return nil, err
	}
	// the declaration, checked with the runner's own rules
	ids := make([]string, 0, len(in.Cases))
	inputs := map[string][]byte{}
	for _, c := range in.Cases {
		ids = append(ids, c.ID)
		inputs[c.ID] = c.Input
	}
	spec, _ := json.Marshal(map[string]any{"interface": iodecl.Interface, "entry": in.Entry, "cases": ids, "case_seconds": in.CaseSeconds, "compare": "exact"})
	if _, err := iodecl.Parse(spec, inputs); err != nil || len(inputs) != len(in.Cases) {
		return nil, &Refusal{[]string{"verifier_missing"}}
	}
	files := []extra{{"io/spec.json", spec, 0o644}}
	var tests []any
	for _, c := range in.Cases {
		files = append(files, extra{"io/inputs/" + c.ID + ".in", c.Input, 0o644})
		tests = append(tests, "cases/"+c.ID+"::output")
	}
	vArchive, err := tarTree(l, nil, "", files...)
	if err != nil {
		return nil, err
	}
	vArc, err := l.PutObject(vArchive)
	if err != nil {
		return nil, err
	}
	// the stored reference: every changed file of the result, and the removed ones
	added, removed, changed := trees.Diff(base, result)
	resFiles := map[string]trees.File{}
	for _, f := range result.Files {
		resFiles[f.Path] = f
	}
	var solution []string
	sfiles := []trees.File{}
	for _, p := range append(append([]string{}, added...), changed...) {
		solution = append(solution, p)
		sfiles = append(sfiles, resFiles[p])
	}
	if len(solution)+len(removed) == 0 {
		return nil, &Refusal{[]string{"no_checkable_outcome"}}
	}
	sArchive, err := tarTree(l, sfiles, "files/", extra{"solve.sh", []byte(solveScript), 0o755}, extra{"deleted.txt", []byte(strings.Join(removed, "\n")), 0o644})
	if err != nil {
		return nil, err
	}
	sArc, err := l.PutObject(sArchive)
	if err != nil {
		return nil, err
	}
	baseArchive, err := tarTree(l, base.Files, "")
	if err != nil {
		return nil, err
	}
	baseArc, err := l.PutObject(baseArchive)
	if err != nil {
		return nil, err
	}
	instr := instruction(Input{Request: in.Request}, lang) + fmt.Sprintf(
		"\nThe program is run as %q in %s with one input on stdin; its stdout and exit status are checked.\n", strings.Join(in.Entry, " "), Workdir)
	instrD, err := l.PutObject([]byte(instr))
	if err != nil {
		return nil, err
	}
	setupD, err := l.PutObject([]byte(setupScript(lang)))
	if err != nil {
		return nil, err
	}
	reqText, _ := l.PutObject([]byte(in.Request))
	refText, _ := l.PutObject([]byte("The program's stdout and exit status equal the stored reference's on every declared case."))
	reqs := []any{
		map[string]any{"req_id": "R1", "kind": "behavior", "origin": "request", "text": reqText, "evidence": []any{in.RequestAt}, "checks": []any{}, "mapping": "none"},
		map[string]any{"req_id": "R2", "kind": "behavior", "origin": "result_state", "text": refText, "evidence": []any{}, "checks": []any{"B1"}, "mapping": "construction"},
	}
	rec := map[string]any{
		"record": "eval", "manifest_version": 1, "eval_id": "ev-" + short(in.EpisodeID, in.Base, in.Result, "cli-file-v1"), "version": 1, "previous": nil,
		"created_at": in.Now.UTC().Format(time.RFC3339), "episode_id": in.EpisodeID, "family": orDefault(in.Family, "bug_fix"),
		"repositories": []any{map[string]any{"repo": in.Repo, "base_tree": in.Base, "base_archive": baseArc, "result_tree": in.Result}},
		"instruction":  instrD, "requirements": reqs,
		"verifier": map[string]any{"archive": vArc, "kind": iodecl.Interface,
			"checks": []any{map[string]any{"check_id": "B1", "origin": "declared_io", "tests": tests}}},
		"reference":    sArc,
		"environment":  map[string]any{"image": img.Ref, "setup": setupD, "workdir": Workdir, "test_command": "tests/io/spec.json"},
		"dependencies": []any{}, "state": "packaged", "reasons": []any{},
		"qualification": map[string]any{"policy": "q3", "checks": []any{}}, "override": nil,
	}
	return &Built{Record: rec, Language: lang, Solution: solution, Deleted: removed}, nil
}
