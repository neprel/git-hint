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

// ledgerAgentEnd says which outcomes have an enclosure end (when the ledger has entries); every
// outcome except refused_before_launch has an enclosure start then.
var ledgerAgentEnd = map[string]bool{
	"exited": true, "exited_nonzero": true, "stopped_after_timeout": true, "stopped_after_cancel": true, "stopped_after_wait_error": true,
	"stop_unconfirmed": false, "start_unconfirmed": false, "refused_before_launch": false,
}

// LedgerAgentRunProblem checks the optional launch/exit enclosure of a decoded model ledger
// (draft v0.2) against the outcome table and the entries: exit_code exactly for exited (0) and
// exited_nonzero (not 0); offsets only when the ledger has entries, then a start for every
// outcome except refused_before_launch and an end only for the outcomes that completed a wait or
// a confirmed stop; refused_before_launch with no admitted request; every admission inside the
// enclosure on the shared monotonic offsets (no clock slack), the end not after the seal. It
// returns the first violated rule, or "" when agent_run is absent (lifecycle unknown) or agrees.
func LedgerAgentRunProblem(l map[string]any) string {
	raw, has := l["agent_run"]
	if !has {
		return ""
	}
	a, ok := raw.(map[string]any)
	if !ok {
		return "agent_run is not an object"
	}
	outcome := str(a["outcome"])
	hasEnd, known := ledgerAgentEnd[outcome]
	if !known {
		return "unknown agent outcome"
	}
	code, isCode := a["exit_code"].(float64)
	switch {
	case outcome == "exited" && !(isCode && code == 0),
		outcome == "exited_nonzero" && !(isCode && code != 0 && code == math.Trunc(code)),
		outcome != "exited" && outcome != "exited_nonzero" && a["exit_code"] != nil:
		return "exit code disagrees with the outcome"
	}
	lc, _ := l["counts"].(map[string]any)
	if admitted, _ := lc["admitted"].(float64); outcome == "refused_before_launch" && admitted != 0 {
		return "requests admitted although the agent was refused before launch"
	}
	nat := func(v any) (int64, bool) {
		f, ok := v.(float64)
		return int64(f), ok && f >= 0 && f <= 1<<53-1 && f == math.Trunc(f)
	}
	start, okS := nat(a["enclosure_start_ms"])
	end, okE := nat(a["enclosure_end_ms"])
	entries, withEntries := l["entries"].([]any)
	if !withEntries {
		if a["enclosure_start_ms"] != nil || a["enclosure_end_ms"] != nil {
			return "enclosure offsets without entries"
		}
		return ""
	}
	wantStart := outcome != "refused_before_launch"
	if okS != wantStart || (a["enclosure_start_ms"] != nil && !okS) || okE != hasEnd || (a["enclosure_end_ms"] != nil && !okE) {
		return "enclosure offsets disagree with the outcome"
	}
	if sealed, _ := nat(l["sealed_ms"]); okE && (end < start || end > sealed) {
		return "enclosure end before its start or after the seal"
	}
	for _, x := range entries {
		e, _ := x.(map[string]any)
		adm, _ := nat(e["admitted_ms"])
		if adm < start || (okE && adm > end) {
			return "an admission outside the launch/exit enclosure"
		}
	}
	return ""
}

// LedgerCaptureWidthMs is the widest claimed bracket of one backend capture (its 60 s bound).
const LedgerCaptureWidthMs = 60000

// LedgerBackendCaptureProblem checks the optional backend captures of a decoded model ledger
// (draft v0.2) against the status table and the ledger's own monotonic offsets: complete has a
// snapshot and, with entries, send and receive offsets; timeout and error have no snapshot and,
// with entries, a send offset only; not_attempted has nothing; without entries no offsets at
// all. send_ms <= recv_ms (equal is valid: floored readings, no zero round trip implied), at most
// LedgerCaptureWidthMs apart; the start capture not after the enclosure start or any admission,
// the end capture not before the drain end (finalized_ms). The rules check the consistency of the
// claimed observations on one clock; they do not prove that the remote inspect happened. It
// returns the first violated rule, or "" when backend_capture is absent (window unknown) or agrees.
func LedgerBackendCaptureProblem(l map[string]any) string {
	raw, has := l["backend_capture"]
	if !has {
		return ""
	}
	bc, ok := raw.(map[string]any)
	if !ok {
		return "backend_capture is not an object"
	}
	nat := func(v any) (int64, bool) {
		f, ok := v.(float64)
		return int64(f), ok && f >= 0 && f <= 1<<53-1 && f == math.Trunc(f)
	}
	entries, withEntries := l["entries"].([]any)
	type bracket struct {
		send, recv       int64
		hasSend, hasRecv bool
	}
	side := func(name string) (bracket, string) {
		c, ok := bc[name].(map[string]any)
		if !ok {
			return bracket{}, name + " capture is not an object"
		}
		var b bracket
		b.send, b.hasSend = nat(c["send_ms"])
		b.recv, b.hasRecv = nat(c["recv_ms"])
		if (c["send_ms"] != nil && !b.hasSend) || (c["recv_ms"] != nil && !b.hasRecv) {
			return b, name + " capture offset is not a non-negative integer"
		}
		status, snap := str(c["status"]), c["snapshot"] != nil
		var wantSend, wantRecv, wantSnap bool
		switch status {
		case "complete":
			wantSend, wantRecv, wantSnap = true, true, true
		case "timeout", "error":
			wantSend = true
		case "not_attempted":
		default:
			return b, "unknown " + name + " capture status"
		}
		if !withEntries {
			wantSend, wantRecv = false, false
		}
		if b.hasSend != wantSend || b.hasRecv != wantRecv || snap != wantSnap {
			return b, name + " capture disagrees with its status"
		}
		if b.hasRecv && (b.recv < b.send || b.recv-b.send > LedgerCaptureWidthMs) {
			return b, name + " capture bracket reversed or wider than its bound"
		}
		return b, ""
	}
	start, p := side("start")
	if p != "" {
		return p
	}
	end, p := side("end")
	if p != "" {
		return p
	}
	if !withEntries {
		return ""
	}
	last := start.send // the latest claimed reading of the start capture
	if start.hasRecv {
		last = start.recv
	}
	if start.hasSend {
		if a, _ := l["agent_run"].(map[string]any); a != nil {
			if es, ok := nat(a["enclosure_start_ms"]); ok && last > es {
				return "start capture after the launch enclosure start"
			}
		}
		for _, x := range entries {
			e, _ := x.(map[string]any)
			if adm, _ := nat(e["admitted_ms"]); last > adm {
				return "start capture after an admission"
			}
		}
	}
	if final, _ := nat(l["finalized_ms"]); end.hasSend && end.send < final {
		return "end capture before the drain end"
	}
	return ""
}
