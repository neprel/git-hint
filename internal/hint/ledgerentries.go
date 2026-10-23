package hint

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"time"
)

var opaqueID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// LedgerEntryBytes bounds the serialized entries of one ledger (draft v0.2). A producer that
// would exceed it, or 1000 entries, writes no entries and an incomplete ledger with the reason
// entries_over_limit; a list is never truncated.
const LedgerEntryBytes = 512 << 10

// LedgerTiming are the timing fields that exist exactly when a ledger has entries.
var LedgerTiming = []string{"anchor_utc", "sealed_ms", "finalized_ms", "clock_step_ms"}

// ledgerTerminations maps each termination (how the proxy handler ended, independent of usage)
// to the terminal states it allows; the table is total over both enums.
var ledgerTerminations = map[string][]string{
	"response_ended": {"complete", "usage_partial", "missing_usage", "usage_conflict", "unreadable", "truncated"},
	"error_status":   {"error"},
	"proxy_error":    {"error"},
	"context_lost":   {"cancelled"},
	"no_response":    {"cancelled"},
	"handler_abort":  {"cancelled"},
	"seal_timeout":   {"unknown"},
}

// LedgerEntriesProblem checks the per-entry evidence of one decoded model ledger (JSON numbers
// as float64) against itself and the ledger's aggregates. It returns the first violated rule,
// or "" when the ledger has no entries field and no timing fields (an aggregate-only ledger),
// or when everything agrees. The rules: timing fields exist exactly with entries; entries and
// the reason entries_over_limit exclude each other; at most 1000 entries and LedgerEntryBytes
// serialized; ordinals 1..n in order; unique opaque entry IDs; termination and state per the
// total table; response, status class and stream facts consistent with them; counts per state,
// component sums and coverage, and the ledger's model check derived exactly from the entries;
// admitted times non-decreasing and not after the seal, every finish between its admission and
// the end of the drain, a seal timeout finished at the end of the drain; and clock_step_ms the
// observed difference (sealed_at - anchor_utc, whole milliseconds) minus sealed_ms.
func LedgerEntriesProblem(l map[string]any) string {
	raw, has := l["entries"]
	for _, k := range LedgerTiming {
		if _, ok := l[k]; ok != has {
			return "timing fields without entries or entries without timing fields"
		}
	}
	if !has {
		return ""
	}
	if slices.Contains(list(l["reasons"]), any("entries_over_limit")) {
		return "entries with the reason entries_over_limit"
	}
	entries, ok := raw.([]any)
	if !ok {
		return "entries is not a list"
	}
	if b, _ := json.Marshal(entries); len(entries) > 1000 || len(b) > LedgerEntryBytes {
		return "entries over the bound"
	}
	nat := func(v any) (int64, bool) {
		f, ok := v.(float64)
		return int64(f), ok && f >= 0 && f <= 1<<53-1 && f == math.Trunc(f)
	}
	sealed, ok1 := nat(l["sealed_ms"])
	final, ok2 := nat(l["finalized_ms"])
	step, ok3 := l["clock_step_ms"].(float64)
	if !ok1 || !ok2 || !ok3 || step != math.Trunc(step) || sealed > final {
		return "seal times invalid"
	}
	a, e1 := time.Parse(time.RFC3339Nano, str(l["anchor_utc"]))
	s, e2 := time.Parse(time.RFC3339Nano, str(l["sealed_at"]))
	if e1 != nil || e2 != nil || int64(step) != s.Sub(a).Milliseconds()-sealed {
		return "clock_step_ms is not the observed minus the derived seal time"
	}
	counts := map[string]int{}
	sums, cover := map[string]int64{}, map[string]int{}
	ids := map[string]bool{}
	checks := map[string]bool{}
	var last int64
	for i, x := range entries {
		e, ok := x.(map[string]any)
		if !ok {
			return "entry is not an object"
		}
		ord, okO := nat(e["ordinal"])
		id := str(e["entry_id"])
		if !okO || ord != int64(i+1) || !opaqueID.MatchString(id) || ids[id] {
			return "ordinal or entry ID invalid"
		}
		ids[id] = true
		state, term := str(e["state"]), str(e["termination"])
		if !slices.Contains(ledgerTerminations[term], state) {
			return "termination and state disagree"
		}
		started, okS := e["response_started"].(bool)
		class := str(e["status_class"])
		done, isBool := e["stream_done"].(bool)
		if !okS || (e["stream_done"] != nil && !isBool) || !slices.Contains([]string{"2xx", "3xx", "4xx", "5xx", "none"}, class) ||
			started != (class != "none") ||
			(term == "no_response" && started) ||
			(slices.Contains([]string{"response_ended", "error_status", "proxy_error"}, term) && !started) ||
			(term == "response_ended" && class != "2xx") ||
			(term == "error_status" && (class == "2xx" || class == "none")) ||
			(term == "proxy_error" && class != "5xx") ||
			(state == "truncated" && !(isBool && !done)) ||
			(term == "response_ended" && isBool && !done && !slices.Contains([]string{"truncated", "unreadable", "usage_conflict"}, state)) {
			return "response facts disagree with the termination"
		}
		mc := str(e["model_check"])
		if !slices.Contains([]string{"requested_model_observed", "mismatch", "unverified"}, mc) {
			return "entry model check invalid"
		}
		checks[mc] = true
		adm, okA := nat(e["admitted_ms"])
		fin, okF := nat(e["finished_ms"])
		if !okA || !okF || adm < last || adm > sealed || fin < adm || fin > final || (term == "seal_timeout" && fin != final) {
			return "entry times out of order"
		}
		last = adm
		comps, ok := e["components"].(map[string]any)
		if !ok || len(comps) != 4 {
			return "entry components invalid"
		}
		for _, k := range []string{"prompt", "completion", "cached", "reasoning"} {
			v, present := comps[k]
			if !present {
				return "entry components invalid"
			}
			if v == nil {
				continue
			}
			n, okN := nat(v)
			if !okN {
				return "entry components invalid"
			}
			sums[k] += n
			cover[k]++
		}
		_, p := comps["prompt"].(float64)
		_, c := comps["completion"].(float64)
		if (state == "complete" && !(p && c)) || (state == "usage_partial" && p && c) ||
			(slices.Contains([]string{"missing_usage", "usage_conflict"}, state) && (p || c || comps["cached"] != nil || comps["reasoning"] != nil)) {
			return "entry usage disagrees with its state"
		}
		counts[state]++
	}
	lc, _ := l["counts"].(map[string]any)
	if n, _ := nat(lc["admitted"]); n != int64(len(entries)) {
		return "entries are not the admitted requests"
	}
	for _, k := range ledgerEntryStates {
		if n, _ := nat(lc[k]); n != int64(counts[k]) {
			return "state counts are not the entries' states"
		}
	}
	lcomp, _ := l["components"].(map[string]any)
	lcov, _ := l["component_coverage"].(map[string]any)
	for _, k := range []string{"prompt", "completion", "cached", "reasoning"} {
		cv, _ := nat(lcov[k])
		if cv != int64(cover[k]) {
			return "coverage is not the entries' coverage"
		}
		if cover[k] > 0 {
			if v, okv := nat(lcomp[k]); !okv || v != sums[k] {
				return "components are not the entries' sums"
			}
		}
	}
	want := "not_observed"
	switch {
	case checks["mismatch"]:
		want = "mismatch"
	case checks["unverified"]:
		want = "unverified"
	case checks["requested_model_observed"]:
		want = "requested_model_observed"
	}
	if l["model_check"] != want {
		return "model check is not derived from the entries"
	}
	return ""
}
