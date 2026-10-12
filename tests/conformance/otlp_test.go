package conformance_test

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

// mapSpans is the reference form of the OTLP span -> HINT Observation mapping table in
// docs/standard/compatibility.md §3. It builds each observation from an allowlist; every
// other attribute, event, link, resource attribute and the status message is dropped.
func mapSpans(t *testing.T, key []byte, otlp map[string]any) (sources []any, observations []any) {
	t.Helper()
	agentVersion := any(nil)
	for _, a := range list(obj(otlp, "resource")["attributes"]) {
		am := a.(map[string]any)
		if am["key"] == "service.version" {
			v := str(obj(am, "value")["stringValue"])
			if regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(v) {
				agentVersion = v
			}
		}
	}
	srcID := hintconf.DeriveID(key, "source", "otlp-receiver")
	sources = []any{map[string]any{"source_id": srcID, "agent": "other", "agent_version": agentVersion,
		"adapter":      map[string]any{"name": "otlp-receiver", "version": "0.1.0"},
		"capabilities": []any{"time.start", "time.end", "correlation.trace_id", "correlation.span_id", "correlation.parent_span_id", "outcome.status", "usage"},
		"unavailable":  []any{"targets", "size", "outcome.exit_code", "content_delivery"}}}
	toolOps := map[string]string{"read": "READ", "glob": "SEARCH", "grep": "SEARCH", "list": "DISCOVER",
		"edit": "EDIT_INTENT", "write": "EDIT_INTENT", "patch": "EDIT_INTENT", "bash": "EXECUTE", "task": "DELEGATE"}
	for _, s := range list(otlp["spans"]) {
		sp := s.(map[string]any)
		attrs := map[string]map[string]any{}
		for _, a := range list(sp["attributes"]) {
			am := a.(map[string]any)
			attrs[str(am["key"])] = obj(am, "value")
		}
		sv := func(k string) string { return str(attrs[k]["stringValue"]) }
		op := "UNKNOWN"
		switch sv("gen_ai.operation.name") {
		case "execute_tool":
			op = "TOOL"
			if o, ok := toolOps[sv("gen_ai.tool.name")]; ok {
				op = o
			}
		case "invoke_agent":
			op = "TURN"
		case "chat":
			op = "USAGE"
		}
		if sp["name"] == "Tool.execute" { // OpenCode native span name
			op = "TOOL"
			if o, ok := toolOps[sv("tool.name")]; ok {
				op = o
			}
		}
		status := map[float64]string{0: "unknown", 1: "success", 2: "failure"}[obj(sp, "status")["code"].(float64)]
		// trace and span IDs are pseudonymized with the project key (hint-data §4.1); the
		// source IDs stay local. A parent uses the trace-id of its span, so it equals the
		// exported span_id of the parent span.
		corr := map[string]any{}
		missing := []any{"targets", "size", "outcome.exit_code"}
		rawTrace := str(sp["traceId"])
		if v, ok := hintconf.TraceID(key, rawTrace); ok {
			corr["trace_id"] = v
		} else {
			missing = append(missing, "correlation.trace_id")
		}
		if v, ok := hintconf.SpanID(key, rawTrace, str(sp["spanId"])); ok {
			corr["span_id"] = v
		} else {
			missing = append(missing, "correlation.span_id")
		}
		if p := str(sp["parentSpanId"]); p != "" {
			if v, ok := hintconf.SpanID(key, rawTrace, p); ok {
				corr["parent_span_id"] = v
			} else {
				missing = append(missing, "correlation.parent_span_id")
			}
		}
		for _, k := range []string{"gen_ai.conversation.id", "session.id"} {
			if v := sv(k); v != "" {
				corr["session_id"] = hintconf.DeriveID(key, "session", srcID, v)
			}
		}
		for _, k := range []string{"gen_ai.tool.call.id", "tool.call_id"} {
			if v := sv(k); v != "" {
				corr["tool_call_id"] = hintconf.DeriveID(key, "call", srcID, v)
			}
		}
		if corr["session_id"] == nil {
			missing = append(missing, "correlation.session_id")
		}
		o := map[string]any{
			"observation_id": hintconf.DeriveID(key, "event", srcID, str(sp["spanId"])), "class": "observation",
			"op": op, "source": "otlp", "source_id": srcID, "precision": "exact",
			"time":        map[string]any{"start": nanos(sp["startTimeUnixNano"]), "end": nanos(sp["endTimeUnixNano"]), "clock": "agent"},
			"correlation": corr, "outcome": map[string]any{"status": status}, "missing": missing,
			"provenance": map[string]any{"captured_at": "2026-10-11T12:00:00Z",
				"transformations": []any{map[string]any{"step": "normalize", "tool": "hint-cli", "version": "0.1.0"}}},
		}
		switch op {
		case "READ":
			o["content_delivery"] = "unknown"
		case "EXECUTE":
			o["command_class"] = "UNKNOWN" // the command string is content; only a local parser may classify it
		case "DELEGATE":
			o["coordination"] = map[string]any{}
			o["missing"] = append(missing, "coordination")
		case "USAGE":
			u := map[string]any{}
			for field, k := range map[string]string{"input_tokens": "gen_ai.usage.input_tokens", "output_tokens": "gen_ai.usage.output_tokens"} {
				if v, ok := attrs[k]["intValue"]; ok { // a reported 0 stays 0; an absent attribute is unknown
					n, _ := strconv.Atoi(str(v))
					u[field] = map[string]any{"unit": "tokens", "state": "known", "value": n, "precision": "exact", "method": "otlp_attribute"}
				} else {
					u[field] = map[string]any{"unit": "tokens", "state": "unknown", "reason": "not_supported"}
				}
			}
			o["usage"] = u
		}
		observations = append(observations, o)
	}
	return sources, observations
}

func nanos(v any) any {
	n, err := strconv.ParseInt(str(v), 10, 64)
	if err != nil {
		return nil
	}
	return time.Unix(0, n).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func list(v any) []any { l, _ := v.([]any); return l }
func str(v any) string { s, _ := v.(string); return s }
func obj(v map[string]any, k string) map[string]any {
	m, _ := v[k].(map[string]any)
	return m
}

func TestOTLPSpanMapping(t *testing.T) {
	key := seqKey()
	cases := []struct {
		file    string
		wantOps []string
	}{
		{filepath.Join("otlp", "genai-spans.synthetic.json"), []string{"TURN", "USAGE", "READ", "EXECUTE", "UNKNOWN"}},
		// recorded OpenCode 1.18.15 spans (scrubbed); this shows the mapping, not 2.0.26 support
		{filepath.Join("otlp", "opencode-1.18.15-spans.scrubbed.json"), nil},
	}
	for _, c := range cases {
		var otlp map[string]any
		readJSON(t, c.file, &otlp)
		sources, obs := mapSpans(t, key, otlp)
		payload := map[string]any{"observations": obs}
		doc := map[string]any{
			"hint_version": "0.1.0", "document_type": "activity_stream", "document_id": hintconf.DeriveID(key, "document", c.file),
			"project_namespace": hintconf.NamespaceID(key), "repositories": []any{},
			"producer": map[string]any{"tool": "hint-cli", "version": "0.1.0"}, "sources": sources,
			"created_at": "2026-10-11T12:00:00Z",
			"redaction": map[string]any{"policy_id": hintconf.DeriveID(key, "policy", "default"), "profile": "detailed",
				"record_types": []any{"Observation"}, "free_text": "denied", "time_resolution_seconds": 0},
			"payload": payload,
		}
		dg, _ := hintconf.Digest(payload)
		doc["content_digest"] = dg
		raw, _ := json.Marshal(doc)
		dec, _ := hintconf.Decode(raw)
		if err := schemas.Validate(dec); err != nil {
			t.Fatalf("%s: mapped document is invalid: %v", c.file, err)
		}
		plain, _ := hintconf.Plain(dec)
		if fs := hintconf.Check(plain, nil); len(fs) > 0 {
			t.Errorf("%s: %v", c.file, fs)
		}
		for _, bad := range append(append(hintconf.Canaries, "example.invalid", "/home/", `"ses_`, `"call_`, "conv-"),
			sourceIDs(otlp)...) {
			if strings.Contains(string(raw), bad) {
				t.Errorf("%s: mapped output contains %q", c.file, bad)
			}
		}
		checkTraceCorrelation(t, c.file, key, otlp, obs)
		if c.wantOps != nil {
			var got []string
			for _, o := range obs {
				got = append(got, o.(map[string]any)["op"].(string))
			}
			if strings.Join(got, ",") != strings.Join(c.wantOps, ",") {
				t.Errorf("ops %v, want %v", got, c.wantOps)
			}
			// the chat span reports output_tokens 0: a real zero, not unknown
			u := obs[1].(map[string]any)["usage"].(map[string]any)
			if u["output_tokens"].(map[string]any)["state"] != "known" || u["output_tokens"].(map[string]any)["value"] != 0 {
				t.Errorf("reported zero usage must stay a known 0: %v", u)
			}
		}
	}
}

// sourceIDs returns every source trace-id and span-id of an OTLP capture, including links.
func sourceIDs(otlp map[string]any) []string {
	var ids []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"traceId", "spanId", "parentSpanId"} {
				if s := str(x[k]); s != "" {
					ids = append(ids, s)
				}
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(otlp)
	if len(ids) == 0 {
		panic("capture has no trace or span IDs")
	}
	return ids
}

// checkTraceCorrelation checks the trace and span ID rule of hint-data §4.1 on one capture:
// determinism, kept correlation, no source ID in the output, and other IDs under another key.
func checkTraceCorrelation(t *testing.T, file string, key []byte, otlp map[string]any, obs []any) {
	t.Helper()
	corr := func(o any) map[string]any { return o.(map[string]any)["correlation"].(map[string]any) }
	// determinism: the same capture and key give the same IDs
	_, again := mapSpans(t, key, otlp)
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(32 + i) // test key K2
	}
	_, rekeyed := mapSpans(t, other, otlp)
	exported := map[string]string{} // source "trace/span" -> exported span_id
	traces := map[string]string{}   // source trace -> exported trace_id
	for i, s := range list(otlp["spans"]) {
		sp := s.(map[string]any)
		c, c2, ck := corr(obs[i]), corr(again[i]), corr(rekeyed[i])
		for _, f := range []string{"trace_id", "span_id", "parent_span_id"} {
			if c[f] != c2[f] {
				t.Errorf("%s span %d: %s is not deterministic", file, i, f)
			}
			if c[f] != nil && c[f] == ck[f] {
				t.Errorf("%s span %d: %s does not change with the key", file, i, f)
			}
		}
		if c["trace_id"] == nil || c["span_id"] == nil {
			t.Fatalf("%s span %d: valid source IDs must give trace_id and span_id", file, i)
		}
		if prev, ok := traces[str(sp["traceId"])]; ok && prev != c["trace_id"] {
			t.Errorf("%s: one source trace gives two trace_ids", file)
		}
		traces[str(sp["traceId"])] = c["trace_id"].(string)
		exported[str(sp["traceId"])+"/"+str(sp["spanId"])] = c["span_id"].(string)
	}
	// kept correlation: a parent reference equals the exported span_id of the parent span
	// when the parent is in the capture; distinct source spans give distinct span_ids
	linked := 0
	for i, s := range list(otlp["spans"]) {
		sp := s.(map[string]any)
		p := str(sp["parentSpanId"])
		if p == "" {
			continue
		}
		if want, ok := exported[str(sp["traceId"])+"/"+p]; ok {
			linked++
			if corr(obs[i])["parent_span_id"] != want {
				t.Errorf("%s span %d: parent_span_id does not match the parent's span_id", file, i)
			}
		}
	}
	seen := map[string]bool{}
	for _, v := range exported {
		if seen[v] {
			t.Errorf("%s: two source spans give one span_id", file)
		}
		seen[v] = true
	}
	if strings.Contains(file, "synthetic") && linked < 4 {
		t.Errorf("%s: %d parent links checked, want 4", file, linked)
	}
	t.Logf("%s: %d spans, %d traces, %d parent links in the capture", file, len(exported), len(traces), linked)
}

func TestTraceAndSpanIDDerivation(t *testing.T) {
	k1, k2 := seqKey(), make([]byte, 32)
	for i := range k2 {
		k2[i] = byte(32 + i)
	}
	const tr, sp = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	a, _ := hintconf.TraceID(k1, tr)
	b, _ := hintconf.TraceID(k1, strings.ToUpper(tr))
	c, _ := hintconf.TraceID(k2, tr)
	if a != b || a == c || a == tr || len(a) != 32 {
		t.Errorf("trace: %s %s %s", a, b, c)
	}
	s1, _ := hintconf.SpanID(k1, tr, sp)
	s2, _ := hintconf.SpanID(k1, "5bf92f3577b34da6a3ce929d0e0e4736", sp) // same span-id, other trace
	s3, _ := hintconf.SpanID(k2, tr, sp)
	if len(s1) != 16 || s1 == sp || s1 == s2 || s1 == s3 {
		t.Errorf("span: %s %s %s", s1, s2, s3)
	}
	for _, bad := range [][2]string{{strings.Repeat("0", 32), sp}, {tr, strings.Repeat("0", 16)}, {tr[:31], sp},
		{tr, "00f067aa0ba902bz"}, {"", sp}} {
		if _, ok := hintconf.SpanID(k1, bad[0], bad[1]); ok {
			t.Errorf("invalid source IDs accepted: %v", bad)
		}
	}
	if _, ok := hintconf.TraceID(k1, strings.Repeat("0", 32)); ok {
		t.Error("all-zero source trace-id accepted")
	}
}
