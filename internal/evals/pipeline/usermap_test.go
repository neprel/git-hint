package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/testkit"
)

func storedBugFix(t *testing.T) (*library.Library, string) {
	t.Helper()
	l := evaltest.Library(t)
	name, err := l.PutManifest("eval", evaltest.GoBugFix(t, l))
	if err != nil {
		t.Fatal(err)
	}
	return l, name
}

func encode(t *testing.T, f MapFile) []byte {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var mapNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// Q-10-2: without a model the file shows each original request as one requirement
// without tests; the user's links and context make a new packaged version, which the
// existing qualification path picks up. Nothing becomes validated here.
func TestMapFileWithoutModel(t *testing.T) {
	l, name := storedBugFix(t)
	f, err := MapDraft(l, name)
	if err != nil {
		t.Fatal(err)
	}
	if f.Eval != name || f.Digest == "" || f.Draft != "" || len(f.Requirements) != 1 || len(f.Requirements[0].Tests) != 0 ||
		f.Requirements[0].Quote != f.Sources["R1"] || strings.Join(f.Tests, ",") != evaltest.GoBugFixTest+",C2" {
		t.Fatalf("draft %+v", f)
	}
	f.Requirements = []build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: []string{evaltest.GoBugFixTest}}}
	f.Context = []string{"Total ignores the discount."}
	next, unmapped, err := ApplyMapFile(context.Background(), l, encode(t, f), t.TempDir(), mapNow)
	if err != nil || len(unmapped) != 0 {
		t.Fatalf("apply: %v %v", err, unmapped)
	}
	var rec map[string]any
	l.Manifest("eval", next, &rec)
	if rec["state"] != "packaged" || rec["previous"] != name || len(rec["qualification"].(map[string]any)["checks"].([]any)) != 0 || len(rec["reasons"].([]any)) != 0 {
		t.Fatalf("new version %v %v %v", rec["state"], rec["previous"], rec["reasons"])
	}
	r1 := rec["requirements"].([]any)[0].(map[string]any)
	if r1["mapping"] != "user" || r1["generator"] != nil {
		t.Fatalf("R1 %v", r1)
	}
	if cs, _ := rec["context"].([]any); len(cs) != 1 || cs[0].(map[string]any)["by"] != "user" || cs[0].(map[string]any)["source"] != r1["source"] {
		t.Fatalf("confirmed context %v", rec["context"])
	}
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatal(err)
	}
	if p := Pending(l); len(p) != 1 || p[0] != next {
		t.Fatalf("pending %v", p)
	}
	// the same file again is stale: the eval has a newer version now
	if _, _, err := ApplyMapFile(context.Background(), l, encode(t, f), t.TempDir(), mapNow); !errors.Is(err, ErrStale) {
		t.Fatalf("stale file: %v", err)
	}
	// the next file starts from the originals again, with the user's links as draft
	g, err := MapDraft(l, next)
	if err != nil || len(g.Sources) != 1 || g.Sources["R1"] != f.Sources["R1"] || len(g.Requirements) != 1 || g.Requirements[0].Tests[0] != evaltest.GoBugFixTest || len(g.Context) != 1 {
		t.Fatalf("second draft %+v %v", g, err)
	}
}

func TestMapFileRefusals(t *testing.T) {
	l, name := storedBugFix(t)
	f, _ := MapDraft(l, name)
	cases := map[string]func(f *MapFile){
		"changed digest": func(f *MapFile) { f.Digest = library.Digest([]byte("other")) },
		"unknown eval":   func(f *MapFile) { f.Eval = "ev-0000000000000000@1" },
		"unknown test":   func(f *MapFile) { f.Requirements[0].Tests = []string{"x_test.go::TestX"} },
		"wrong quote":    func(f *MapFile) { f.Requirements[0].Quote = "Total rounds the price" },
		"part left out": func(f *MapFile) {
			f.Requirements[0] = build.Link{Source: "R1", Quote: "Fix Total", Tests: []string{evaltest.GoBugFixTest}}
		},
		"bad claim": func(f *MapFile) { f.Alternatives = []MapAlternative{{Worktree: ".", ClaimedBy: "model"}} },
	}
	for name, change := range cases {
		g := f
		g.Requirements = append([]build.Link{}, f.Requirements...)
		change(&g)
		if _, _, err := ApplyMapFile(context.Background(), l, encode(t, g), t.TempDir(), mapNow); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, _, err := ApplyMapFile(context.Background(), l, []byte(`{"map_version":1,"eval":"x","digest":"y","requirements":[],"context":[],"alternatives":[],"extra":1}`), "", mapNow); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if names, _ := l.Manifests("eval"); len(names) != 1 {
		t.Fatalf("a refused file stored a version: %v", names)
	}
}

// A model draft stays an estimate: the file marks it, a model context sentence appears as
// a requirement without tests, and a link the user keeps unchanged keeps the model's
// provenance next to mapping "user".
func TestMapFileFromModelDraft(t *testing.T) {
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	reqs, err := build.ApplyMapping(l, rec, build.Mapping{By: "model", Generator: map[string]any{"name": "requirement_map", "version": "1", "inputs": []any{}, "output": library.Digest([]byte("answer")), "requests": 1},
		Links:   []build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: []string{evaltest.GoBugFixTest}}},
		Context: []string{"Total ignores the discount."}})
	if err != nil {
		t.Fatal(err)
	}
	rec["requirements"] = reqs
	name, err := l.PutManifest("eval", rec)
	if err != nil {
		t.Fatal(err)
	}
	f, err := MapDraft(l, name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Draft, "model estimate") || len(f.Sources) != 1 || len(f.Requirements) != 2 || len(f.Requirements[1].Tests) != 0 || f.Requirements[1].Quote != "Total ignores the discount." {
		t.Fatalf("draft %+v", f)
	}
	// the user confirms the background sentence as context
	f.Context, f.Requirements = []string{f.Requirements[1].Quote}, f.Requirements[:1]
	next, unmapped, err := ApplyMapFile(context.Background(), l, encode(t, f), "", mapNow)
	if err != nil || len(unmapped) != 0 {
		t.Fatalf("apply %v %v", err, unmapped)
	}
	var out map[string]any
	l.Manifest("eval", next, &out)
	r1 := out["requirements"].([]any)[0].(map[string]any)
	if r1["mapping"] != "user" || r1["generator"] == nil {
		t.Fatalf("R1 %v", r1)
	}
	if err := evals.ValidateValue("eval", out); err != nil {
		t.Fatal(err)
	}
}

// Existing alternatives are stored with their provenance and the user's claim; the
// qualification reads them back.
func TestMapFileAlternatives(t *testing.T) {
	l, name := storedBugFix(t)
	alt := t.TempDir()
	testkit.Git(t, alt, "init", "-q")
	for p, c := range map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoFix, "cart_test.go": evaltest.GoOld} {
		os.WriteFile(filepath.Join(alt, p), []byte(c), 0o644)
	}
	f, _ := MapDraft(l, name)
	f.Requirements = []build.Link{{Source: "R1", Quote: f.Sources["R1"], Tests: []string{evaltest.GoBugFixTest}}}
	f.Alternatives = []MapAlternative{{Worktree: filepath.Base(alt), ClaimedBy: "user"}, {Worktree: alt}}
	next, _, err := ApplyMapFile(context.Background(), l, encode(t, f), filepath.Dir(alt), mapNow)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	l.Manifest("eval", next, &rec)
	as := Alternatives(rec)
	if len(as) != 2 || as[0].Source != "user" || as[0].ClaimedBy != "user" || as[1].ClaimedBy != "" || as[0].Tree != as[1].Tree {
		t.Fatalf("alternatives %+v", as)
	}
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatal(err)
	}
}
