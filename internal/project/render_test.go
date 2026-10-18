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
	for _, want := range []string{"## Findings", "heuristic priority, not a calibrated ranking", "### Finding 1: repeated_navigation (supported)",
		"- stale_index: **supported** (stale_context_measurement, repeat_without_observed_edit)", "| large_read_bytes | bytes | unknown | 2 of 5 |",
		"Next step: **refresh_index_and_recheck** on billing/ledger.go", "- coordination_wait: insufficient_coverage", "### Finding 2: large_low_selectivity_read (limited)",
		"Intervention comparison (heuristic; cost model unavailable, so no net value or break-even): **COLLECT_MORE_EVIDENCE**.",
		"| CONTEXT_ONLY | known | -120.5 (-400 to 300; 6 of 6) | 0.1 (0 to 0.2; 6 of 6) | context_only, held_out, 6 objectives, 3 families |",
		"| REFACTOR_CODE | unknown | unknown | unknown | none |",
		"## Context policy feedback", "**keep** (expansion_is_normal, less_navigation_after_delivery, no_worse_outcome_with_delivery)",
		"  - navigation_after_delivery_per_task: 2.5 operations_per_task"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Index(md, "## Findings") > strings.Index(md, "## Plan 1") {
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
	if md := Render(im, cur, "x.json"); strings.Contains(md, "## Findings") {
		t.Error("a set without findings shows a findings section")
	}
}
