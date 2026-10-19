package activity

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

// LedgerNormalization labels the provider usage of a model ledger (draft v0.2): the
// OpenAI-compatible usage components as the provider reported them. It is descriptive: it maps
// nothing to canonical model tokens, so the canonical usage of a ledger record stays unknown.
const LedgerNormalization = "openai-compat-usage-v1"

// LedgerEntryStates are the terminal states of one admitted model request; their counts add up
// to the admitted count.
var LedgerEntryStates = []string{"complete", "usage_partial", "missing_usage", "usage_conflict", "unreadable", "truncated", "error", "cancelled", "unknown"}

// LedgerCounts are every count of a ledger record (all required, never omitted).
var LedgerCounts = append([]string{"admitted", "late", "rejected_limit"}, LedgerEntryStates...)

// LedgerComponents are the provider usage components of a ledger record.
var LedgerComponents = []string{"prompt", "completion", "cached", "reasoning"}

// LedgerBinding ties a ledger to exactly one agent run and its declared task (draft v0.2). All
// references use the project-scoped eval-report domains: run_ref is the trial reference of the
// run, unit_ref and attempt_ref the work-unit references, configuration_digest the digest of the
// run's canonical configuration without its declared part (Configuration.Digest).
type LedgerBinding struct {
	RunRef              string `json:"run_ref"`
	TaskID              string `json:"task_id"`
	TaskFamilyID        string `json:"task_family_id"`
	ConfigurationDigest string `json:"configuration_digest"`
	UnitRef             string `json:"unit_ref"`
	AttemptRef          string `json:"attempt_ref"`
}

// ModelLedger is the sealed account of one run's model traffic at one revision (draft v0.2). It
// certifies the bounded model-traffic scope only; it says nothing about tool events.
type ModelLedger struct {
	LedgerRef         string            `json:"ledger_ref"`
	Revision          int               `json:"revision"`
	State             string            `json:"state"` // complete | incomplete
	Reasons           []string          `json:"reasons"`
	Counts            map[string]int    `json:"counts"`
	Components        map[string]*int64 `json:"components"`         // provider sums; null when no entry reported one
	ComponentCoverage map[string]int    `json:"component_coverage"` // entries that reported each component
	ModelCheck        string            `json:"model_check"`        // requested_model_observed | mismatch | unverified | not_observed
	Bypass            string            `json:"bypass"`             // enforced | observed_no_bypass | unverified
	Scope             string            `json:"scope"`              // isolated_container | restricted
	SealedAt          string            `json:"sealed_at"`
	Binding           LedgerBinding     `json:"binding"`
}

// RunRef is the opaque reference of one agent run: the eval-report trial reference of the run.
// The runner derives it once and passes it to the agent (HINT_RUN_REF).
func RunRef(key []byte, trialName string) string { return hint.EvalRef(key, "trial", trialName) }

// LedgerRef is the reference of the ledger of one run.
func LedgerRef(key []byte, runRef string) string { return hint.DeriveID(key, "ledger", runRef) }

var opaque = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidRunRef says whether a value is a usable opaque run reference (32 lowercase hex).
func ValidRunRef(v string) bool { return opaque.MatchString(v) }

// LedgerSource is the source of the ledger records of runs with one canonical configuration.
func LedgerSource(key []byte, agent string, cfg Configuration) Source {
	return Source{SourceID: hint.DeriveID(key, "source", "hint-model-ledger", Version, cfg.Digest()), Agent: agent,
		Adapter:       Adapter{Name: "hint-model-ledger", Version: Version},
		Capabilities:  []string{"time.start", "time.end", "correlation.task_id", "correlation.task_family_id", "correlation.run_ref", "usage", "model_ledger"},
		Unavailable:   []string{"correlation.session_id", "correlation.turn_id", "correlation.tool_call_id", "targets"},
		Configuration: &cfg, UsageNormalization: LedgerNormalization}
}

// LedgerObservation is the USAGE record of one ledger revision. Its canonical usage is unknown:
// the provider components stay in model_ledger, labeled by the source's normalization.
func LedgerObservation(key []byte, src Source, m ModelLedger) Observation {
	unknown := scan.Quantity{Unit: "tokens", State: "unknown", Reason: "not_supported"}
	id := hint.DeriveID(key, "ledger-usage", m.LedgerRef, strconv.Itoa(m.Revision))
	t := m.SealedAt
	return Observation{ObservationID: id, Class: "observation", Op: "USAGE", Source: "transport", SourceID: src.SourceID, Precision: "exact",
		Time:        Time{Start: &t, End: &t, Clock: "collector"},
		Correlation: map[string]string{"task_id": m.Binding.TaskID, "task_family_id": m.Binding.TaskFamilyID, "run_ref": m.Binding.RunRef},
		Outcome:     Outcome{Status: "success"}, Usage: &Usage{InputTokens: unknown, OutputTokens: unknown}, ModelLedger: &m,
		Missing: []string{"correlation.session_id", "correlation.turn_id"},
		Provenance: Provenance{CapturedAt: t, SourceEventID: hint.DeriveID(key, "ledger-event", m.LedgerRef, strconv.Itoa(m.Revision)),
			Transformations: []Transform{{Step: "normalize", Tool: "hint-cli", Version: Version}}}}
}

// LedgerProblems checks a ledger record fail-closed: every count present and non-negative, the
// admitted count equal to the sum of the terminal states, coverage within the admitted count,
// components null exactly when nothing covered them, and a complete state only with complete
// evidence. It returns the violated rules; empty means consistent.
func LedgerProblems(m ModelLedger) []string {
	var p []string
	bad := func(cond bool, f string, a ...any) {
		if cond {
			p = append(p, fmt.Sprintf(f, a...))
		}
	}
	sum := 0
	for _, k := range LedgerCounts {
		v, ok := m.Counts[k]
		bad(!ok || v < 0, "count %s missing or negative", k)
		if slices.Contains(LedgerEntryStates, k) {
			sum += v
		}
	}
	bad(len(m.Counts) != len(LedgerCounts), "unknown count")
	bad(m.Counts["admitted"] != sum, "admitted is not the sum of the terminal states")
	for _, k := range LedgerComponents {
		c, ok := m.ComponentCoverage[k]
		bad(!ok || c < 0 || c > m.Counts["admitted"], "coverage %s out of range", k)
		v := m.Components[k]
		bad((v == nil) != (c == 0) || (v != nil && *v < 0), "component %s inconsistent with its coverage", k)
	}
	complete := m.Counts["admitted"] > 0 && m.Counts["complete"] == m.Counts["admitted"] && m.ComponentCoverage["prompt"] == m.Counts["admitted"] &&
		m.ComponentCoverage["completion"] == m.Counts["admitted"] && m.ModelCheck == "requested_model_observed" && m.Scope == "isolated_container" &&
		m.Bypass == "enforced" && m.Counts["late"] == 0 && m.Counts["rejected_limit"] == 0
	bad(m.State == "complete" && (!complete || len(m.Reasons) > 0), "complete state without complete evidence")
	bad(m.State == "incomplete" && len(m.Reasons) == 0, "incomplete state without reasons")
	bad(m.State != "complete" && m.State != "incomplete", "unknown state")
	bad(m.Revision < 0, "negative revision")
	b := m.Binding
	for _, v := range []string{m.LedgerRef, b.RunRef, b.TaskID, b.TaskFamilyID, b.UnitRef, b.AttemptRef} {
		bad(!opaque.MatchString(v), "reference %q is not opaque", v)
	}
	return p
}

// LedgerLifecycleProblem says whether revision next cannot follow revision prev of the same
// ledger (C7BE-30): the sealed evidence is frozen (admitted and terminal counts, components and
// their coverage, the requested-model check, scope, egress, binding and seal time); the late and
// rejected counts never decrease; a known incompleteness and every earlier reason stay. A later
// revision can only add late traffic, never heal or rewrite the account.
func LedgerLifecycleProblem(prev, next ModelLedger) bool {
	if next.Revision != prev.Revision+1 || next.LedgerRef != prev.LedgerRef || next.Binding != prev.Binding || next.SealedAt != prev.SealedAt ||
		next.ModelCheck != prev.ModelCheck || next.Scope != prev.Scope || next.Bypass != prev.Bypass {
		return true
	}
	for _, k := range append([]string{"admitted"}, LedgerEntryStates...) {
		if next.Counts[k] != prev.Counts[k] {
			return true
		}
	}
	for _, k := range []string{"late", "rejected_limit"} {
		if next.Counts[k] < prev.Counts[k] {
			return true
		}
	}
	for _, k := range LedgerComponents {
		a, b := prev.Components[k], next.Components[k]
		if next.ComponentCoverage[k] != prev.ComponentCoverage[k] || (a == nil) != (b == nil) || (a != nil && *a != *b) {
			return true
		}
	}
	if prev.State == "incomplete" && next.State != "incomplete" {
		return true
	}
	for _, r := range prev.Reasons {
		if !slices.Contains(next.Reasons, r) {
			return true
		}
	}
	return false
}
