package activity

import (
	"encoding/json"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

// The client mirror of the server completeness rule (07B-E §33) on replayed adapter output.

func coverageOf(t *testing.T, lines []map[string]any, ack bool) Coverage {
	t.Helper()
	n := withCalls(ack)
	n.TaskID = "t"
	src := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true, ModelCalls: true})
	n.SourceID = src.SourceID
	return UsageCoverage(run(t, n, lines...), map[string]Source{src.SourceID: src})
}

func TestACompleteReplayedChainHasKnownUsage(t *testing.T) {
	c := coverageOf(t, append(chain(), closing(true)), true)
	// primary 5405+165 and 900+120, title 686+432, compaction 2000+300
	if c.State != "complete" || c.Input != 5405+900+686+2000 || c.Output != 165+120+432+300 || c.Normalization != UsageNormalization || c.Cache != nil {
		t.Fatalf("%+v", c)
	}
}

func TestReplayedNegativesAreIncomplete(t *testing.T) {
	cases := map[string]struct {
		lines []map[string]any
		ack   bool
		want  string
	}{
		"no acknowledgment":   {append(chain(), closing(true)), false, "closure_not_flushed"},
		"flush failure":       {append(chain(), closing(false)), true, "closure_not_flushed"},
		"no closure line":     {chain(), true, "closure_not_flushed"},
		"lost model.request":  {append(chain(bus("x1", "session.step.started", "s1", map[string]any{"assistantMessageID": "m9"}), bus("x2", "session.step.ended", "s1", map[string]any{"assistantMessageID": "m9", "tokens": tok(1, 1, 0, 0, 0)})), closing(true)), true, "collection_loss"},
		"overlapping titles":  {append(chain(request("s1", "title"), bus("t9", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(1, 1, 0, 0, 0)})), closing(true)), true, "title_ambiguous"},
		"dropped title usage": {append(chain(request("s2", "title")), closing(true)), true, "title_ambiguous"},
		"generate":            {append(chain(request("s1", "generate")), closing(true)), true, "generate_without_terminal"},
		"late event":          {append(chain(), closing(true), bus("z1", "session.usage.recorded", "s1", map[string]any{"source": "title", "tokens": tok(5, 5, 0, 0, 0)})), true, "stale_after_closure"},
	}
	for name, c := range cases {
		if got := coverageOf(t, c.lines, c.ack); got.State == "complete" || got.Reason != c.want {
			t.Errorf("%s: %+v (want %s)", name, got, c.want)
		}
	}
	mid := []map[string]any{created("e0", "s1", ""), attach()}
	mid = append(mid, step("p", "s1", "m1", tok(10, 1, 0, 0, 0))...)
	if got := coverageOf(t, append(mid, closing(true)), true); got.Reason != "attached_mid_session" {
		t.Errorf("mid-session attach: %+v", got)
	}
}

func TestCoverageNeedsTheSourceNormalizationAndCapabilities(t *testing.T) {
	n := withCalls(true)
	src := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true, ModelCalls: true})
	n.SourceID = src.SourceID
	obs := run(t, n, append(chain(), closing(true))...)
	plain := src
	plain.UsageNormalization = ""
	if c := UsageCoverage(obs, map[string]Source{src.SourceID: plain}); c.Reason != "normalization_unknown" {
		t.Fatalf("%+v", c)
	}
	nocap := src
	nocap.Capabilities = []string{"usage"}
	if c := UsageCoverage(obs, map[string]Source{src.SourceID: nocap}); c.Reason != "capabilities_missing" {
		t.Fatalf("%+v", c)
	}
}

// roundTrip serializes records as the spool and the export do and reads them back.
func roundTrip(t *testing.T, obs []Observation) []Observation {
	t.Helper()
	b, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	var out []Observation
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func replayed(t *testing.T, lines ...map[string]any) ([]Observation, map[string]Source) {
	t.Helper()
	n := withCalls(true)
	n.TaskID = "t"
	src := OpenCodeSource(testKey, SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true, ModelCalls: true})
	n.SourceID = src.SourceID
	return run(t, n, lines...), map[string]Source{src.SourceID: src}
}

func TestEveryRelevantSessionOfTheTaskNeedsCompleteCollection(t *testing.T) {
	obs, srcs := replayed(t, append(chain(), closing(true))...)
	if c := UsageCoverage(roundTrip(t, obs), srcs); c.State != "complete" {
		t.Fatalf("baseline %+v", c)
	}
	base := obs[0]
	extra := func(op, session string, mut func(*Observation)) []Observation {
		o := base
		o.ObservationID = hint.DeriveID(testKey, "extra", op, session)
		o.Op, o.RequestKind, o.Usage, o.Collection = op, "", nil, nil
		o.Correlation = map[string]string{"session_id": sid(session), "task_id": "t"}
		if mut != nil {
			mut(&o)
		}
		return append(append([]Observation{}, obs...), o)
	}
	nine := 9000.0
	cases := map[string]struct {
		obs  []Observation
		want string
	}{
		"untyped usage": {extra("USAGE", "s1", func(o *Observation) {
			o.Usage = &Usage{InputTokens: scan.Known("tokens", nine, "harness_usage"), OutputTokens: scan.Known("tokens", nine, "harness_usage")}
		}), "untyped_usage"},
		"child with only tools": {extra("TOOL", "unclosed-child", nil), "closure_missing"},
		"child interrupted closure": {extra("SESSION", "unclosed-child", func(o *Observation) {
			z := scan.Known("count", 0, "count_of_records")
			o.Collection = &Collection{Lifecycle: "attached_at_start", Closure: "interrupted", EventsDropped: z, Errors: z, OpenCalls: z}
		}), "closure_not_flushed"},
		"record without session": {extra("TOOL", "", func(o *Observation) { delete(o.Correlation, "session_id") }), "session_unknown"},
	}
	for name, c := range cases {
		if got := UsageCoverage(roundTrip(t, c.obs), srcs); got.State == "complete" || got.Reason != c.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
	watcher := extra("TOOL", "w", func(o *Observation) { o.Source = "watcher" })
	if got := UsageCoverage(roundTrip(t, watcher), srcs); got.State != "complete" {
		t.Errorf("a watcher-only record is not an agent session: %+v", got)
	}
}

func TestInvalidPluginCountersStayIncompleteAfterSerialization(t *testing.T) {
	for _, v := range []any{nil, -1, 0.5, 1e12} {
		end := closing(true)
		if v == nil {
			delete(end["close"].(map[string]any), "errors")
		} else {
			end["close"].(map[string]any)["errors"] = v
		}
		obs, srcs := replayed(t, append(chain(), end)...)
		if c := UsageCoverage(roundTrip(t, obs), srcs); c.State == "complete" || c.Reason != "collection_loss" {
			t.Errorf("errors %v: %+v", v, c)
		}
	}
}

func TestEventsBeforeADelayedAssociationAreReplayedOrTheTaskIsIncomplete(t *testing.T) {
	// the reviewer's probe: the grandchild s3 names parent s4 before s4's link to the root
	probe := chain(created("grandchild-create", "s3", "s4"))
	probe = append(probe, step("early-grandchild", "s3", "early-message", tok(9999, 42, 0, 0, 0))...)
	probe = append(probe, toolCall("read", "s3", "early-read", "/w/proj/grandchild.go")...)
	probe = append(probe, created("parent-create", "s4", "s1"), closing(true))
	obs, srcs := replayed(t, probe...)
	c := UsageCoverage(roundTrip(t, obs), srcs)
	if c.State != "complete" || c.Input != 8991+9999 || c.Output != 1017+42 {
		t.Fatalf("every retained event counts exactly: %+v", c)
	}
	// multilevel: s6 -> s5 -> s4 -> s1 with primary, retry, title and compaction before any link
	deep := chain(created("c6", "s6", "s5"), created("c5", "s5", "s4"))
	deep = append(deep, request("s6", "primary"), bus("d1", "session.step.started", "s6", map[string]any{"assistantMessageID": "m6"}),
		bus("d2", "session.retry.scheduled", "s6", map[string]any{"assistantMessageID": "m6"}),
		bus("d3", "session.step.ended", "s6", map[string]any{"assistantMessageID": "m6", "tokens": tok(5, 5, 0, 0, 0)}))
	deep = append(deep, request("s5", "title"), bus("d4", "session.usage.recorded", "s5", map[string]any{"source": "title", "tokens": tok(7, 3, 0, 0, 0)}))
	deep = append(deep, bus("d5", "session.compaction.started", "s5", nil), request("s5", "compaction"),
		bus("d6", "session.compaction.ended", "s5", map[string]any{"tokens": tok(11, 2, 0, 0, 0)}))
	deep = append(deep, created("c4", "s4", "s1"), closing(true))
	obs, srcs = replayed(t, deep...)
	c = UsageCoverage(roundTrip(t, obs), srcs)
	if c.State == "complete" || c.Reason != "usage_unknown" { // the retried step's usage is unknown: never complete
		t.Fatalf("retry before the association: %+v", c)
	}
	if len(ops(obs, "MODEL_CALL")) != 4+3 { // every held start was replayed (primary, title, compaction)
		t.Fatalf("held starts: %d", len(ops(obs, "MODEL_CALL")))
	}
	// without the retry the same ancestry is complete with the exact total
	noRetry := deep[:0:0]
	for _, l := range deep {
		if e, ok := l["event"].(map[string]any); ok && e["type"] == "session.retry.scheduled" {
			continue
		}
		noRetry = append(noRetry, l)
	}
	obs, srcs = replayed(t, noRetry...)
	if c := UsageCoverage(roundTrip(t, obs), srcs); c.State != "complete" || c.Input != 8991+5+7+11 || c.Output != 1017+5+3+2 {
		t.Fatalf("late multilevel ancestry: %+v", c)
	}
	// an unrelated root with early events stays absent; owned children are unaffected
	unrel := chain(created("x1", "x", ""))
	unrel = append(unrel, step("x", "x", "mx", tok(9999, 1, 0, 0, 0))...)
	obs, srcs = replayed(t, append(unrel, closing(true))...)
	if sessionsOf(obs)[sid("x")] {
		t.Fatal("an unrelated session was recorded")
	}
	if c := UsageCoverage(roundTrip(t, obs), srcs); c.State != "complete" || c.Input != 8991 {
		t.Fatalf("owned sessions unaffected: %+v", c)
	}
}

func TestAWaitingOverflowMakesEveryClosureIncomplete(t *testing.T) {
	flood := chain()
	for i := 0; i < 10001; i++ {
		flood = append(flood, request("pending", "title")) // a session whose association never arrives
	}
	obs, srcs := replayed(t, append(flood, closing(true))...)
	if c := UsageCoverage(roundTrip(t, obs), srcs); c.State == "complete" || c.Reason != "collection_loss" {
		t.Fatalf("an overflow must not look complete: %+v", c)
	}
}
