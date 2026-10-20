package activity

import (
	"encoding/json"
	"slices"
	"time"
)

// Coverage is the model usage of one task on the coverage path (07B-E §30-§33), the same rule
// as the server's metrics.usage_coverage. Complete only when every session of the task's
// model calls has exactly one flushed closure attached at start with known zero drops,
// errors and open calls and no later record; every call is linked exactly (primary: one
// MODEL_CALL and one USAGE with the same call_ref; title and compaction: exactly one start
// and one terminal of the kind in the session; generate never); and one allowlisted
// normalization on sources with the coverage capabilities. Counters alone never complete.
type Coverage struct {
	State         string // complete, partial, unknown
	Reason        string
	Input, Output float64
	Cache         *float64 // read + write when every call reports both; nil otherwise
	Normalization string
	// Provider are the descriptive provider totals of a ledger that passed every gate (nil
	// otherwise); they never fill Input/Output and never make the usage complete
	Provider *ProviderTotals
}

// ProviderTotalsSemantics labels the provider-reported prompt and completion totals of the
// observed requests of an isolated run (07B-J §25 A): cache and reasoning semantics unknown,
// never task effort, never added to another channel.
const ProviderTotalsSemantics = "openai-compat-provider-totals-v1"

// ProviderTotals are the sums of a complete, final, enforced and bound ledger.
type ProviderTotals struct {
	Semantics, Scope string // ProviderTotalsSemantics, full_task (the observed request population)
	Input, Output    int64  // Σ prompt, Σ completion of the complete entries
}

var coverageCaps = []string{"usage", "request_kind", "collection"}

func knownZero(state string, v *float64, unit string) bool {
	return state == "known" && unit == "count" && v != nil && *v == 0
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// UsageCoverage evaluates the records of one task (all of its observations).
func UsageCoverage(obs []Observation, sources map[string]Source) Coverage {
	for _, o := range obs {
		if o.ModelLedger != nil {
			return ledgerCoverage(obs, sources)
		}
	}
	return pluginCoverage(obs, sources)
}

// ledgerCoverage is the usage of a task with model ledger records (the server's
// metrics._ledger_coverage without the work-attempt check, which needs the received reports):
// never complete while the ledger usage is descriptive, and the plugin's USAGE values never
// enter it. Fail closed in the same order as the server.
func ledgerCoverage(obs []Observation, sources map[string]Source) Coverage {
	no := func(r string) Coverage { return Coverage{State: "partial", Reason: r} }
	var ledgers, agents []Observation
	refs := map[string]bool{}
	for _, o := range obs {
		switch {
		case o.ModelLedger != nil:
			ledgers = append(ledgers, o)
			refs[o.ModelLedger.LedgerRef] = true
		case o.Source != "watcher":
			agents = append(agents, o)
		}
	}
	if len(refs) != 1 {
		return no("ledger_ambiguous")
	}
	byRev := map[int]Observation{}
	for _, o := range ledgers {
		if prev, ok := byRev[o.ModelLedger.Revision]; ok && !sameLedger(prev.ModelLedger, o.ModelLedger) {
			return no("ledger_conflict")
		}
		byRev[o.ModelLedger.Revision] = o
	}
	for r := range len(byRev) {
		if _, ok := byRev[r]; !ok {
			return no("ledger_revision_gap")
		}
	}
	first, top := byRev[0], byRev[len(byRev)-1]
	for _, o := range ledgers {
		if o.ModelLedger.Binding != first.ModelLedger.Binding || o.ModelLedger.SealedAt != first.ModelLedger.SealedAt {
			return no("ledger_binding_changed")
		}
	}
	for r := 1; r < len(byRev); r++ {
		if LedgerLifecycleProblem(*byRev[r-1].ModelLedger, *byRev[r].ModelLedger) {
			return no("ledger_lifecycle_violation") // a later revision never heals or rewrites the sealed account
		}
	}
	m, b := top.ModelLedger, top.ModelLedger.Binding
	src, ok := sources[top.SourceID]
	if len(LedgerProblems(*m)) > 0 || !ok || src.UsageNormalization != LedgerNormalization || top.Usage == nil || top.Usage.InputTokens.State == "known" ||
		top.Usage.OutputTokens.State == "known" || top.Correlation["run_ref"] != b.RunRef || top.Correlation["task_id"] != b.TaskID ||
		top.Correlation["task_family_id"] != b.TaskFamilyID {
		return no("ledger_inconsistent")
	}
	if src.Configuration == nil || src.Configuration.Digest() != b.ConfigurationDigest {
		return no("ledger_binding_mismatch")
	}
	if len(agents) == 0 {
		return no("ledger_without_behavior") // the ledger never stands in for missing behavioral events
	}
	for _, o := range agents {
		if o.Correlation["run_ref"] != b.RunRef {
			return no("ledger_other_run") // an agent record outside the bound run is never discarded
		}
		if o.Correlation["task_id"] != b.TaskID || o.Correlation["task_family_id"] != b.TaskFamilyID {
			return no("ledger_binding_mismatch")
		}
		if c := sources[o.SourceID].Configuration; c == nil || c.Digest() != b.ConfigurationDigest {
			return no("ledger_configuration_mismatch")
		}
	}
	if m.State != "complete" {
		return no("ledger_incomplete")
	}
	// the canonical usage stays unknown; the provider totals are descriptive (a complete ledger
	// without problems has every entry complete with known prompt and completion, enforced
	// egress, an isolated scope and no late, rejected or teardown-incomplete account)
	return Coverage{State: "unknown", Reason: "usage_normalization_unmapped",
		Provider: &ProviderTotals{Semantics: ProviderTotalsSemantics, Scope: "full_task", Input: *m.Components["prompt"], Output: *m.Components["completion"]}}
}

func sameLedger(a, b *ModelLedger) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// pluginCoverage is the usage of a task on the plugin coverage path.
func pluginCoverage(obs []Observation, sources map[string]Source) Coverage {
	type scope struct{ src, ses string }
	sc := func(o Observation) scope { return scope{o.SourceID, o.Correlation["session_id"]} }
	// the relevant population is every agent record of the task (watcher-only records
	// excluded): each of its sessions needs a complete closure, and an untyped USAGE anywhere in
	// it blocks complete usage (C7BE-19)
	var calls, relevant []Observation
	untyped := false
	for _, o := range obs {
		if o.Source == "watcher" {
			continue
		}
		relevant = append(relevant, o)
		if o.Op == "MODEL_CALL" || o.Op == "USAGE" && o.RequestKind != "" {
			calls = append(calls, o)
		} else if o.Op == "USAGE" {
			untyped = true
		}
	}
	if len(calls) == 0 {
		if untyped {
			return Coverage{State: "unknown", Reason: "no_usage_coverage"}
		}
		return Coverage{State: "unknown", Reason: "no_model_calls"}
	}
	if untyped {
		return Coverage{State: "partial", Reason: "untyped_usage"}
	}
	norm := ""
	for _, o := range calls {
		src, ok := sources[o.SourceID]
		if !ok || src.UsageNormalization != UsageNormalization {
			return Coverage{State: "unknown", Reason: "normalization_unknown"}
		}
		norm = src.UsageNormalization
	}
	sessions := map[scope]bool{}
	for _, o := range relevant {
		if o.Correlation["session_id"] == "" {
			return Coverage{State: "partial", Reason: "session_unknown"}
		}
		sessions[sc(o)] = true
	}
	for s := range sessions {
		src := sources[s.src]
		for _, c := range coverageCaps {
			if !slices.Contains(src.Capabilities, c) {
				return Coverage{State: "unknown", Reason: "capabilities_missing"}
			}
		}
	}
	for s := range sessions {
		var closures []Observation
		for _, o := range obs {
			if o.Op == "SESSION" && o.Collection != nil && sc(o) == s {
				closures = append(closures, o)
			}
		}
		if len(closures) != 1 {
			if len(closures) == 0 {
				return Coverage{State: "partial", Reason: "closure_missing"}
			}
			return Coverage{State: "partial", Reason: "closure_conflict"}
		}
		c := closures[0].Collection
		if c.Lifecycle != "attached_at_start" {
			return Coverage{State: "partial", Reason: "attached_mid_session"}
		}
		if c.Closure != "flushed" || c.ClosedAt == nil {
			return Coverage{State: "partial", Reason: "closure_not_flushed"}
		}
		if !knownZero(c.EventsDropped.State, c.EventsDropped.Value, c.EventsDropped.Unit) || !knownZero(c.Errors.State, c.Errors.Value, c.Errors.Unit) ||
			!knownZero(c.OpenCalls.State, c.OpenCalls.Value, c.OpenCalls.Unit) {
			return Coverage{State: "partial", Reason: "collection_loss"}
		}
		closed, _ := parseTS(*c.ClosedAt)
		for _, o := range obs {
			if sc(o) != s || o.ObservationID == closures[0].ObservationID || o.Time.Start == nil {
				continue
			}
			if at, ok := parseTS(*o.Time.Start); ok && at.After(closed) {
				return Coverage{State: "partial", Reason: "stale_after_closure"}
			}
		}
	}
	var linked []Observation
	type key struct {
		s   scope
		ref string
	}
	starts, ends := map[key]int{}, map[key]int{}
	for _, o := range calls {
		switch {
		case o.RequestKind == "generate":
			return Coverage{State: "partial", Reason: "generate_without_terminal"}
		case o.RequestKind == "primary":
			r := o.Correlation["call_ref"]
			if r == "" {
				return Coverage{State: "partial", Reason: "primary_without_call_ref"}
			}
			if o.Op == "MODEL_CALL" {
				starts[key{sc(o), r}]++
			} else {
				ends[key{sc(o), r}]++
				linked = append(linked, o)
			}
		}
	}
	for k, n := range starts {
		if n != 1 || ends[k] > 1 {
			return Coverage{State: "partial", Reason: "duplicate_call"}
		}
	}
	for k, n := range ends {
		if n != 1 {
			return Coverage{State: "partial", Reason: "duplicate_call"}
		}
		if starts[k] != 1 {
			return Coverage{State: "partial", Reason: "unlinked_call"}
		}
	}
	if len(starts) != len(ends) {
		return Coverage{State: "partial", Reason: "unlinked_call"}
	}
	for _, kind := range []string{"title", "compaction"} {
		for s := range sessions {
			var ks, ke []Observation
			for _, o := range calls {
				if o.RequestKind == kind && sc(o) == s {
					if o.Op == "MODEL_CALL" {
						ks = append(ks, o)
					} else {
						ke = append(ke, o)
					}
				}
			}
			if len(ks) == 1 && len(ke) == 1 && ke[0].Correlation["call_ref"] == "" {
				linked = append(linked, ke[0])
			} else if len(ks)+len(ke) > 0 {
				return Coverage{State: "partial", Reason: kind + "_ambiguous"}
			}
		}
	}
	res := Coverage{State: "complete", Normalization: norm}
	cache, cacheKnown := 0.0, true
	for _, o := range linked {
		u := o.Usage
		if u == nil || u.InputTokens.State != "known" || u.OutputTokens.State != "known" || u.InputTokens.Value == nil || u.OutputTokens.Value == nil {
			return Coverage{State: "partial", Reason: "usage_unknown"}
		}
		res.Input += *u.InputTokens.Value
		res.Output += *u.OutputTokens.Value
		if u.CacheReadTokens == nil || u.CacheWriteTokens == nil || u.CacheReadTokens.State != "known" || u.CacheWriteTokens.State != "known" {
			cacheKnown = false
		} else {
			cache += *u.CacheReadTokens.Value + *u.CacheWriteTokens.Value
		}
	}
	if cacheKnown {
		res.Cache = &cache
	}
	return res
}
