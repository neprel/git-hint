package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// exampleLedger reads the model ledger of a v0.2 example (observation 6).
func exampleLedger(t *testing.T, name string) ModelLedger {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "v0.2", name))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Payload struct {
			Observations []Observation `json:"observations"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return *d.Payload.Observations[6].ModelLedger
}

func clone(t *testing.T, m ModelLedger) ModelLedger {
	t.Helper()
	b, _ := json.Marshal(m)
	var c ModelLedger
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLedgerEntriesProblems(t *testing.T) {
	agg := exampleLedger(t, "activity-model-ledger.json")
	if p := LedgerProblems(agg); len(p) != 0 || agg.Entries != nil || agg.SealedMs != nil {
		t.Fatalf("the aggregate-only example changed or is refused: %v", p)
	}
	m := exampleLedger(t, "activity-model-ledger-entries.json")
	if p := LedgerProblems(m); len(p) != 0 {
		t.Fatalf("the entries example is refused: %v", p)
	}
	// the totals include partial values of cancelled and error entries
	if *m.Components["prompt"] != 3000+3091 || m.ComponentCoverage["prompt"] != 2 || m.Counts["cancelled"] != 1 || m.Counts["error"] != 1 {
		t.Fatalf("example totals: %+v %+v", m.Components, m.ComponentCoverage)
	}
	for name, change := range map[string]func(c *ModelLedger){
		"sum":           func(c *ModelLedger) { v := int64(3001); c.Entries[0].Components["prompt"] = &v },
		"partial error": func(c *ModelLedger) { v := int64(7); c.Entries[1].Components["completion"] = &v },
		"coverage":      func(c *ModelLedger) { c.Entries[2].Components["cached"] = nil },
		"ordinal":       func(c *ModelLedger) { c.Entries[1].Ordinal = 1 },
		"duplicate id":  func(c *ModelLedger) { c.Entries[1].EntryID = c.Entries[0].EntryID },
		"termination":   func(c *ModelLedger) { c.Entries[0].Termination = "context_lost" },
		"after seal":    func(c *ModelLedger) { c.Entries[3].AdmittedMs = *c.SealedMs + 1 },
		"clock step":    func(c *ModelLedger) { z := int64(0); c.ClockStepMs = &z },
		"no timing":     func(c *ModelLedger) { c.SealedMs = nil },
		"fewer":         func(c *ModelLedger) { c.Entries = c.Entries[:3] },
		"over limit":    func(c *ModelLedger) { c.Reasons = append(c.Reasons, "entries_over_limit") },
		"model check":   func(c *ModelLedger) { c.Entries[0].ModelCheck = "mismatch" },
	} {
		c := clone(t, m)
		change(&c)
		if len(LedgerProblems(c)) == 0 {
			t.Errorf("%s: an inconsistent entry ledger passed", name)
		}
	}
}

func TestLedgerEntriesLifecycle(t *testing.T) {
	m := exampleLedger(t, "activity-model-ledger-entries.json")
	next := func(change func(c *ModelLedger)) ModelLedger {
		c := clone(t, m)
		c.Revision, c.Counts["late"], c.Reasons = 1, 1, append(c.Reasons, "late_requests")
		change(&c)
		return c
	}
	if LedgerLifecycleProblem(m, next(func(*ModelLedger) {})) {
		t.Fatal("a revision with identical entries and one late request is refused")
	}
	for name, change := range map[string]func(c *ModelLedger){
		"entry changed": func(c *ModelLedger) { c.Entries[0].FinishedMs++ },
		"entries removed": func(c *ModelLedger) {
			c.Entries, c.AnchorUTC, c.SealedMs, c.FinalizedMs, c.ClockStepMs = nil, "", nil, nil, nil
		},
		"timing rewritten": func(c *ModelLedger) { c.AnchorUTC = "2026-10-11T10:54:59Z" },
	} {
		if !LedgerLifecycleProblem(m, next(change)) {
			t.Errorf("%s: a revision that rewrites the entries passed", name)
		}
	}
	agg := exampleLedger(t, "activity-model-ledger.json")
	enriched := clone(t, agg)
	enriched.Revision, enriched.Counts["late"], enriched.State, enriched.Reasons = 1, 1, "incomplete", []string{"late_requests"}
	enriched.Entries = m.Entries
	if !LedgerLifecycleProblem(agg, enriched) {
		t.Fatal("a revision that adds entries to an aggregate-only ledger passed")
	}
}
