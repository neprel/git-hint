package activity

import (
	"testing"
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
