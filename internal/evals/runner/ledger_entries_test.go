package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeClock steps the wall clock and the monotonic time independently.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (f *fakeClock) clock() *ledgerClock {
	return &ledgerClock{
		wall: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.wall },
		mono: func() time.Duration { f.mu.Lock(); defer f.mu.Unlock(); return f.mono },
	}
}

// step advances both clocks by mono and moves the wall clock by an extra jump.
func (f *fakeClock) step(jump, mono time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall, f.mono = f.wall.Add(mono+jump), f.mono+mono
}

func entryUsage() entryFacts {
	return entryFacts{termination: "response_ended", started: true, statusClass: "2xx"}
}

// A wall-clock step forward or backward between the anchor and the seal changes only
// clock_step_ms; the monotonic entry offsets and their order stay as measured.
func TestLedgerEntriesMonotonicUnderWallSteps(t *testing.T) {
	for name, wallStep := range map[string]time.Duration{"none": 0, "backward": -90 * time.Minute, "forward": 3 * time.Hour} {
		f := &fakeClock{wall: time.Date(2026, 10, 24, 10, 0, 0, 123456789, time.UTC)}
		c := &Counter{token: "t", limit: 10}
		c.ledger.clock = f.clock()
		var es []*ledgerEntry
		for i := 0; i < 3; i++ {
			f.step(0, 100*time.Millisecond)
			e, _ := c.admit()
			es = append(es, e)
		}
		f.step(wallStep, 50*time.Millisecond) // the wall clock jumps; the monotonic time does not
		v := int64(10)
		for _, e := range es {
			f.step(0, 10*time.Millisecond)
			c.finish(e, EntryComplete, &usageComponents{Prompt: &v, Completion: &v}, entryUsage())
		}
		f.step(0, time.Second)
		r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
		// the anchor is the first admission (monotonic 100 ms): offsets are measured from it
		if len(r.Entries) != 3 || *r.SealedMs != 1280 || *r.FinalizedMs != 1280 {
			t.Fatalf("%s: entries %d sealed %v finalized %v", name, len(r.Entries), *r.SealedMs, *r.FinalizedMs)
		}
		for i, e := range r.Entries {
			if e.Ordinal != i+1 || e.AdmittedMs != int64(100*i) || e.FinishedMs != int64(250+10*(i+1)) {
				t.Fatalf("%s: entry %d %+v", name, i, e)
			}
		}
		if *r.ClockStepMs != wallStep.Milliseconds() {
			t.Fatalf("%s: clock step %d, want %d", name, *r.ClockStepMs, wallStep.Milliseconds())
		}
		if r.SealedAt != c.SealedAt() || r.AnchorUTC != "2026-10-24T10:00:00.223456789Z" {
			t.Fatalf("%s: anchor %s sealed %s / %s", name, r.AnchorUTC, r.SealedAt, c.SealedAt())
		}
	}
}

func TestLedgerWithoutRequestsHasNoEntriesAndNoTiming(t *testing.T) {
	c := &Counter{token: "t", limit: 10}
	r := c.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"entries", "anchor_utc", "sealed_ms", "finalized_ms", "clock_step_ms"} {
		if _, ok := m[k]; ok {
			t.Fatalf("an account without requests has %s", k)
		}
	}
	if r.State != "incomplete" || r.SealedAt == "" {
		t.Fatalf("empty account %+v", r)
	}
}

func TestLedgerOverTheBoundHasNoEntriesAndAReason(t *testing.T) {
	c := &Counter{token: "t", limit: 2000}
	v := int64(1)
	for i := 0; i < 1001; i++ {
		e, _ := c.admit()
		c.finish(e, EntryComplete, &usageComponents{Prompt: &v, Completion: &v}, entryUsage())
	}
	r := c.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	if r.Entries != nil || r.SealedMs != nil || r.AnchorUTC != "" || r.State != "incomplete" {
		t.Fatalf("over the bound: entries %d sealed %v state %s", len(r.Entries), r.SealedMs, r.State)
	}
	found := false
	for _, x := range r.Reasons {
		found = found || x == "entries_over_limit"
	}
	if !found || r.Counts[EntryComplete] != 1001 {
		t.Fatalf("reasons %v counts %v", r.Reasons, r.Counts)
	}
}

// A seal timeout freezes the entry at the end of the drain; a later finish changes nothing, and
// a late request after the seal adds a revision with identical entries and timing.
func TestLedgerEntriesAreFrozen(t *testing.T) {
	f := &fakeClock{wall: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
	c := &Counter{token: "t", limit: 10}
	c.ledger.clock = f.clock()
	f.step(0, 5*time.Millisecond)
	e, _ := c.admit()
	f.step(0, 20*time.Millisecond)
	first := c.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	if x := first.Entries[0]; x.State != EntryUnknown || x.Termination != "seal_timeout" || x.FinishedMs != *first.FinalizedMs {
		t.Fatalf("seal timeout entry %+v", x)
	}
	v := int64(3)
	c.finish(e, EntryComplete, &usageComponents{Prompt: &v, Completion: &v}, entryUsage())
	c.admit() // late: refused and counted, never an entry
	again := c.Report()
	a, _ := json.Marshal([]any{first.Entries, first.AnchorUTC, first.SealedMs, first.FinalizedMs, first.ClockStepMs, first.SealedAt})
	b, _ := json.Marshal([]any{again.Entries, again.AnchorUTC, again.SealedMs, again.FinalizedMs, again.ClockStepMs, again.SealedAt})
	if string(a) != string(b) || again.Revision != 1 || len(again.Entries) != 1 {
		t.Fatalf("the entries changed after the seal:\n%s\n%s", a, b)
	}
}

func TestLedgerErrorStatusAndProxyError(t *testing.T) {
	_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":0}}`))
	}, 4)
	post(t, context.Background(), url, request)
	r := seal(c)
	if x := r.Entries[0]; x.State != EntryError || x.Termination != "error_status" || x.StatusClass != "5xx" || !x.ResponseStarted || x.StreamDone != nil {
		t.Fatalf("error status entry %+v", x)
	}
	if p := r.Entries[0].Components["prompt"]; p == nil || *p != 5 {
		t.Fatalf("the error entry's partial usage is not kept: %v", r.Entries[0].Components)
	}
	// an unreachable upstream: the proxy's error handler writes 502
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := StartLive(LiveConfig{BaseURL: "http://127.0.0.1:1/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lv.Close() })
	lv.ContainerHost = "127.0.0.1"
	c2 := lv.NewTrial(4)
	post(t, context.Background(), lv.BaseURL(c2)+"/chat/completions", request)
	r2 := seal(c2)
	if x := r2.Entries[0]; x.State != EntryError || x.Termination != "proxy_error" || x.StatusClass != "5xx" {
		t.Fatalf("proxy error entry %+v", x)
	}
}

func TestLedgerStreamFacts(t *testing.T) {
	_, c, url, _ := ledgerProxy(t, sse(usage, "data: [DONE]\n\n"), 4)
	post(t, context.Background(), url, request)
	_, c2, url2, _ := ledgerProxy(t, sse(usage), 4) // no [DONE]
	post(t, context.Background(), url2, request)
	r, r2 := seal(c), seal(c2)
	if x := r.Entries[0]; x.State != EntryComplete || x.Termination != "response_ended" || x.StreamDone == nil || !*x.StreamDone || x.Model != "verified" {
		t.Fatalf("complete stream entry %+v", x)
	}
	if x := r2.Entries[0]; x.State != EntryTruncated || x.Termination != "response_ended" || x.StreamDone == nil || *x.StreamDone {
		t.Fatalf("truncated stream entry %+v", x)
	}
}
