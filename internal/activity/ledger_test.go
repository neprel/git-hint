package activity

import (
	"encoding/json"
	"strings"
	"testing"
)

// The configuration digest of a ledger binding is the JCS digest of the canonical configuration
// without its declared part. The same vector is checked by the server (test_ledger_channel.py).
const ledgerConfigJSON = `{"agent":{"name":"opencode","version":{"state":"known","value":"2.0.26"}},` +
	`"model":{"state":"known","ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provenance":"launch_parameter"},` +
	`"harness":{"kind":"harbor","version":{"state":"known","value":"0.23.0"}},"context_policy":{"state":"disabled"},` +
	`"config_refs":{"state":"known","refs":["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]},` +
	`"budget":{"max_requests":{"state":"known","value":16},"max_seconds":{"state":"known","value":300}},` +
	`"declared":{"model":{"state":"known","ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`

const ledgerConfigDigest = "sha256:9c331599944513fb4cc0cf063cddeca09790e404348742e1a1340a09428f68b4"

func TestLedgerConfigurationDigestVector(t *testing.T) {
	var c Configuration
	if err := json.Unmarshal([]byte(ledgerConfigJSON), &c); err != nil {
		t.Fatal(err)
	}
	if d := c.Digest(); d != ledgerConfigDigest {
		t.Fatalf("digest %s", d)
	}
}

func goodLedger() ModelLedger {
	v := func(x int64) *int64 { return &x }
	h := strings.Repeat("c", 32)
	return ModelLedger{LedgerRef: h, State: "complete", Reasons: []string{},
		Counts:            map[string]int{"admitted": 2, "late": 0, "rejected_limit": 0, "complete": 2, "usage_partial": 0, "missing_usage": 0, "usage_conflict": 0, "unreadable": 0, "truncated": 0, "error": 0, "cancelled": 0, "unknown": 0},
		Components:        map[string]*int64{"prompt": v(10), "completion": v(3), "cached": nil, "reasoning": nil},
		ComponentCoverage: map[string]int{"prompt": 2, "completion": 2, "cached": 0, "reasoning": 0},
		ModelCheck:        "requested_model_observed", Bypass: "enforced", Scope: "isolated_container", SealedAt: "2026-10-19T10:00:00.000Z",
		Binding: LedgerBinding{RunRef: h, TaskID: h, TaskFamilyID: h, ConfigurationDigest: "sha256:" + strings.Repeat("d", 64), UnitRef: h, AttemptRef: h}}
}

// The consistency check fails closed: missing or negative counts, an admitted count that is not
// the sum of the terminal states, coverage and components that disagree, and a complete state
// without complete evidence (also with no request) are problems.
func TestLedgerProblems(t *testing.T) {
	if p := LedgerProblems(goodLedger()); len(p) > 0 {
		t.Fatalf("good ledger: %v", p)
	}
	for name, mut := range map[string]func(*ModelLedger){
		"missing count":         func(m *ModelLedger) { delete(m.Counts, "unknown") },
		"negative count":        func(m *ModelLedger) { m.Counts["late"] = -1 },
		"admitted mismatch":     func(m *ModelLedger) { m.Counts["admitted"] = 3 },
		"extra count":           func(m *ModelLedger) { m.Counts["other"] = 0 },
		"coverage over":         func(m *ModelLedger) { m.ComponentCoverage["prompt"] = 3 },
		"component without cov": func(m *ModelLedger) { x := int64(1); m.Components["cached"] = &x },
		"partial completion":    func(m *ModelLedger) { m.ComponentCoverage["completion"] = 1 },
		"late but complete":     func(m *ModelLedger) { m.Counts["late"] = 1 },
		"model not observed":    func(m *ModelLedger) { m.ModelCheck = "unverified" },
		"restricted scope":      func(m *ModelLedger) { m.Scope = "restricted" },
		"watched egress":        func(m *ModelLedger) { m.Bypass = "observed_no_bypass" },
		"no requests": func(m *ModelLedger) {
			m.Counts["admitted"], m.Counts["complete"] = 0, 0
			m.ComponentCoverage["prompt"], m.ComponentCoverage["completion"] = 0, 0
			m.Components["prompt"], m.Components["completion"] = nil, nil
		},
		"incomplete no reason": func(m *ModelLedger) { m.State = "incomplete" },
		"raw reference":        func(m *ModelLedger) { m.Binding.RunRef = "trial-1" },
	} {
		m := goodLedger()
		mut(&m)
		if p := LedgerProblems(m); len(p) == 0 {
			t.Errorf("%s: no problem", name)
		}
	}
}
