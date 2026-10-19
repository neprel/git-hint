package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// The shared usage-selection vectors (tests/conformance/vectors/ledger-selection.json, also
// checked by the server): the client gives the declared reason for every case, and a task with
// a ledger never has complete usage.
func TestLedgerSelectionVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "conformance", "vectors", "ledger-selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors []struct {
			Name         string        `json:"name"`
			Reason       string        `json:"reason"`
			Sources      []Source      `json:"sources"`
			Observations []Observation `json:"observations"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Vectors) < 13 {
		t.Fatalf("%d vectors", len(f.Vectors))
	}
	for _, v := range f.Vectors {
		srcs := map[string]Source{}
		for _, s := range v.Sources {
			srcs[s.SourceID] = s
		}
		c := UsageCoverage(v.Observations, srcs)
		if c.Reason != v.Reason || c.State == "complete" || c.Input != 0 || c.Output != 0 {
			t.Errorf("%s: %+v, want reason %s", v.Name, c, v.Reason)
		}
	}
}

// C7BE-30: a later revision can only add late or rejected traffic and reasons; every sealed
// value stays, and a known incompleteness never heals.
func TestLedgerLifecycle(t *testing.T) {
	late := func(m ModelLedger, n int) ModelLedger {
		m.Counts = map[string]int{}
		for k, v := range goodLedger().Counts {
			m.Counts[k] = v
		}
		m.Counts["late"], m.State, m.Reasons = n, "incomplete", []string{"late_requests"}
		return m
	}
	r0 := goodLedger()
	r1 := late(goodLedger(), 1)
	r1.Revision = 1
	if LedgerLifecycleProblem(r0, r1) {
		t.Fatal("a late request after a complete seal must be a valid next revision")
	}
	for name, next := range map[string]func() ModelLedger{
		"heal": func() ModelLedger { n := goodLedger(); n.Revision = 2; return n },
		"rewrite prompt": func() ModelLedger {
			n := late(goodLedger(), 2)
			n.Revision = 2
			v := int64(100010)
			n.Components = map[string]*int64{"prompt": &v, "completion": goodLedger().Components["completion"], "cached": nil, "reasoning": nil}
			return n
		},
		"lower late": func() ModelLedger { n := late(goodLedger(), 0); n.Revision = 2; return n },
		"drop reason": func() ModelLedger {
			n := late(goodLedger(), 2)
			n.Revision, n.Reasons = 2, []string{"rejected_limit"}
			return n
		},
		"new seal time": func() ModelLedger {
			n := late(goodLedger(), 2)
			n.Revision, n.SealedAt = 2, "2026-10-19T11:00:00.000Z"
			return n
		},
		"admitted": func() ModelLedger {
			n := late(goodLedger(), 2)
			n.Revision = 2
			n.Counts["admitted"], n.Counts["error"] = 3, 1
			return n
		},
		"model check":   func() ModelLedger { n := late(goodLedger(), 2); n.Revision, n.ModelCheck = 2, "unverified"; return n },
		"skip revision": func() ModelLedger { n := late(goodLedger(), 2); n.Revision = 3; return n },
	} {
		if !LedgerLifecycleProblem(r1, next()) {
			t.Errorf("%s: accepted", name)
		}
	}
}
