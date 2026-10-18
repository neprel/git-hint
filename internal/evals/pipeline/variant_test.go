package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

var variantNow = time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)

func baselineRef(t *testing.T, l *library.Library) string {
	t.Helper()
	for _, n := range Pending(l) {
		t.Fatalf("pending %s", n)
	}
	names, _ := l.Manifests("eval")
	for _, n := range names {
		var rec map[string]any
		l.Manifest("eval", n, &rec)
		if rec["state"] == "validated" {
			return n
		}
	}
	t.Fatal("no validated baseline")
	return ""
}

// refactoredShop is the baseline's base after a refactoring: a package comment at the top
// of cart.go and a new file. The fix of Total still applies with its context.
func refactoredShop(cart string) map[string]string {
	return map[string]string{"go.mod": evaltest.GoMod, "cart.go": cart, "cart_test.go": evaltest.GoOld, "doc.go": "package shop\n\n// Version of the shop package.\nconst Version = 2\n"}
}

func TestContextVariant(t *testing.T) {
	_, l := validatedProject(t)
	base := baselineRef(t, l)
	v, err := MakeVariant(l, base, VariantInput{Arm: "context_only", Context: []ContextFile{{Path: "AGENTS.md", Data: []byte("Run go test ./... before you finish.\n")}}}, variantNow)
	if err != nil || !v.Created || v.State != "packaged" || v.Reference != "original" {
		t.Fatalf("%+v %v", v, err)
	}
	var rec, b map[string]any
	l.Manifest("eval", v.Ref, &rec)
	l.Manifest("eval", base, &b)
	vr := rec["variant"].(map[string]any)
	d, _ := l.ManifestDigest("eval", base)
	if vr["of"] != base || vr["of_digest"] != d || vr["arm"] != "context_only" || rec["episode_id"] != b["episode_id"] ||
		fmt.Sprint(build.C1Tests(rec)) != fmt.Sprint(build.C1Tests(b)) || fmt.Sprint(rec["requirements"]) != fmt.Sprint(b["requirements"]) {
		t.Fatalf("variant %v", vr)
	}
	tr, _ := trees.Load(l, rec["repositories"].([]any)[0].(map[string]any)["base_tree"].(string))
	found := false
	for _, f := range tr.Files {
		found = found || f.Path == "AGENTS.md"
	}
	if !found || rec["reference"] != b["reference"] {
		t.Fatalf("the context file is not in the base, or the reference changed")
	}
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatal(err)
	}
	// the same input again finds the same variant
	again, err := MakeVariant(l, base, VariantInput{Arm: "context_only", Context: []ContextFile{{Path: "AGENTS.md", Data: []byte("Run go test ./... before you finish.\n")}}}, variantNow)
	if err != nil || again.Created || again.Ref != v.Ref {
		t.Fatalf("again %+v %v", again, err)
	}
}

func TestVariantRefusals(t *testing.T) {
	s, l := validatedProject(t)
	base := baselineRef(t, l)
	ctx := func(p string) VariantInput {
		return VariantInput{Arm: "context_only", Context: []ContextFile{{Path: p, Data: []byte("x\n")}}}
	}
	names, _ := l.Manifests("eval")
	for name, c := range map[string]struct {
		ref string
		in  VariantInput
	}{
		"part of the change": {base, ctx("cart.go")},
		"test file":          {base, ctx("extra_test.go")},
		"hidden test":        {base, ctx("discount_test.go")},
		"no files":           {base, VariantInput{Arm: "context_only"}},
		"label arm":          {base, VariantInput{Arm: "no_change"}},
		"not validated":      {names[0], ctx("AGENTS.md")},
		"unknown refactor":   {base, VariantInput{Arm: "refactor", Base: library.Digest([]byte("none"))}},
	} {
		if _, err := MakeVariant(l, c.ref, c.in, variantNow); !errors.Is(err, ErrVariant) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// a variant arm never runs a baseline, and a baseline arm never runs a variant
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "refactor", Evals: []string{base}}, variantNow); err == nil || !strings.Contains(err.Error(), "needs a variant") {
		t.Fatalf("label-only refactor arm: %v", err)
	}
	v, err := MakeVariant(l, base, ctx("AGENTS.md"), variantNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MakeVariant(l, v.Ref, ctx("NOTES.md"), variantNow); !errors.Is(err, ErrVariant) {
		t.Fatalf("variant of a variant (not validated): %v", err)
	}
	q := Qualify(context.Background(), s, &e2eRunner{}, variantNow)
	if q.States["validated"] != 1 {
		t.Fatalf("context variant qualification %+v", q)
	}
	names, _ = l.Manifests("eval")
	vv := ""
	for _, n := range names {
		if strings.HasPrefix(n, strings.Split(v.Ref, "@")[0]+"@") {
			vv = n
		}
	}
	if vv == v.Ref {
		t.Fatalf("no qualified version of %s", v.Ref)
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "baseline", Evals: []string{vv}}, variantNow); err == nil || !strings.Contains(err.Error(), "cannot run as arm") {
		t.Fatalf("a variant ran as the baseline arm: %v", err)
	}
	// a suite without named evals keeps the variant out
	rep, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle"}, variantNow)
	if err != nil || rep.Trials != 1 {
		t.Fatalf("default suite %+v %v", rep, err)
	}
}

// E14: a refactor variant whose reference is ported onto the refactored base qualifies
// like the baseline, and trials of both arms pair by objective with their digests.
func TestRefactorVariantComparison(t *testing.T) {
	s, l := validatedProject(t)
	base := baselineRef(t, l)
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	nb := evaltest.Tree(t, l, refactoredShop("// Package shop sells things.\n"+evaltest.GoBug))
	v, err := MakeVariant(l, base, VariantInput{Arm: "refactor", Base: nb}, variantNow)
	if err != nil || v.Reference != "reconstructed_patch" {
		t.Fatalf("%+v %v", v, err)
	}
	var rec map[string]any
	l.Manifest("eval", v.Ref, &rec)
	sol, _ := l.GetObject(rec["reference"].(string))
	if !strings.Contains(string(untarAll(t, sol)["files/cart.go"]), "price - discount") {
		t.Fatal("the ported reference lacks the fix")
	}
	if q := Qualify(context.Background(), s, &e2eRunner{}, variantNow); q.States["validated"] != 1 {
		t.Fatalf("qualify %+v", q)
	}
	names, _ := l.Manifests("eval")
	variant := ""
	for _, n := range names {
		if strings.HasPrefix(n, strings.Split(v.Ref, "@")[0]+"@") {
			variant = n
		}
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "baseline", Evals: []string{base}}, variantNow); err != nil {
		t.Fatal(err)
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "refactor", Evals: []string{variant}}, variantNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := Compare(l, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	objective, _, _ := strings.Cut(base, "@")
	for _, r := range rows {
		if r.Objective != objective || r.Passed != 1 {
			t.Fatalf("row %+v", r)
		}
	}
	trials, _ := l.Manifests("trial")
	bd, _ := l.ManifestDigest("eval", base)
	vd, _ := l.ManifestDigest("eval", variant)
	seen := map[string]bool{}
	for _, n := range trials {
		var tr map[string]any
		l.Manifest("trial", n, &tr)
		b := tr["binding"].(map[string]any)
		if tr["objective"] != objective || b["baseline"] != base || b["baseline_digest"] != bd {
			t.Fatalf("trial %v", tr)
		}
		seen[fmt.Sprintf("%v %v", tr["arm"], b["eval_digest"] == vd)] = true
		if err := evals.ValidateValue("trial", tr); err != nil {
			t.Fatal(err)
		}
	}
	if !seen["baseline false"] || !seen["refactor true"] {
		t.Fatalf("bindings %v", seen)
	}
}

// When the change does not port onto the refactored base, the variant has no reference:
// qualification keeps the observed base and kept-behavior results and ends needs_review
// (reference_missing), never reproducible or validated; the run refuses it.
func TestRefactorVariantWithoutReference(t *testing.T) {
	s, l := validatedProject(t)
	base := baselineRef(t, l)
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	// the refactoring renamed Total's parameters: the fix's context no longer matches
	moved := "package shop\n\n// Total returns the price after the discount.\nfunc Total(p, d int) int { return p }\n"
	nb := evaltest.Tree(t, l, refactoredShop(moved))
	v, err := MakeVariant(l, base, VariantInput{Arm: "refactor", Base: nb}, variantNow)
	if err != nil || v.Reference != "missing" {
		t.Fatalf("%+v %v", v, err)
	}
	var rec map[string]any
	l.Manifest("eval", v.Ref, &rec)
	if _, ok := rec["reference"]; ok || !strings.Contains(fmt.Sprint(rec["reasons"]), "reference_missing") {
		t.Fatalf("record %v %v", rec["reference"], rec["reasons"])
	}
	if err := evals.ValidateValue("eval", rec); err != nil {
		t.Fatal(err)
	}
	q := Qualify(context.Background(), s, &e2eRunner{}, variantNow)
	if q.States["needs_review"] != 1 {
		t.Fatalf("qualify %+v", q)
	}
	names, _ := l.Manifests("eval")
	var v2 map[string]any
	for _, n := range names {
		if strings.HasPrefix(n, strings.Split(v.Ref, "@")[0]+"@") {
			l.Manifest("eval", n, &v2)
		}
	}
	qual := v2["qualification"].(map[string]any)
	st := map[string]any{}
	for _, c := range qual["checks"].([]any) {
		st[c.(map[string]any)["name"].(string)] = c.(map[string]any)["status"]
	}
	if v2["state"] != "needs_review" || st["base_fails"] != "pass" || st["reference_passes"] != "missing" || !strings.Contains(fmt.Sprint(v2["reasons"]), "reference_missing") {
		t.Fatalf("state %v checks %v reasons %v", v2["state"], st, v2["reasons"])
	}
	kept := false
	for _, r := range qual["requirements"].([]any) {
		m := r.(map[string]any)
		kept = kept || m["status"] == "pass" // the preserve requirement, observed on the base
	}
	if !kept {
		t.Fatalf("requirements %v", qual["requirements"])
	}
	if _, err := RunSuite(context.Background(), s, &e2eRunner{}, "0.23.0", SuiteOptions{Agent: "oracle", Arm: "refactor", Evals: []string{v.Ref}}, variantNow); err == nil {
		t.Fatal("a variant without a reference ran")
	}
}

// untarAll reads a HINT archive into memory (path -> content).
func untarAll(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = data
	}
	return out
}

// TestHarborRefactorComparison is the E14 check through the real Harbor CLI with no
// model: the synthetic bug fix is qualified, a refactor variant with a ported reference
// is qualified, nop and oracle run on both arms, and the comparison pairs them by
// objective; a refactor whose change does not port ends needs_review. Set HINT_HARBOR.
func TestHarborRefactorComparison(t *testing.T) {
	if os.Getenv("HINT_HARBOR") == "" {
		t.Skip("set HINT_HARBOR to the harbor executable")
	}
	ctx := context.Background()
	h, err := runner.FindHarbor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled(nil)
	p.Run = &evals.Run{Use: "docker", Runner: "harbor", MaxTrialsPerRun: 40}
	s.Cfg.Evals = &p
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"] = 2, fmt.Sprintf("%s@1", rec["eval_id"])
	if _, err := l.PutManifest("eval", v2); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if q := Qualify(ctx, s, h, time.Now()); q.States["validated"] != 1 {
		t.Fatalf("baseline %+v", q)
	}
	base := fmt.Sprintf("%s@3", rec["eval_id"])
	ok, err := MakeVariant(l, base, VariantInput{Arm: "refactor", Base: evaltest.Tree(t, l, refactoredShop("// Package shop sells things.\n"+evaltest.GoBug))}, time.Now())
	if err != nil || ok.Reference != "reconstructed_patch" {
		t.Fatalf("%+v %v", ok, err)
	}
	bad, err := MakeVariant(l, base, VariantInput{Arm: "refactor", Base: evaltest.Tree(t, l, refactoredShop("package shop\n\n// Total returns the price after the discount.\nfunc Total(p, d int) int { return p }\n"))}, time.Now())
	if err != nil || bad.Reference != "missing" {
		t.Fatalf("%+v %v", bad, err)
	}
	q := Qualify(ctx, s, h, time.Now())
	t.Logf("variants: %+v", q)
	if q.States["validated"] != 1 || q.States["needs_review"] != 1 {
		t.Fatalf("variants %+v", q)
	}
	variant := strings.Split(ok.Ref, "@")[0] + "@2"
	var vr map[string]any
	l.Manifest("eval", variant, &vr)
	for _, c := range vr["qualification"].(map[string]any)["checks"].([]any) {
		t.Logf("  refactor %-20v %v", c.(map[string]any)["name"], c.(map[string]any)["status"])
	}
	for _, run := range []struct{ arm, ref, agent string }{{"baseline", base, "nop"}, {"baseline", base, "oracle"}, {"refactor", variant, "nop"}, {"refactor", variant, "oracle"}} {
		rep, err := RunSuite(ctx, s, h, h.Version, SuiteOptions{Agent: run.agent, Arm: run.arm, Evals: []string{run.ref}}, time.Now())
		if err != nil || rep.Trials != 1 {
			t.Fatalf("%v: %+v %v", run, rep, err)
		}
	}
	if _, err := RunSuite(ctx, s, h, h.Version, SuiteOptions{Agent: "oracle", Arm: "refactor", Evals: []string{bad.Ref}}, time.Now()); err == nil {
		t.Fatal("a variant without a reference ran")
	}
	rows, _ := Compare(l, "")
	for _, r := range rows {
		t.Logf("compare objective %s arm %-9s agent %-6s trials %d passed %d failed %d", r.Objective, r.Arm, r.Agent, r.Trials, r.Passed, r.Failed)
		if (r.Agent == "oracle") != (r.Passed == 1) {
			t.Fatalf("row %+v", r)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("rows %d", len(rows))
	}
	t.Logf("harbor %s: %s", h.Version, time.Since(start).Round(time.Second))
}
