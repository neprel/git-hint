package build

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
)

// the reviewer's case: the request adds a requirement that the result tests do not check
const panicRequest = "Total ignores the discount. Fix Total so it subtracts the discount. Total must panic on a negative discount."

func panicEval(t *testing.T, l *library.Library) map[string]any {
	t.Helper()
	in := input(t, l)
	in.Request = panicRequest
	in.Corrections = nil
	b, err := Build(l, in)
	if err != nil {
		t.Fatal(err)
	}
	return b.Record
}

// G10-3: request and correction requirements are not linked to tests by assumption.
func TestRequestRequirementsStartUnmapped(t *testing.T) {
	l := lib(t)
	b, err := Build(l, input(t, l))
	if err != nil {
		t.Fatal(err)
	}
	if u := Unmapped(b.Record); len(u) != 2 || u[0] != "R1" || u[1] != "R2" {
		t.Fatalf("unmapped %v", u)
	}
	for _, r := range b.Record["requirements"].([]any) {
		m := r.(map[string]any)
		if m["mapping"] == "none" && len(m["checks"].([]any)) != 0 {
			t.Fatalf("an unmapped requirement names a check: %v", m)
		}
	}
	if ts := C1Tests(b.Record); len(ts) != 1 || ts[0] != "discount_test.go::TestDiscount" {
		t.Fatalf("C1 tests %v", ts)
	}
	// an eval with unmapped requirements cannot be stored as validated
	v := map[string]any{}
	for k, x := range b.Record {
		v[k] = x
	}
	v["state"] = "validated"
	if err := evals.ValidateValue("eval", v); err == nil {
		t.Fatal("a validated eval with an unmapped requirement passed the schema")
	}
}

func TestApplyMapping(t *testing.T) {
	l := lib(t)
	rec := panicEval(t, l)
	const test = "discount_test.go::TestDiscount"
	cases := []struct {
		name string
		m    Mapping
		ok   bool
	}{
		{"panic-clause-dropped", Mapping{By: "model", Links: []Link{{"R1", "Fix Total so it subtracts the discount", []string{test}}},
			Context: []string{"Total ignores the discount."}}, false},
		{"quote-not-in-request", Mapping{By: "model", Links: []Link{{"R1", "Total rounds the result", []string{test}}},
			Context: []string{panicRequest}}, false},
		{"unknown-test", Mapping{By: "model", Links: []Link{{"R1", panicRequest, []string{"x_test.go::TestX"}}}}, false},
		{"unknown-source", Mapping{By: "model", Links: []Link{{"R9", panicRequest, []string{test}}}}, false},
		{"unknown-origin", Mapping{By: "", Links: []Link{{"R1", panicRequest, []string{test}}}}, false},
		{"context-not-in-request", Mapping{By: "user", Links: []Link{{"R1", panicRequest, []string{test}}},
			Context: []string{"Total rounds the result"}}, false},
		{"complete", Mapping{By: "model", Links: []Link{
			{"R1", "Fix Total so it subtracts the discount", []string{test}},
			{"R1", "total must panic on a negative   discount", []string{test}},
			{"R1", "Total ignores the discount", []string{test}}}}, true},
	}
	for _, c := range cases {
		reqs, err := ApplyMapping(l, rec, c.m)
		if (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.ok {
			if !errors.Is(err, ErrMapping) {
				t.Fatalf("%s: %v", c.name, err)
			}
			continue
		}
		if len(reqs) != 5 {
			t.Fatalf("%s: %d requirements", c.name, len(reqs))
		}
		v := map[string]any{}
		for k, x := range rec {
			v[k] = x
		}
		v["requirements"], v["state"], v["created_at"] = reqs, "validated", time.Now().UTC().Format(time.RFC3339)
		if err := evals.ValidateValue("eval", v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r2 := reqs[1].(map[string]any)
		if r2["req_id"] != "R2" || r2["mapping"] != "model" || r2["origin"] != "request" || r2["tests"].([]any)[0] != test {
			t.Fatalf("%s: %v", c.name, r2)
		}
		if q, _ := l.GetObject(r2["text"].(string)); string(q) != "total must panic on a negative   discount" {
			t.Fatalf("%s: text %q", c.name, q)
		}
		if q, _ := l.GetObject(r2["source"].(string)); string(q) != panicRequest {
			t.Fatalf("%s: source %q", c.name, q)
		}
	}
	// an atomic requirement that no test verifies stays unmapped and named
	reqs, err := ApplyMapping(l, rec, Mapping{By: "model", Links: []Link{
		{"R1", "Fix Total so it subtracts the discount", []string{test}},
		{"R1", "Total must panic on a negative discount", nil}},
		Context: []string{"Total ignores the discount"}})
	if err != nil {
		t.Fatal(err)
	}
	// the model's context sentence stays an unmapped requirement too (R3)
	r2 := reqs[1].(map[string]any)
	if r2["mapping"] != "none" || len(r2["checks"].([]any)) != 0 {
		t.Fatalf("%v", r2)
	}
	v := map[string]any{}
	for k, x := range rec {
		v[k] = x
	}
	v["requirements"] = reqs
	if err := evals.ValidateValue("eval", v); err != nil {
		t.Fatal(err)
	}
	if u := Unmapped(v); len(u) != 2 || u[0] != "R2" || u[1] != "R3" {
		t.Fatalf("unmapped %v", u)
	}
}

// G10-3, review round 2 (the reviewer's TestReviewerModelContextDropsRequirement): a model
// answer put a requested condition of R1 into context and linked the rest to the C1
// tests. ApplyMapping accepted it, the panic text disappeared and Unmapped was empty. A
// model's context and a link that joins several sentences now stay unmapped requirements.
func TestReviewerModelContextDropsRequirement(t *testing.T) {
	l := lib(t)
	rec := panicEval(t, l)
	panicText := "Total must panic on a negative discount."
	for name, m := range map[string]Mapping{
		"context":        {By: "model", Links: []Link{{"R1", "Total ignores the discount. Fix Total so it subtracts the discount.", C1Tests(rec)}}, Context: []string{panicText}},
		"one-sentence":   {By: "model", Links: []Link{{"R1", "Fix Total so it subtracts the discount", C1Tests(rec)}}, Context: []string{"Total ignores the discount.", panicText}},
		"joined-in-link": {By: "model", Links: []Link{{"R1", panicRequest, C1Tests(rec)}}},
	} {
		reqs, err := ApplyMapping(l, rec, m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v := map[string]any{}
		for k, x := range rec {
			v[k] = x
		}
		v["requirements"] = reqs
		if err := evals.ValidateValue("eval", v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, r := range reqs {
			q := r.(map[string]any)
			text, _ := l.GetObject(q["text"].(string))
			if strings.Contains(string(text), "panic") {
				found = true
				if q["mapping"] != "none" {
					t.Fatalf("%s: the panic condition got coverage from a model answer: %v", name, q)
				}
			}
		}
		if !found || len(Unmapped(v)) == 0 {
			t.Fatalf("%s: the panic condition disappeared; unmapped %v", name, Unmapped(v))
		}
		for _, r := range reqs {
			q := r.(map[string]any)
			d, _ := q["source"].(string)
			if src, _ := l.GetObject(d); q["origin"] == "request" && string(src) != panicRequest {
				t.Fatalf("%s: the original request is not kept: %q", name, src)
			}
		}
	}
	// the user can confirm context
	reqs, err := ApplyMapping(l, rec, Mapping{By: "user", Links: []Link{
		{"R1", "Fix Total so it subtracts the discount", C1Tests(rec)},
		{"R1", panicText, nil}}, Context: []string{"Total ignores the discount."}})
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]any{"requirements": reqs}
	if u := Unmapped(v); len(u) != 1 || u[0] != "R2" {
		t.Fatalf("user mapping: unmapped %v", u)
	}
}

// A refactoring links its requested structure to the API check and the kept behavior to
// the existing tests; an internal part of the request that no check sees stays unmapped.
func TestApplyMappingWholeChecks(t *testing.T) {
	l := lib(t)
	in := input(t, l)
	in.Family, in.Corrections = "refactor", nil
	in.Request = "Rename Total to Sum and make it use integer math; keep the behavior."
	in.Base = mkTree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "cart.go": "package shop\n\n// Total sums.\nfunc Total(p, d int) int { return p - d }\n", "cart_test.go": "package shop\n"})
	in.Result = mkTree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "cart.go": "package shop\n\n// Sum sums.\nfunc Sum(p, d int) int { return p - d }\n", "cart_test.go": "package shop\n"})
	in.Paths = []string{"cart.go"}
	b, err := Build(l, in)
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := ApplyMapping(l, b.Record, Mapping{By: "model", Links: []Link{
		{"R1", "Rename Total to Sum", []string{"C1"}},
		{"R1", "make it use integer math", nil},
		{"R1", "keep the behavior", []string{"C2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	k := func(i int) map[string]any { return reqs[i].(map[string]any) }
	if k(0)["kind"] != "structure" || k(0)["checks"].([]any)[0] != "C1" || k(1)["mapping"] != "none" || k(2)["kind"] != "preserve" || k(2)["checks"].([]any)[0] != "C2" {
		t.Fatalf("%v", reqs)
	}
	if _, err := ApplyMapping(l, b.Record, Mapping{By: "model", Links: []Link{{"R1", in.Request, []string{"C1", "C2"}}}}); err == nil {
		t.Fatal("a link to two whole checks was accepted")
	}
}
