package main

// Draft v0.2 model calls (C7B-E): the plugin source reports usage under a supported usage
// normalization; one primary call has an exact call identity (MODEL_CALL + terminal USAGE
// with the same call_ref and request_kind); one title call has none (a singleton of its
// kind in the session); the closing SESSION record carries the collector closure.

var (
	callPrimary = id("model-call", "session", "msg-1")
	callSession = id("session", "model-calls")
)

func modelCallDoc(act M) M {
	d := clone(act)
	d["hint_version"] = "0.2.0"
	d["document_id"] = id("document", "activity", "model-calls")
	src := d["sources"].([]any)[0].(map[string]any)
	src["usage_normalization"] = "opencode-2.0.26:tokeninfo-v1"
	src["capabilities"] = append(src["capabilities"].([]any), "usage", "request_kind", "correlation.call_ref", "collection")
	var unavailable A
	for _, f := range src["unavailable"].([]any) {
		if f != "usage" {
			unavailable = append(unavailable, f)
		}
	}
	src["unavailable"] = unavailable
	obs := obj(d, "payload")["observations"].([]any)
	session := plain(obs[0].(map[string]any)) // the SESSION record of the plugin source
	usage := plain(obs[8].(map[string]any))   // a USAGE record
	mk := func(n int, base M, op string, at string) M {
		o := plain(base)
		o["observation_id"] = id("event", "model-call", string(rune('a'+n)))
		o["op"], o["source"], o["source_id"] = op, "plugin", src["source_id"]
		o["correlation"] = M{"session_id": callSession}
		o["time"] = M{"clock": "agent", "start": at, "end": at}
		o["provenance"].(map[string]any)["source_event_id"] = id("source-event", "model-calls", string(rune('a'+n)))
		return o
	}
	tokens := func(in, out float64) M {
		return M{"input_tokens": known("tokens", in, "exact", "harness_usage"), "output_tokens": known("tokens", out, "exact", "harness_usage"),
			"reasoning_tokens": M{"state": "unknown", "unit": "tokens", "reason": "not_supported"}}
	}
	start := mk(0, session, "SESSION", "2026-10-11T11:00:00Z")
	call := mk(1, session, "MODEL_CALL", "2026-10-11T11:00:01Z")
	call["request_kind"] = "primary"
	call["correlation"].(map[string]any)["call_ref"] = callPrimary
	done := mk(2, usage, "USAGE", "2026-10-11T11:00:03Z")
	done["request_kind"] = "primary"
	done["correlation"].(map[string]any)["call_ref"] = callPrimary
	done["usage"] = tokens(5405, 165)
	title := mk(3, session, "MODEL_CALL", "2026-10-11T11:00:01Z")
	title["request_kind"] = "title"
	titled := mk(4, usage, "USAGE", "2026-10-11T11:00:02Z")
	titled["request_kind"] = "title"
	titled["usage"] = tokens(686, 432)
	closing := mk(5, session, "SESSION", "2026-10-11T11:00:04Z")
	closing["collection"] = M{"lifecycle": "attached_at_start", "closure": "flushed", "closed_at": "2026-10-11T11:00:04Z",
		"events_dropped": known("count", 0, "exact", "count_of_records"), "errors": known("count", 0, "exact", "count_of_records"),
		"open_calls": known("count", 0, "exact", "count_of_records")}
	obj(d, "payload")["observations"] = A{start, call, done, title, titled, closing}
	obj(d, "payload")["metrics"] = A{} // the metrics of the trace name its other observations
	return reseal(d)
}

// modelCallInvalid are the negative cases of the model-call records and the closure.
func modelCallInvalid() []invalidCase {
	o := func(d M, i int) M { return obj(d, "payload", "observations", i) }
	col := func(d M) M { return obj(d, "payload", "observations", 5, "collection") }
	const base = "activity-model-call.json"
	return []invalidCase{
		{"model-call-in-v01-document", base, "schema", "version", func(d M) { d["hint_version"] = "0.1.0" }, nil},
		{"model-call-without-kind", base, "schema", "observation", func(d M) { delete(o(d, 1), "request_kind") }, nil},
		{"model-call-with-usage", base, "schema", "observation", func(d M) { o(d, 1)["usage"] = o(d, 2)["usage"] }, nil},
		{"model-call-with-coordination", base, "schema", "observation", func(d M) { o(d, 1)["coordination"] = M{"wait_evidence": "none"} }, nil},
		{"model-call-unknown-kind", base, "schema", "observation", func(d M) { o(d, 1)["request_kind"] = "embedding" }, nil},
		{"request-kind-on-read", base, "schema", "observation", func(d M) { o(d, 0)["request_kind"] = "primary" }, nil},
		{"call-ref-without-kind", base, "schema", "observation", func(d M) {
			o(d, 0)["correlation"].(map[string]any)["call_ref"] = callPrimary
		}, nil},
		{"collection-on-usage", base, "schema", "observation", func(d M) { o(d, 2)["collection"] = col(d) }, nil},
		{"collection-flushed-without-time", base, "schema", "observation", func(d M) { col(d)["closed_at"] = nil }, nil},
		{"collection-count-in-tokens", base, "schema", "observation", func(d M) { col(d)["errors"].(map[string]any)["unit"] = "tokens" }, nil},
		{"usage-normalization-free-text", base, "schema", "source", func(d M) {
			d["sources"].([]any)[0].(map[string]any)["usage_normalization"] = "opencode/local-coder"
		}, nil},
		{"collection-fractional-count", base, "schema", "observation", func(d M) {
			col(d)["open_calls"].(map[string]any)["value"] = 0.5
		}, nil},
		{"collection-closed-before-start", base, "semantic", "collection_inconsistent", func(d M) {
			col(d)["closed_at"] = "2026-10-11T10:59:00Z"
		}, nil},
		{"collection-duplicate-closure", base, "semantic", "collection_inconsistent", func(d M) {
			second := plain(o(d, 5))
			second["observation_id"] = id("event", "model-call", "z")
			obj(d, "payload")["observations"] = append(obj(d, "payload")["observations"].([]any), second)
		}, nil},
		{"call-link-two-terminals", base, "semantic", "call_link_conflict", func(d M) {
			second := plain(o(d, 2))
			second["observation_id"] = id("event", "model-call", "y")
			obj(d, "payload")["observations"] = append(obj(d, "payload")["observations"].([]any), second)
		}, nil},
		{"call-link-other-kind", base, "semantic", "call_link_conflict", func(d M) { o(d, 2)["request_kind"] = "generate" }, nil},
	}
}
