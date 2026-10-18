package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

// D03: the local report shows at most five findings in the server's order, with local names,
// unknown values as unknown and the heuristic label; a v0.1 set shows no findings section.
func TestRenderFindings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", "recommendation-findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	payload := doc["payload"].(map[string]any)
	fs := payload["findings"].([]any)
	target := fs[0].(map[string]any)["targets"].([]any)[0].(string)
	cur := &Current{Names: map[string]string{target: "billing/ledger.go"}}
	im := &Imported{Result: hint.ImportResult{Document: "accepted"}, Doc: doc}
	md := Render(im, cur, "x.json")
	for _, want := range []string{"## Summary", "- Problems found: 2 (1 with enough evidence, 1 limited). Not diagnosed: coordination_wait.",
		"- First problem: repeated_navigation (supported). Proposed action: COLLECT_MORE_EVIDENCE. Next step: refresh_index_and_recheck.",
		"- Evaluation: 1 eval comparisons of your project", "Kinds of values: **observed**", "## Findings", "heuristic priority, not a calibrated ranking", "### Finding 1: repeated_navigation (supported)",
		"- stale_index: **supported** (stale_context_measurement, repeat_without_observed_edit)", "| large_read_bytes | bytes | unknown | 2 of 5 |",
		"Next step: **refresh_index_and_recheck** on billing/ledger.go", "- coordination_wait: insufficient_coverage", "### Finding 2: large_low_selectivity_read (limited)",
		"Intervention comparison (heuristic; cost model unavailable, so no net value or break-even): **COLLECT_MORE_EVIDENCE**.",
		"| CONTEXT_ONLY | unbound | unknown | unknown | none |", "No eval comparison is bound to this finding's objectives",
		"## Project eval comparisons", "| context_only | held_out | 6 | -120.5 (-400 to 300; 6 of 6) | 0.1 (0 to 0.2; 6 of 6) |",
		"## Context policy feedback", "**keep** (expansion_is_normal, less_total_navigation, no_worse_outcome_with_delivery)",
		"  - total_navigation_per_task: 4.5 operations_per_task"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Index(md, "## Summary") > strings.Index(md, "## Findings") || strings.Index(md, "## Findings") > strings.Index(md, "## Plan 1") {
		t.Error("findings come after the plans")
	}
	// seven findings: five are shown
	for len(fs) < 7 {
		fs = append(fs, fs[1])
	}
	payload["findings"] = fs
	md = Render(im, cur, "x.json")
	if strings.Count(md, "### Finding ") != 5 || !strings.Contains(md, "2 more findings are not shown.") {
		t.Errorf("limit: %d findings shown", strings.Count(md, "### Finding "))
	}
	delete(payload, "findings")
	delete(payload, "abstentions")
	delete(payload, "context_feedback")
	delete(payload, "project_comparisons")
	if md := Render(im, cur, "x.json"); strings.Contains(md, "## Findings") || strings.Contains(md, "## Summary") {
		t.Error("a set without findings shows a findings section")
	}
}

// BenchmarkRender measures the local report of the v0.2 example set (3 plans, 2 findings,
// 1 comparison, 1 feedback entry).
// Run: go test ./internal/project -run '^$' -bench Render -benchmem
func BenchmarkRender(b *testing.B) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", "recommendation-findings.json"))
	if err != nil {
		b.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	im := &Imported{Result: hint.ImportResult{Document: "accepted"}, Doc: doc}
	cur := &Current{Names: map[string]string{}}
	var n int
	for i := 0; i < b.N; i++ {
		n = len(Render(im, cur, "x.json"))
	}
	b.ReportMetric(float64(n), "report_bytes")
}
