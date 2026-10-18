package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/patch"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// Variant arms change what the agent gets (E14). Other arms are labels of the run.
var VariantArms = []string{"context_only", "refactor"}

// ErrVariant marks an input that cannot make a comparable variant of an eval.
var ErrVariant = errors.New("no comparable variant")

// ContextFile is a file that a context variant adds to the agent's repository.
type ContextFile struct {
	Path string // relative to the repository root
	Data []byte
	Mode string // "100644" or "100755"
}

// VariantInput is what a variant changes: context files (context_only) or the state of
// a refactored repository (refactor, a tree digest).
type VariantInput struct {
	Arm     string
	Context []ContextFile
	Base    string
}

// Variant is a stored variant eval and its current state.
type Variant struct {
	Ref       string // latest version of the variant eval
	State     string
	Reference string // original, reconstructed_patch or missing
	Created   bool
}

// MakeVariant stores (or finds) the variant of a validated baseline eval version. The
// variant keeps the task objective: the same episode (family and partition), request,
// requirements, hidden tests and environment. A context variant adds files to the base
// and to the reference result. A refactor variant replaces the base by the refactored
// state; the reference change is ported onto it (strict patch) or, when it does not
// apply, the variant has no reference and cannot become validated. The new eval version
// is packaged; the existing qualification worker qualifies it.
func MakeVariant(l *library.Library, baseline string, in VariantInput, now time.Time) (Variant, error) {
	var rec map[string]any
	if err := l.Manifest("eval", baseline, &rec); err != nil {
		return Variant{}, fmt.Errorf("%w: no eval %s", ErrVariant, baseline)
	}
	if rec["state"] != "validated" {
		return Variant{}, fmt.Errorf("%w: %s is %v; a variant needs a validated baseline", ErrVariant, baseline, rec["state"])
	}
	if v, ok := rec["variant"]; ok && v != nil {
		return Variant{}, fmt.Errorf("%w: %s is already a variant", ErrVariant, baseline)
	}
	digest, err := l.ManifestDigest("eval", baseline)
	if err != nil {
		return Variant{}, err
	}
	repos := rec["repositories"].([]any)
	if len(repos) != 1 {
		return Variant{}, fmt.Errorf("%w: %s spans several repositories", ErrVariant, baseline)
	}
	repo := repos[0].(map[string]any)
	base, err := trees.Load(l, str(repo["base_tree"]))
	if err != nil {
		return Variant{}, err
	}
	result, err := trees.Load(l, str(repo["result_tree"]))
	if err != nil {
		return Variant{}, err
	}
	lang := build.Language(base)
	added, removed, changed := trees.Diff(base, result)
	touched := map[string]bool{}
	for _, p := range append(append(append([]string{}, added...), removed...), changed...) {
		touched[p] = true
	}
	bin := build.Input{EpisodeID: str(rec["episode_id"]), Family: str(rec["family"]), Repo: str(repo["repo"]),
		Attribution: "exclusive", Outcome: "test_passed", CheckHint: map[string]string{"go": "go test ./...", "python": "pytest"}[lang], Now: now}
	for p := range touched {
		bin.Paths = append(bin.Paths, p)
	}
	sort.Strings(bin.Paths)
	orig := build.Originals(rec)
	for _, r := range orig["requirements"].([]any) {
		m := r.(map[string]any)
		if m["mapping"] != "none" {
			continue
		}
		text, err := l.GetObject(m["text"].(string))
		if err != nil {
			return Variant{}, err
		}
		at := map[string]any{}
		if ev, _ := m["evidence"].([]any); len(ev) > 0 {
			at, _ = ev[0].(map[string]any)
		}
		if m["origin"] == "request" {
			bin.Request, bin.RequestAt = string(text), at
		} else {
			bin.Corrections = append(bin.Corrections, build.Text{Body: string(text), At: at})
		}
	}
	var inputs []any
	refRole := "original"
	switch in.Arm {
	case "context_only":
		if len(in.Context) == 0 {
			return Variant{}, fmt.Errorf("%w: a context variant needs at least one file", ErrVariant)
		}
		var files []trees.File
		for _, c := range in.Context {
			switch {
			case touched[c.Path]:
				return Variant{}, fmt.Errorf("%w: %s is part of the task's change, not context", ErrVariant, c.Path)
			case build.IsTest(lang, c.Path):
				return Variant{}, fmt.Errorf("%w: %s is a test file, not context", ErrVariant, c.Path)
			}
			d, err := l.PutObject(c.Data)
			if err != nil {
				return Variant{}, err
			}
			mode := c.Mode
			if mode == "" {
				mode = "100644"
			}
			files = append(files, trees.File{Path: c.Path, Mode: mode, Size: int64(len(c.Data)), Digest: d})
			inputs = append(inputs, d)
		}
		if bin.Base, err = overlay(l, base, files); err != nil {
			return Variant{}, err
		}
		if bin.Result, err = overlay(l, result, files); err != nil {
			return Variant{}, err
		}
	case "refactor":
		nb, err := trees.Load(l, in.Base)
		if err != nil {
			return Variant{}, fmt.Errorf("%w: the refactored state is not stored", ErrVariant)
		}
		if build.Language(nb) != lang {
			return Variant{}, fmt.Errorf("%w: the refactored state is not a %s project", ErrVariant, lang)
		}
		inputs = append(inputs, in.Base)
		bin.Base = in.Base
		res, ok, err := port(l, base, result, nb, lang)
		if err != nil {
			return Variant{}, err
		}
		bin.Result = res
		refRole = "reconstructed_patch"
		if !ok {
			refRole, bin.NoReference = "missing", true
		}
	default:
		return Variant{}, fmt.Errorf("%w: arm %q changes nothing in the task (variants: %s)", ErrVariant, in.Arm, strings.Join(VariantArms, ", "))
	}
	built, err := build.Build(l, bin)
	if err != nil {
		return Variant{}, fmt.Errorf("%w: %v", ErrVariant, err)
	}
	v := built.Record
	// the same objective: hidden checks and environment of the baseline
	if !reflect.DeepEqual(build.C1Tests(v), build.C1Tests(rec)) {
		return Variant{}, fmt.Errorf("%w: the hidden tests differ from the baseline's", ErrVariant)
	}
	if !reflect.DeepEqual(v["environment"].(map[string]any)["image"], rec["environment"].(map[string]any)["image"]) {
		return Variant{}, fmt.Errorf("%w: the environment image differs from the baseline's", ErrVariant)
	}
	v["requirements"] = rec["requirements"]
	if c, ok := rec["context"]; ok {
		v["context"] = c
	}
	v["reasons"] = []any{}
	if refRole == "missing" {
		v["reasons"] = []any{"reference_missing"}
	}
	v["variant"] = map[string]any{"arm": in.Arm, "of": baseline, "of_digest": digest, "inputs": inputs, "reference": refRole}
	// the variant's identity is the exact baseline version and digest, the arm and the
	// intervention inputs: the same trees from another baseline version or arm are
	// another variant (G10-9)
	parts := []string{"variant", baseline, digest, in.Arm}
	for _, x := range inputs {
		parts = append(parts, x.(string))
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	id := "ev-" + hex.EncodeToString(h[:8])
	v["eval_id"] = id
	latest := ""
	names, _ := l.Manifests("eval")
	for _, n := range names {
		if strings.HasPrefix(n, id+"@") && (latest == "" || versionOf(strings.SplitN(n, "@", 2)[1]) > versionOf(strings.SplitN(latest, "@", 2)[1])) {
			latest = n
		}
	}
	if latest != "" {
		var cur map[string]any
		if err := l.Manifest("eval", latest, &cur); err != nil {
			return Variant{}, err
		}
		cv, _ := cur["variant"].(map[string]any)
		if cv == nil || cv["of"] != baseline || cv["of_digest"] != digest || cv["arm"] != in.Arm || fmt.Sprint(cv["inputs"]) != fmt.Sprint(inputs) {
			return Variant{}, fmt.Errorf("%w: %s exists with another binding", ErrVariant, latest)
		}
		return Variant{Ref: latest, State: str(cur["state"]), Reference: refRole}, nil
	}
	name, err := l.PutManifest("eval", v)
	if err != nil {
		return Variant{}, err
	}
	return Variant{Ref: name, State: "packaged", Reference: refRole, Created: true}, nil
}

// overlay stores tree t with files added or replaced.
func overlay(l *library.Library, t trees.Tree, files []trees.File) (string, error) {
	m := map[string]trees.File{}
	for _, f := range t.Files {
		m[f.Path] = f
	}
	for _, f := range files {
		m[f.Path] = f
	}
	var out []trees.File
	for _, f := range m {
		out = append(out, f)
	}
	return trees.Put(l, out)
}

// port applies the baseline's change (base -> result) to a refactored base nb. New files
// are added when nb does not have them; removed files are removed; changed files take the
// strict patch. ok is false when any part does not apply; the returned tree then holds nb
// plus the hidden test files only (a variant without a reference).
func port(l *library.Library, base, result, nb trees.Tree, lang string) (string, bool, error) {
	bm, rm, nm := map[string]trees.File{}, map[string]trees.File{}, map[string]trees.File{}
	for _, f := range base.Files {
		bm[f.Path] = f
	}
	for _, f := range result.Files {
		rm[f.Path] = f
	}
	for _, f := range nb.Files {
		nm[f.Path] = f
	}
	added, removed, changed := trees.Diff(base, result)
	out := map[string]trees.File{}
	for p, f := range nm {
		out[p] = f
	}
	tests := map[string]trees.File{}
	ok := true
	for _, p := range added {
		if build.IsTest(lang, p) {
			tests[p] = rm[p]
		}
		if cur, there := nm[p]; there && cur.Digest != rm[p].Digest {
			ok = false
		}
		out[p] = rm[p]
	}
	for _, p := range removed {
		delete(out, p)
	}
	for _, p := range changed {
		cur, there := nm[p]
		if !there {
			ok = false
			continue
		}
		if build.IsTest(lang, p) {
			tests[p] = rm[p]
		}
		b, err1 := l.GetObject(bm[p].Digest)
		r, err2 := l.GetObject(rm[p].Digest)
		t, err3 := l.GetObject(cur.Digest)
		if err := errors.Join(err1, err2, err3); err != nil {
			return "", false, err
		}
		ported, applies := patch.Port(string(b), string(r), string(t))
		if !applies {
			ok = false
			continue
		}
		d, err := l.PutObject([]byte(ported))
		if err != nil {
			return "", false, err
		}
		out[p] = trees.File{Path: p, Mode: cur.Mode, Size: int64(len(ported)), Digest: d}
	}
	if !ok {
		// no reference: the refactored base plus the hidden tests of the task
		out = map[string]trees.File{}
		for p, f := range nm {
			out[p] = f
		}
		for p, f := range tests {
			out[p] = f
		}
	}
	var files []trees.File
	for _, f := range out {
		files = append(files, f)
	}
	d, err := trees.Put(l, files)
	return d, ok, err
}
