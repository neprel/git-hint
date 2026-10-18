package build

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
)

// Link is one atomic requirement: an exact quote from a request or correction (by its
// requirement ID, for example R1) and the C1 tests that verify it, or one whole check:
// "C2" (the existing tests, for kept behavior) or "C1" when C1 has no test functions (the
// API check of a refactoring).
type Link struct {
	Source string   `json:"source"`
	Quote  string   `json:"quote"`
	Tests  []string `json:"tests"`
}

// Mapping is a requirement-to-test matrix for the request and the corrections of an eval.
// Context lists quoted sentences that state no requirement (background, a description of
// the defect). By is "model" or "user". Only the user can confirm context: a sentence that
// the model calls context stays an unmapped requirement, because a model answer must not
// remove a requested condition (G10-3, review round 2).
type Mapping struct {
	Links   []Link   `json:"requirements"`
	Context []string `json:"context"`
	By      string   `json:"-"`
	// Generator is the provenance of a model mapping (name, version, model, inputs,
	// output, requests), kept with each requirement it produced.
	Generator map[string]any `json:"-"`
}

// ErrMapping marks a mapping that cannot be applied; the eval keeps its unmapped
// requirements.
var ErrMapping = errors.New("the requirement mapping is not usable")

var word = regexp.MustCompile(`[\pL\pN_][\pL\pN_'-]*`)

// connectors may stay outside every quote.
var connectors = map[string]bool{"and": true, "also": true, "then": true, "but": true, "the": true, "please": true, "too": true}

func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// sentenceBreak finds a sentence boundary inside a quote.
var sentenceBreak = regexp.MustCompile(`[.!?;]\s+\S`)

// uncovered returns the first content word of text that no quote occurrence covers.
func uncovered(text string, quotes []string) string {
	covered := make([]bool, len(text))
	for _, q := range quotes {
		q = strings.Trim(q, " .!?;:,")
		if q == "" {
			continue
		}
		for i := 0; ; {
			j := strings.Index(text[i:], q)
			if j < 0 {
				break
			}
			for k := i + j; k < i+j+len(q); k++ {
				covered[k] = true
			}
			i += j + 1
		}
	}
	for _, loc := range word.FindAllStringIndex(text, -1) {
		w := text[loc[0]:loc[1]]
		if len(w) >= 3 && !connectors[w] && !covered[loc[0]] {
			return w
		}
	}
	return ""
}

// Unmapped returns the request and correction requirements of rec that have no check.
func Unmapped(rec map[string]any) []string {
	var out []string
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		if m["mapping"] == "none" {
			out = append(out, m["req_id"].(string))
		}
	}
	return out
}

// C1Tests returns the test IDs that check C1 of rec runs.
func C1Tests(rec map[string]any) []string {
	var out []string
	for _, c := range rec["verifier"].(map[string]any)["checks"].([]any) {
		m := c.(map[string]any)
		if m["check_id"] == "C1" {
			ts, _ := m["tests"].([]any)
			for _, t := range ts {
				out = append(out, t.(string))
			}
		}
	}
	return out
}

// ApplyMapping replaces the unmapped request and correction requirements of rec by the
// atomic requirements of m and returns the new requirement list. Each atomic requirement
// keeps the original request or correction as its source. An atomic requirement without
// a test stays unmapped (no check). It refuses a mapping that quotes text the user did not
// write, names a test that C1 does not run, or leaves a content word of a source outside
// every requirement and context quote (an omitted requirement must not disappear
// silently). An exact quote is not evidence of coverage, so a model mapping gets less
// trust: its context sentences and its links that join several sentences stay unmapped
// requirements, which keep the eval in needs_review until the user confirms them.
func ApplyMapping(l *library.Library, rec map[string]any, m Mapping) ([]any, error) {
	if m.By != "model" && m.By != "user" {
		return nil, fmt.Errorf("%w: unknown origin", ErrMapping)
	}
	tests := map[string]bool{}
	for _, t := range C1Tests(rec) {
		tests[t] = true
	}
	// whole checks: C2 (existing tests: kept behavior), C1 when it has no test functions
	// (the API check of a refactoring)
	whole := map[string]bool{"C2": true, "C1": len(tests) == 0}
	type source struct {
		req  map[string]any
		text string
	}
	sources := map[string]source{}
	var order []string
	var kept []any
	for _, r := range rec["requirements"].([]any) {
		req := r.(map[string]any)
		if req["mapping"] != "none" {
			kept = append(kept, req)
			continue
		}
		b, err := l.GetObject(req["text"].(string))
		if err != nil {
			return nil, err
		}
		sources[req["req_id"].(string)] = source{req, string(b)}
		order = append(order, req["req_id"].(string))
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: nothing to map", ErrMapping)
	}
	quotes := map[string][]string{} // source -> normalized quotes
	links := append([]Link{}, m.Links...)
	// a context quote must be the user's text too; a model's context stays a requirement
	var context []string
	for _, c := range m.Context {
		q := strings.Trim(norm(c), " .!?;:,")
		if q == "" {
			continue
		}
		in := ""
		for _, id := range order {
			if strings.Contains(norm(sources[id].text), q) {
				in = id
				break
			}
		}
		if in == "" {
			return nil, fmt.Errorf("%w: a context quote is not in the user's text", ErrMapping)
		}
		if m.By == "model" {
			links = append(links, Link{Source: in, Quote: strings.Trim(strings.TrimSpace(c), " ")})
			continue
		}
		context = append(context, q)
	}
	for _, ln := range links {
		src, ok := sources[ln.Source]
		if !ok {
			return nil, fmt.Errorf("%w: unknown source %q", ErrMapping, ln.Source)
		}
		q := strings.Trim(norm(ln.Quote), " .!?;:,")
		if len(q) < 3 || !strings.Contains(norm(src.text), q) {
			return nil, fmt.Errorf("%w: a quote is not in the user's text", ErrMapping)
		}
		for _, t := range ln.Tests {
			if whole[t] && len(ln.Tests) == 1 {
				continue
			}
			if !tests[t] {
				return nil, fmt.Errorf("%w: test %q is not run by C1", ErrMapping, t)
			}
		}
		quotes[ln.Source] = append(quotes[ln.Source], q)
	}
	for _, id := range order {
		qs := append([]string{}, quotes[id]...)
		qs = append(qs, context...)
		if w := uncovered(norm(sources[id].text), qs); w != "" {
			return nil, fmt.Errorf("%w: a part of %s is neither a requirement nor context", ErrMapping, id)
		}
	}
	var out []any
	for _, ln := range links {
		if m.By == "model" && sentenceBreak.MatchString(strings.Trim(strings.TrimSpace(ln.Quote), " .!?;:,")) {
			ln.Tests = nil // a model link for several sentences is not trusted
		}
		src := sources[ln.Source].req
		d, err := l.PutObject([]byte(strings.TrimSpace(ln.Quote)))
		if err != nil {
			return nil, err
		}
		ts := []any{}
		for _, t := range ln.Tests {
			ts = append(ts, t)
		}
		r := map[string]any{"kind": "behavior", "origin": src["origin"], "text": d, "evidence": src["evidence"],
			"checks": []any{"C1"}, "tests": ts, "mapping": m.By, "source": src["text"]}
		switch {
		case len(ts) == 0:
			// an atomic requirement that no test verifies stays without a check
			r["checks"], r["mapping"] = []any{}, "none"
			delete(r, "tests")
		case whole[ln.Tests[0]]:
			r["checks"] = []any{ln.Tests[0]}
			delete(r, "tests")
			if ln.Tests[0] == "C2" {
				r["kind"] = "preserve" // kept behavior: the base passes it too
			} else {
				r["kind"] = "structure"
			}
		}
		if m.Generator != nil {
			r["generator"] = m.Generator
		}
		out = append(out, r)
	}
	out = append(out, kept...)
	if len(out) > 999 {
		return nil, fmt.Errorf("%w: too many requirements", ErrMapping)
	}
	for i, r := range out {
		r.(map[string]any)["req_id"] = fmt.Sprintf("R%d", i+1)
	}
	return out, nil
}

// Originals returns a copy of rec whose request and correction requirements are the
// original texts again, unmapped, in the order of first appearance and before the other
// requirements. The atomic requirements that a mapping made from one original (same
// source) collapse into it. A new mapping always starts from these originals.
func Originals(rec map[string]any) map[string]any {
	var sources, others []any
	seen := map[string]bool{}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		origin := m["origin"]
		if origin != "request" && origin != "correction" {
			others = append(others, m)
			continue
		}
		text, _ := m["source"].(string)
		if text == "" {
			text, _ = m["text"].(string) // never mapped: the requirement is the original
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		sources = append(sources, map[string]any{"kind": "behavior", "origin": origin, "text": text, "evidence": m["evidence"], "checks": []any{}, "mapping": "none"})
	}
	out := map[string]any{}
	for k, v := range rec {
		out[k] = v
	}
	reqs := append(sources, others...)
	for i, r := range reqs {
		c := map[string]any{}
		for k, v := range r.(map[string]any) {
			c[k] = v
		}
		c["req_id"] = fmt.Sprintf("R%d", i+1)
		reqs[i] = c
	}
	out["requirements"] = reqs
	return out
}
