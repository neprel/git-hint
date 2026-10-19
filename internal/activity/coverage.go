package activity

import (
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
