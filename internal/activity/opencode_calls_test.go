package activity

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

// Replay of reduced OpenCode 2.0.26 model-call events (07B-E §30-§33), synthetic lines only.

func withCalls(ack bool) *Normalizer {
	n := norm()
	n.ModelCalls = true
	n.Acknowledged = func() bool { return ack }
	return n
}

func run(t *testing.T, n *Normalizer, lines ...map[string]any) []Observation {
	t.Helper()
	var out []Observation
	if _, err := n.Stream(strings.NewReader(events(lines...)), func(o Observation) error { out = append(out, o); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

var wall int64 = 1791700000000

func hook(name string, fields map[string]any) map[string]any {
	wall += 1000
	l := map[string]any{"hook": name, "t_wall_ms": wall}
	for k, v := range fields {
		l[k] = v
	}
	return l
}

func attach() map[string]any { return hook("hint.attach", nil) }

func request(session, kind string) map[string]any {
	return hook("model.request", map[string]any{"input": map[string]any{"sessionID": session, "kind": kind}})
}

func bus(id, typ, session string, f map[string]any) map[string]any {
	e := map[string]any{"id": id, "type": typ, "sessionID": session}
	for k, v := range f {
		e[k] = v
	}
	return hook("event", map[string]any{"event": e})
}

func tok(in, out, reasoning, read, write float64) map[string]any {
	return map[string]any{"input": in, "output": out, "reasoning": reasoning, "cache": map[string]any{"read": read, "write": write}}
}

func closing(drained bool) map[string]any {
	return hook("hint.close", map[string]any{"close": map[string]any{"drained": drained, "dropped": 0, "errors": 0}})
}

func created(id, session, parent string) map[string]any {
	f := map[string]any{}
	if parent != "" {
		f["parentID"] = parent
	}
	return bus(id, "session.created", session, f)
}

func step(id, session, msg string, tokens map[string]any) []map[string]any {
	return []map[string]any{request(session, "primary"), bus(id+"s", "session.step.started", session, map[string]any{"assistantMessageID": msg}),
		bus(id+"e", "session.step.ended", session, map[string]any{"assistantMessageID": msg, "tokens": tokens})}
}

func chain(extra ...map[string]any) []map[string]any {
	l := []map[string]any{attach(), created("e1", "s1", "")}
	l = append(l, step("p1", "s1", "m1", tok(5405, 165, 0, 0, 0))...)
	l = append(l, request("s1", "title"), bus("t1", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(686, 432, 0, 0, 0)}))
	l = append(l, created("e2", "s2", "s1"))
	l = append(l, step("p2", "s2", "m2", tok(900, 100, 20, 300, 0))...)
	l = append(l, bus("c1", "session.compaction.started", "s1", nil), request("s1", "compaction"),
		bus("c2", "session.usage.recorded", "s1", map[string]any{"source": "compaction", "tokens": tok(2000, 300, 0, 0, 0)}),
		bus("c3", "session.compaction.ended", "s1", map[string]any{"tokens": tok(2000, 300, 0, 0, 0)}))
	return append(l, extra...)
}

func ops(obs []Observation, op string) []Observation {
	var out []Observation
	for _, o := range obs {
		if o.Op == op {
			out = append(out, o)
		}
	}
	return out
}

func closure(t *testing.T, obs []Observation, raw string) *Collection {
	t.Helper()
	sid := hint.DeriveID(testKey, "session", norm().SourceID, raw)
	for _, o := range ops(obs, "SESSION") {
		if o.Correlation["session_id"] == sid {
			return o.Collection
		}
	}
	return nil
}

func TestACompleteChainLinksEveryCallAndClosesEachSession(t *testing.T) {
	obs := run(t, withCalls(true), append(chain(), closing(true))...)
	starts, terms := ops(obs, "MODEL_CALL"), ops(obs, "USAGE")
	if len(starts) != 4 || len(terms) != 4 { // 2 primary steps, 1 title, 1 compaction; the compaction counted once
		t.Fatalf("starts %d terminals %d", len(starts), len(terms))
	}
	refs := map[string]int{}
	for _, o := range append(starts, terms...) {
		if o.RequestKind == "primary" {
			if o.Correlation["call_ref"] == "" {
				t.Fatal("a primary record without its exact call identity")
			}
			refs[o.Correlation["call_ref"]]++
		} else if o.Correlation["call_ref"] != "" {
			t.Fatal("a call_ref was fabricated for an auxiliary call")
		}
	}
	for r, n := range refs {
		if n != 2 {
			t.Fatalf("call %s has %d records", r, n)
		}
	}
	for _, s := range []string{"s1", "s2"} {
		c := closure(t, obs, s)
		if c == nil || c.Lifecycle != "attached_at_start" || c.Closure != "flushed" || c.ClosedAt == nil || *c.Errors.Value != 0 || *c.OpenCalls.Value != 0 || *c.EventsDropped.Value != 0 {
			t.Fatalf("%s closure %+v", s, c)
		}
	}
	for _, o := range terms {
		if o.Provenance.SourceEventID == "" || o.Usage.InputTokens.Method != "harness_usage" {
			t.Fatalf("terminal %+v", o)
		}
	}
	child := terms[slices.IndexFunc(terms, func(o Observation) bool {
		return o.Correlation["session_id"] == hint.DeriveID(testKey, "session", norm().SourceID, "s2")
	})]
	if *child.Usage.OutputTokens.Value != 120 || *child.Usage.ReasoningTokens.Value != 20 || *child.Usage.InputTokens.Value != 900 ||
		*child.Usage.CacheReadTokens.Value != 300 || child.Usage.CacheWriteTokens.State != "unknown" {
		t.Fatalf("output must be the generated total; cache separate: %+v", child.Usage)
	}
}

func TestUsageNormalizationKeepsZerosAndInvalidCountersUnknown(t *testing.T) {
	f := func(v ...float64) *ocTokens {
		p := func(x float64) *float64 { return &x }
		tk := &ocTokens{Input: p(v[0]), Output: p(v[1]), Reasoning: p(v[2])}
		tk.Cache = &struct {
			Read  *float64 `json:"read"`
			Write *float64 `json:"write"`
		}{p(v[3]), p(v[4])}
		return tk
	}
	if u := usageOf(f(0, 0, 0, 0, 0), "x"); u.InputTokens.State != "unknown" || u.OutputTokens.State != "unknown" {
		t.Fatal("an all-zero call is unknown, never zero cost")
	}
	if u := usageOf(f(100, 0, 40, 0, 0), "x"); u.OutputTokens.State != "unknown" || u.InputTokens.State != "known" {
		t.Fatal("output 0 with reasoning can be clipped: unknown")
	}
	for _, bad := range [][]float64{{-1, 5, 0, 0, 0}, {10.5, 5, 0, 0, 0}, {10, 5, 0, 0, 2e12}} {
		if u := usageOf(f(bad...), "x"); u.InputTokens.Reason != "source_error" || u.OutputTokens.State != "unknown" {
			t.Fatalf("%v must be unknown", bad)
		}
	}
	if u := usageOf(&ocTokens{}, "x"); u.InputTokens.State != "unknown" {
		t.Fatal("a missing field is unknown")
	}
}

func TestALostModelRequestOrStepIsACollectionError(t *testing.T) {
	lost := chain(bus("x1", "session.step.started", "s1", map[string]any{"assistantMessageID": "m9"}),
		bus("x2", "session.step.ended", "s1", map[string]any{"assistantMessageID": "m9", "tokens": tok(10, 1, 0, 0, 0)}), closing(true))
	if c := closure(t, run(t, withCalls(true), lost...), "s1"); *c.Errors.Value == 0 {
		t.Fatal("a step without its model.request must not look complete")
	}
	lostStep := chain(request("s1", "primary"), closing(true))
	if c := closure(t, run(t, withCalls(true), lostStep...), "s1"); *c.Errors.Value == 0 {
		t.Fatal("a model.request without its step must not look complete")
	}
	lostUsage := chain(request("s1", "primary"), bus("y1", "session.step.started", "s1", map[string]any{"assistantMessageID": "m8"}), closing(true))
	if c := closure(t, run(t, withCalls(true), lostUsage...), "s1"); *c.OpenCalls.Value != 1 {
		t.Fatal("a step without terminal is an open call")
	}
}

func TestARetriedStepHasUnknownUsage(t *testing.T) {
	l := []map[string]any{attach(), created("e1", "s1", ""), request("s1", "primary"),
		bus("a", "session.step.started", "s1", map[string]any{"assistantMessageID": "m1"}),
		bus("b", "session.retry.scheduled", "s1", map[string]any{"assistantMessageID": "m1"}),
		bus("c", "session.step.ended", "s1", map[string]any{"assistantMessageID": "m1", "tokens": tok(10, 1, 0, 0, 0)}), closing(true)}
	u := ops(run(t, withCalls(true), l...), "USAGE")
	if len(u) != 1 || u[0].Usage.InputTokens.State != "unknown" {
		t.Fatal("per-attempt usage of retried requests is unknown")
	}
}

func TestOwnershipIsExplicitAndMidSessionAttachIsLabeled(t *testing.T) {
	// an unrelated session of a shared server: no record at all
	l := []map[string]any{attach(), bus("u1", "session.step.started", "s9", map[string]any{"assistantMessageID": "m1"}),
		bus("u2", "session.step.ended", "s9", map[string]any{"assistantMessageID": "m1", "tokens": tok(10, 1, 0, 0, 0)}),
		created("u3", "s8", "s9"), closing(true)}
	if obs := run(t, withCalls(true), l...); len(obs) != 0 {
		t.Fatalf("unrelated sessions were recorded: %d", len(obs))
	}
	// created before the attach: a model call of this process owns it as attached mid-session
	mid := []map[string]any{created("e0", "s1", ""), attach()}
	mid = append(mid, step("p", "s1", "m1", tok(10, 1, 0, 0, 0))...)
	if c := closure(t, run(t, withCalls(true), append(mid, closing(true))...), "s1"); c.Lifecycle != "attached_mid_session" {
		t.Fatalf("lifecycle %s", c.Lifecycle)
	}
}

func TestReplayCollapsesAndConflictingDuplicatesAreErrors(t *testing.T) {
	dup := bus("t1", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(686, 432, 0, 0, 0)})
	obs := run(t, withCalls(true), append(chain(dup), closing(true))...)
	if len(ops(obs, "USAGE")) != 4 || *closure(t, obs, "s1").Errors.Value != 0 {
		t.Fatal("an identical replay must collapse")
	}
	other := bus("t1", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(1, 1, 0, 0, 0)})
	if c := closure(t, run(t, withCalls(true), append(chain(other), closing(true))...), "s1"); *c.Errors.Value == 0 {
		t.Fatal("the same event with another body is a conflict")
	}
}

func TestCompactionAlternativesAreCountedOnceAndMustAgree(t *testing.T) {
	l := []map[string]any{attach(), created("e1", "s1", ""), bus("c1", "session.compaction.started", "s1", nil), request("s1", "compaction"),
		bus("c2", "session.usage.recorded", "s1", map[string]any{"source": "compaction", "tokens": tok(2000, 300, 0, 0, 0)}),
		bus("c3", "session.compaction.ended", "s1", map[string]any{"tokens": tok(2100, 300, 0, 0, 0)}), closing(true)}
	obs := run(t, withCalls(true), l...)
	u := ops(obs, "USAGE")
	if len(u) != 1 || u[0].Usage.InputTokens.State != "unknown" || *closure(t, obs, "s1").Errors.Value == 0 {
		t.Fatal("unequal alternative observations of one compaction: one unknown terminal and an error")
	}
	overlap := []map[string]any{attach(), created("e1", "s1", ""), bus("c1", "session.compaction.started", "s1", nil),
		bus("c4", "session.compaction.started", "s1", nil), closing(true)}
	if c := closure(t, run(t, withCalls(true), overlap...), "s1"); *c.Errors.Value == 0 || *c.OpenCalls.Value != 2 {
		t.Fatal("overlapping compactions are ambiguous")
	}
}

func TestClosureNeedsDrainAcknowledgmentAndAFinalLine(t *testing.T) {
	if c := closure(t, run(t, withCalls(false), append(chain(), closing(true))...), "s1"); c.Closure != "unknown" || c.ClosedAt != nil {
		t.Fatal("without the child's acknowledgment the closure is not flushed")
	}
	if c := closure(t, run(t, withCalls(true), append(chain(), closing(false))...), "s1"); c.Closure != "unknown" {
		t.Fatal("a flush failure is not flushed")
	}
	c := closure(t, run(t, withCalls(true), chain()...), "s1")
	if c.Closure != "interrupted" || c.ClosedAt != nil || c.EventsDropped.State != "unknown" {
		t.Fatalf("end of input without closure: %+v", c)
	}
}

func TestALateEventAfterTheClosureIsStillRecorded(t *testing.T) {
	late := bus("z1", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(5, 5, 0, 0, 0)})
	obs := run(t, withCalls(true), append(chain(), closing(true), late)...)
	if len(ops(obs, "SESSION")) != 2 || len(ops(obs, "USAGE")) != 5 {
		t.Fatal("a late event is recorded after the closure (the server sees it as stale), and closures are not repeated")
	}
}

func TestIndependentRunsWithEqualRawIDsNeverMerge(t *testing.T) {
	a := run(t, withCalls(true), append(chain(), closing(true))...)
	other := withCalls(true)
	other.SourceID = hint.DeriveID(testKey, "source", "another-run")
	b := run(t, other, append(chain(), closing(true))...)
	for _, x := range a {
		for _, y := range b {
			if x.ObservationID == y.ObservationID || x.Correlation["call_ref"] != "" && x.Correlation["call_ref"] == y.Correlation["call_ref"] ||
				x.Provenance.SourceEventID != "" && x.Provenance.SourceEventID == y.Provenance.SourceEventID {
				t.Fatal("identities of independent runs merged")
			}
		}
	}
}

func TestModelCallLinesAreSkippedForAnUncheckedVersion(t *testing.T) {
	n := norm()
	skipped, err := n.Stream(strings.NewReader(events(append(chain(), closing(true))...)), func(Observation) error {
		t.Fatal("no record for an unchecked OpenCode version")
		return nil
	})
	if err != nil || skipped == 0 {
		t.Fatal(skipped, err)
	}
}

func TestTheModelCallSourceReportsUsageUnderItsNormalization(t *testing.T) {
	plain := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true})
	with := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true, ModelCalls: true})
	if plain.SourceID == with.SourceID || with.UsageNormalization != UsageNormalization || slices.Contains(with.Unavailable, "usage") ||
		!slices.Contains(with.Capabilities, "usage") || !slices.Contains(with.Capabilities, "collection") || plain.UsageNormalization != "" {
		t.Fatalf("%+v", with)
	}
}

func TestModelCallRecordsFormAValidV02Document(t *testing.T) {
	n := withCalls(true)
	n.TaskID, n.TaskFamilyID = hint.DeriveID(testKey, "task", "t1"), hint.DeriveID(testKey, "family", "f1")
	src := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true, FamilyDeclared: true, ModelCalls: true})
	n.SourceID = src.SourceID
	obs := run(t, n, append(chain(), closing(true))...)
	plain := func(v any) any {
		b, _ := json.Marshal(v)
		var x any
		json.Unmarshal(b, &x)
		return x
	}
	payload := map[string]any{"observations": plain(obs), "metrics": []any{}}
	digest, err := hint.Digest(payload)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"hint_version": "0.2.0", "document_type": "activity_stream", "document_id": hint.DeriveID(testKey, "doc", "1"),
		"project_namespace": hint.DeriveID(testKey, "ns"), "repositories": []any{hint.DeriveID(testKey, "repo")},
		"producer": map[string]any{"tool": "hint-cli", "version": "0.1.0"}, "created_at": "2026-10-19T12:00:00Z",
		"sources": []any{plain(src)}, "redaction": map[string]any{"policy_id": hint.DeriveID(testKey, "policy"), "profile": "detailed",
			"record_types": []any{"Observation"}, "free_text": "denied", "time_resolution_seconds": 0},
		"content_digest": digest, "payload": payload}
	sch, err := hint.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	dec, err := hint.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(dec); err != nil {
		t.Fatal(err)
	}
	if f := hint.Check(doc, nil); len(f) > 0 {
		t.Fatal(f)
	}
}
