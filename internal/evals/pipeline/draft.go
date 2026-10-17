package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/model"
	"github.com/neprel/git-hint/internal/evals/trees"
)

var modulePath = regexp.MustCompile(`(?m)^module\s+(\S+)`)
var pkgClause = regexp.MustCompile(`(?m)^package\s+([A-Za-z_][A-Za-z0-9_]*)`)

// draftVerifier asks the local model for a hidden test when an episode has no test of its
// own. It returns the input with an augmented result tree, or false when no draft is
// possible (model off, budget used up, not a single changed Go file, invalid answer).
func draftVerifier(ctx context.Context, l *library.Library, p *evals.Policy, in build.Input) (build.Input, bool) {
	if p.Check(evals.OpModel, "") != nil || in.Base == "" || in.Result == "" || in.Family == "refactor" {
		return in, false
	}
	base, err1 := trees.Load(l, in.Base)
	result, err2 := trees.Load(l, in.Result)
	if err1 != nil || err2 != nil || build.Language(base) != "go" {
		return in, false
	}
	_, _, changed := trees.Diff(base, result)
	var src []string
	for _, c := range changed {
		if strings.HasSuffix(c, ".go") && !strings.HasSuffix(c, "_test.go") {
			src = append(src, c)
		}
	}
	if len(src) != 1 {
		return in, false
	}
	get := func(t trees.Tree, p string) string {
		for _, f := range t.Files {
			if f.Path == p {
				b, _ := l.GetObject(f.Digest)
				return string(b)
			}
		}
		return ""
	}
	before, after := get(base, src[0]), get(result, src[0])
	pm, mm := pkgClause.FindStringSubmatch(after), modulePath.FindStringSubmatch(get(base, "go.mod"))
	if pm == nil {
		return in, false
	}
	module := ""
	if mm != nil {
		module = mm[1]
	}
	h := sha256.Sum256([]byte("draft\x00" + in.EpisodeID + "\x00" + in.Result))
	c, err := model.New(p, l, "jb-"+hex.EncodeToString(h[:8]))
	if err != nil {
		return in, false // also when this episode's draft job ran before: no second budget
	}
	defer c.Close()
	d := model.GoTestDraft{Package: pm[1], Module: module, Request: in.Request, Before: before, After: after}
	test, err := d.Generate(ctx, c)
	if err != nil {
		return in, false
	}
	testPath := path.Join(path.Dir(src[0]), "hint_draft_test.go")
	if path.Dir(src[0]) == "." {
		testPath = "hint_draft_test.go"
	}
	td, err := l.PutObject([]byte(test))
	if err != nil {
		return in, false
	}
	files := append(append([]trees.File{}, result.Files...), trees.File{Path: testPath, Mode: "100644", Size: int64(len(test)), Digest: td})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	tb, _ := json.Marshal(trees.Tree{TreeVersion: 1, Files: files})
	nr, err := l.PutObject(tb)
	if err != nil {
		return in, false
	}
	st, _ := l.Job("jb-" + hex.EncodeToString(h[:8]))
	reqText, _ := l.PutObject([]byte(in.Request))
	in.Result = nr
	in.Paths = append(append([]string{}, in.Paths...), testPath)
	in.Generated = map[string]map[string]any{testPath: {"name": "git-hint lan-draft", "version": "1", "model": p.Model.Model,
		"inputs": []any{reqText, in.Base, in.Result}, "output": td, "requests": st.Requests}}
	in.CheckHint = "go test ./..."
	return in, true
}
