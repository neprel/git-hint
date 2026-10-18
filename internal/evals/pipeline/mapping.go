package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/model"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// mapRequirements asks the local model (policy model) to link the unmapped request and
// correction requirements of a packaged eval to its hidden tests. It returns the record
// with the applied mapping and any reason to add. Without the model, or when the answer
// is not usable, the requirements stay unmapped: qualification then reports them as
// missing evidence, and the eval is not validated.
func mapRequirements(ctx context.Context, l *library.Library, p *evals.Policy, b *build.Built, result string) (map[string]any, string) {
	rec, why, _ := mapRequirementsDetail(ctx, l, p, b, result)
	return rec, why
}

// mapDetail is the model's raw answer and the cause of a refusal, for diagnosis (local).
type mapDetail struct {
	Answer string
	Cause  string
}

func mapRequirementsDetail(ctx context.Context, l *library.Library, p *evals.Policy, b *build.Built, result string) (map[string]any, string, mapDetail) {
	var d mapDetail
	rec := b.Record
	unmapped := build.Unmapped(rec)
	if len(unmapped) == 0 {
		return rec, "", d
	}
	if p.Check(evals.OpModel, "") != nil {
		return rec, "model_disabled", d
	}
	ids := build.C1Tests(rec)
	rm := model.RequirementMap{Sources: map[string]string{}, Order: unmapped, TestIDs: ids, Files: map[string]string{},
		Notes: map[string]string{"C2": "the existing tests of the repository: behavior that must stay as it was"}}
	if len(ids) == 0 {
		// a refactoring: C1 compares the exported API with the expected one
		rm.TestIDs = []string{"C1"}
		rm.Notes["C1"] = "the exported API of the changed packages must equal the listed API"
		if api := archiveFile(l, rec["verifier"].(map[string]any)["archive"].(string), "api.expected"); api != "" {
			rm.Files["C1: expected exported API"] = api
		}
	}
	rm.TestIDs = append(rm.TestIDs, "C2")
	inputs := []any{rec["verifier"].(map[string]any)["archive"]}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		if m["mapping"] == "none" {
			t, err := l.GetObject(m["text"].(string))
			if err != nil {
				return rec, "requirement_mapping_incomplete", d
			}
			rm.Sources[m["req_id"].(string)] = string(t)
			inputs = append(inputs, m["text"])
		}
	}
	tree, err := trees.Load(l, result)
	if err != nil {
		return rec, "requirement_mapping_incomplete", d
	}
	for _, f := range tree.Files {
		for _, h := range b.Hidden {
			if f.Path == h {
				src, _ := l.GetObject(f.Digest)
				rm.Files[h] = string(src)
			}
		}
	}
	h := sha256.Sum256([]byte("map\x00" + rec["eval_id"].(string)))
	job := "jb-" + hex.EncodeToString(h[:8])
	c, err := model.New(p, l, job)
	if err != nil {
		return rec, "requirement_mapping_incomplete", d // also when this eval's mapping job ran before
	}
	defer c.Close()
	ans, raw0, err := rm.GenerateRaw(ctx, c)
	d.Answer = raw0
	if err != nil {
		d.Cause = err.Error()
		return rec, "requirement_mapping_incomplete", d
	}
	raw, _ := json.Marshal(ans)
	out, err := l.PutObject(raw)
	if err != nil {
		return rec, "requirement_mapping_incomplete", d
	}
	st, _ := l.Job(job)
	m := build.Mapping{By: "model", Context: ans.Context, Generator: map[string]any{
		"name": "git-hint lan-requirement-map", "version": "1", "model": p.Model.Model, "inputs": inputs, "output": out, "requests": st.Requests}}
	for _, r := range ans.Requirements {
		m.Links = append(m.Links, build.Link{Source: r.Source, Quote: stripLabel(r.Quote), Tests: r.Tests})
	}
	for i, c := range m.Context {
		m.Context[i] = stripLabel(c)
	}
	reqs, err := build.ApplyMapping(l, rec, m)
	if err != nil {
		d.Cause = err.Error()
		return rec, "requirement_mapping_incomplete", d
	}
	next := map[string]any{}
	for k, v := range rec {
		next[k] = v
	}
	next["requirements"] = reqs
	if len(build.Unmapped(next)) > 0 {
		return next, "requirement_coverage_missing", d
	}
	return next, "", d
}

// archiveFile returns one file of a stored tar archive, or "".
func archiveFile(l *library.Library, digest, name string) string {
	b, err := l.GetObject(digest)
	if err != nil {
		return ""
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			return ""
		}
		if h.Name == name && h.Typeflag == tar.TypeReg {
			c, _ := io.ReadAll(io.LimitReader(tr, 64<<10))
			return string(c)
		}
	}
}

var sourceLabel = regexp.MustCompile(`^\s*R[1-9][0-9]{0,2}:\s*`)

// stripLabel removes a leading source label ("R1: ") that a model copies from the prompt
// layout; the quote itself must still be the user's exact text.
func stripLabel(q string) string { return sourceLabel.ReplaceAllString(q, "") }
