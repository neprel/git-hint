package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ledgerProxy starts the model proxy in front of a synthetic upstream (no model is involved).
func ledgerProxy(t *testing.T, up http.HandlerFunc, limit int) (*Live, *Counter, string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		up(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := StartLive(LiveConfig{BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lv.Close() })
	lv.ContainerHost = "127.0.0.1"
	c := lv.NewTrial(limit)
	return lv, c, lv.BaseURL(c) + "/chat/completions", &n
}

const request = `{"model":"local-coder","messages":[]}`

func post(t *testing.T, ctx context.Context, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	resp, err := testClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// sse writes the given chunks with a flush after each (split anywhere, also inside a UTF-8
// character or a line).
func sse(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, c := range chunks {
			io.WriteString(w, c)
			w.(http.Flusher).Flush()
		}
	}
}

const usage = `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":7}}}` + "\n\n"

func seal(c *Counter) *LedgerReport {
	return c.Seal(2*time.Second, BypassEnforced, ScopeIsolatedContainer)
}

func TestLedgerCompleteStreamAndJSON(t *testing.T) {
	content := "data: {\"choices\":[{\"delta\":{\"content\":\"grüße ✓\"}}]}\n\n"
	// the stream is split inside the multi-byte characters and inside the data lines
	stream := content + usage + "data: [DONE]\n\n"
	var chunks []string
	for i := 0; i < len(stream); i += 7 {
		chunks = append(chunks, stream[i:min(i+7, len(stream))])
	}
	_, c, url, _ := ledgerProxy(t, sse(chunks...), 4)
	code, body := post(t, context.Background(), url, request)
	if code != 200 || body != stream {
		t.Fatalf("forwarded %d %q", code, body)
	}
	r := seal(c)
	want := map[string]int64{"prompt": 120, "completion": 30, "cached": 100, "reasoning": 7}
	if r.State != "complete" || !r.TotalComplete || r.Counts[EntryComplete] != 1 || r.ModelCheck != "verified" || r.Normalization != UsageNormalization {
		t.Fatalf("report %+v", r)
	}
	for k, v := range want {
		if r.Components[k] != v {
			t.Fatalf("components %v", r.Components)
		}
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"grüße", "lan-secret-word", "local-coder", "choices"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("report holds %q: %s", leak, b)
		}
	}
	// a non-streaming JSON response
	_, c2, url2, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}, 4)
	post(t, context.Background(), url2, request)
	if r := seal(c2); r.State != "complete" || r.Components["prompt"] != 5 || r.ComponentsN["cached"] != 0 {
		t.Fatalf("json report %+v", r)
	}
}

func TestLedgerIncompleteStates(t *testing.T) {
	cases := []struct {
		name  string
		up    http.HandlerFunc
		state string
	}{
		{"no usage", sse("data: {\"choices\":[]}\n\n", "data: [DONE]\n\n"), EntryMissingUsage},
		{"usage after done", sse("data: [DONE]\n\n", usage), EntryMissingUsage},
		{"truncated", sse("data: {\"choices\":[]}\n\n", usage), EntryTruncated},
		{"duplicate equal usage", sse(usage, usage, "data: [DONE]\n\n"), EntryComplete},
		{"conflicting usage", sse(usage, strings.Replace(usage, "120", "121", 1), "data: [DONE]\n\n"), EntryUsageConflict},
		{"upstream error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			io.WriteString(w, `{"usage":{"prompt_tokens":9,"completion_tokens":0}}`)
		}, EntryError},
		{"json without usage", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"choices":[]}`) }, EntryMissingUsage},
		{"large content is scanned, not stored", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat(`x\"{,:`, 1<<20)+`"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}, EntryComplete},
		{"usage first, nested usage key ignored", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":1},"choices":[{"usage":{"prompt_tokens":99}}]}`)
		}, EntryComplete},
		{"oversize usage value", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"pad":"`+strings.Repeat("x", maxUsageValue)+`"}}`)
		}, EntryUnreadable},
		{"truncated json", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}`)
		}, EntryUnreadable},
		{"conflicting json usage", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1},"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		}, EntryUsageConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c, url, _ := ledgerProxy(t, tc.up, 4)
			post(t, context.Background(), url, request)
			r := seal(c)
			if r.Counts[tc.state] != 1 {
				t.Fatalf("want %s: %+v", tc.state, r)
			}
			if tc.state != EntryComplete && (r.State != "incomplete" || r.TotalComplete) {
				t.Fatalf("not incomplete: %+v", r)
			}
		})
	}
	// an error keeps its observed usage in the components, never as a complete total
	_, c, url, _ := ledgerProxy(t, cases[5].up, 4)
	post(t, context.Background(), url, request)
	if r := seal(c); r.Components["prompt"] != 9 || r.TotalComplete {
		t.Fatalf("error usage %+v", r)
	}
}

func TestLedgerDisconnectIsCancelled(t *testing.T) {
	release := make(chan struct{})
	_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	post(t, ctx, url, request)
	time.Sleep(200 * time.Millisecond)
	close(release)
	if r := seal(c); r.Counts[EntryCancelled] != 1 || r.State != "incomplete" {
		t.Fatalf("disconnect: %+v", r)
	}
}

// The old gate (Load then Add) let concurrent requests pass a limit of 1; admission is now one
// atomic decision.
func TestLedgerConcurrentCapForwardsExactlyOne(t *testing.T) {
	_, c, url, upstream := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}, 1)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := post(t, context.Background(), url, request)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for code := range codes {
		if code == 200 {
			ok++
		}
	}
	r := seal(c)
	if upstream.Load() != 1 || ok != 1 || c.Requests() != 1 || r.Counts["rejected_limit"] != 15 || r.State != "incomplete" {
		t.Fatalf("upstream %d ok %d requests %d report %+v", upstream.Load(), ok, c.Requests(), r)
	}
}

// Seal and admission race: every request is either admitted (forwarded, accounted) or late
// (never forwarded); nothing disappears.
func TestLedgerSealRacesAdmission(t *testing.T) {
	_, c, url, upstream := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}, 1000)
	var wg sync.WaitGroup
	var sent atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := post(t, context.Background(), url, request); code != 0 {
				sent.Add(1)
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	r := c.Seal(2*time.Second, BypassEnforced, ScopeIsolatedContainer)
	wg.Wait()
	r = c.Seal(0, "", "") // the frozen report
	admitted := 0
	for k, v := range r.Counts {
		if k != "late" && k != "rejected_limit" {
			admitted += v
		}
	}
	if int64(admitted) != upstream.Load() || r.Counts[EntryUnknown] != 0 {
		t.Fatalf("admitted %d upstream %d report %+v", admitted, upstream.Load(), r)
	}
	if late := c.ledger.late; int64(admitted+late) != sent.Load() {
		t.Fatalf("admitted %d + late %d != sent %d", admitted, late, sent.Load())
	}
}

// One shared drain deadline for all open requests (no per-entry timeout multiplication); the
// open ones become unknown; a later terminal does not change the frozen report.
func TestLedgerDrainDeadlineIsShared(t *testing.T) {
	release := make(chan struct{})
	_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}, 10)
	for i := 0; i < 5; i++ {
		go post(t, context.Background(), url, request)
	}
	for c.Requests() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	r := c.Seal(300*time.Millisecond, BypassEnforced, ScopeIsolatedContainer)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("drain took %v for 5 open requests", d)
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if r.Counts[EntryUnknown] != 5 || c.Seal(0, "", "").Counts[EntryUnknown] != 5 {
		t.Fatalf("drain report %+v", r)
	}
	if code, _ := post(t, context.Background(), url, request); code != http.StatusForbidden || c.ledger.late != 1 {
		t.Fatalf("after the seal: %d late %d", code, c.ledger.late)
	}
}

// Completeness needs an enforced egress boundary, an isolated scope and a verified model; usage
// alone does not make a run complete.
func TestLedgerCompletenessConditions(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}
	for _, tc := range []struct {
		bypass, scope, body, reason string
	}{
		{BypassObservedNone, ScopeIsolatedContainer, request, "bypass_not_enforced"},
		{BypassEnforced, ScopeRestricted, request, "scope_not_isolated"},
		{BypassEnforced, ScopeIsolatedContainer, `{"model":"other","messages":[]}`, "model_mismatch"},
		{BypassEnforced, ScopeIsolatedContainer, `{"messages":[]}`, "model_unverified"},
	} {
		_, c, url, _ := ledgerProxy(t, up, 4)
		post(t, context.Background(), url, tc.body)
		r := c.Seal(time.Second, tc.bypass, tc.scope)
		if r.State != "incomplete" || r.TotalComplete || !strings.Contains(strings.Join(r.Reasons, ","), tc.reason) || r.Counts[EntryComplete] != 1 {
			t.Fatalf("%s: %+v", tc.reason, r)
		}
	}
	_, c, _, _ := ledgerProxy(t, up, 4)
	if r := seal(c); r.State != "incomplete" || r.Reasons[0] != "model_not_observed" && !strings.Contains(strings.Join(r.Reasons, ","), "no_requests") {
		t.Fatalf("empty run: %+v", r)
	}
}

// C7BE-25: a usage without a known prompt or completion count is no complete total; null,
// missing and invalid values stay unknown (JSON and stream; mixed entries).
func TestLedgerMainComponentsMustBeKnown(t *testing.T) {
	for _, body := range []string{`{"usage":{"prompt_tokens":5}}`, `{"usage":{"completion_tokens":7}}`,
		`{"usage":{"prompt_tokens":5,"completion_tokens":null}}`, `{"usage":{"prompt_tokens":null,"completion_tokens":7}}`,
		`{"usage":{"prompt_tokens":5,"completion_tokens":7.5}}`, `{"usage":{"prompt_tokens":-1,"completion_tokens":7}}`,
		`{"usage":{"prompt_tokens":"5","completion_tokens":7}}`} {
		_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }, 4)
		post(t, context.Background(), url, request)
		r := seal(c)
		if r.TotalComplete || r.Counts[EntryUsagePartial] != 1 || r.ComponentsN["completion"]+r.ComponentsN["prompt"] != 1 {
			t.Errorf("%s: %+v", body, r)
		}
	}
	partial := `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":null}}` + "\n\n"
	_, c, url, _ := ledgerProxy(t, sse(partial, "data: [DONE]\n\n"), 4)
	post(t, context.Background(), url, request)
	if r := seal(c); r.TotalComplete || r.Counts[EntryUsagePartial] != 1 {
		t.Fatalf("stream: %+v", r)
	}
	// mixed: one complete and one partial exchange; the components keep both, the total is not complete
	n := 0
	_, c, url, _ = ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":null}}}`)
		} else {
			io.WriteString(w, `{"usage":{"prompt_tokens":3}}`)
		}
	}, 4)
	post(t, context.Background(), url, request)
	post(t, context.Background(), url, request)
	if r := seal(c); r.TotalComplete || r.Counts[EntryComplete] != 1 || r.Components["prompt"] != 8 || r.ComponentsN["cached"] != 0 {
		t.Fatalf("mixed: %+v", r)
	}
}

// C7BE-26: only one valid JSON document counts; the scanner checks the whole grammar.
func TestLedgerJSONGrammar(t *testing.T) {
	good := `{"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	for body, want := range map[string]string{
		`{"usage":{"prompt_tokens":5,"completion_tokens":7},"x":INVALID}`: EntryUnreadable,
		good + ` trailing`: EntryUnreadable,
		good + ` ` + good:  EntryUnreadable,
		`{"usage":{"prompt_tokens":5,"completion_tokens":7},}`:                      EntryUnreadable,
		`{"usage":{"prompt_tokens":5,"completion_tokens":7},"s":"a` + "\x01" + `"}`: EntryUnreadable,
		`{"usage":{"prompt_tokens":5,"completion_tokens":7},"n":01}`:                EntryUnreadable,
		`{"usage":{"prompt_tokens":5,"completion_tokens":7},"e":"\q"}`:              EntryUnreadable,
		`{"\u0075sage":{"prompt_tokens":5,"completion_tokens":7}}`:                  EntryUnreadable,
		`{"usage":7}`:    EntryUnreadable,
		`[` + good + `]`: EntryMissingUsage,
		`{"a":[1,-2.5e+3,true,false,null,{"usage":{"prompt_tokens":1}}],"usage":null}`: EntryMissingUsage,
		"\n " + good + " \n": EntryComplete,
		`{"x":{"y":"z\"}\\"},"usage":{"prompt_tokens":5,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":2}}}`: EntryComplete,
		good[:len(good)-1] + `,"usage":{"prompt_tokens":5,"completion_tokens":7}}`:                                           EntryComplete, // an equal duplicate
	} {
		_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }, 4)
		post(t, context.Background(), url, request)
		if r := seal(c); r.Counts[want] != 1 || (want != EntryComplete) == r.TotalComplete {
			t.Errorf("%s: want %s, %+v", body, want, r)
		}
	}
	// many duplicate usage members keep bounded state: only the first parsed value and a flag
	var j jsonUsage
	j.feed([]byte(`{`))
	for i := 0; i < 1000; i++ {
		j.feed([]byte(`"usage":{"prompt_tokens":1,"completion_tokens":1},`))
	}
	j.feed([]byte(`"z":0}`))
	if st, _ := j.result(); st != EntryComplete || cap(j.value) > 2*maxUsageValue {
		t.Fatalf("duplicates: %s cap %d", st, cap(j.value))
	}
}

// C7BE-26: a skipped, undecodable or unusable data line before [DONE] makes the stream
// unreadable even when a later line holds a valid usage.
func TestLedgerStreamUnreadableLines(t *testing.T) {
	for name, first := range map[string]string{
		"oversize line": "data: {\"pad\":\"" + strings.Repeat("x", maxSSELine) + "\"}\n\n",
		"undecodable":   "data: {not json}\n\n",
		"usage number":  "data: {\"usage\":5}\n\n",
	} {
		_, c, url, _ := ledgerProxy(t, sse(first, usage, "data: [DONE]\n\n"), 4)
		post(t, context.Background(), url, request)
		if r := seal(c); r.Counts[EntryUnreadable] != 1 || r.TotalComplete {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// comments and event lines are not data; a usage:null chunk is no usage
	_, c, url, _ := ledgerProxy(t, sse(": keep-alive\n", "event: message\n", "data: {\"usage\":null}\n\n", usage, "data: [DONE]\n\n"), 4)
	post(t, context.Background(), url, request)
	if r := seal(c); !r.TotalComplete {
		t.Fatalf("control: %+v", r)
	}
}

// C7BE-27: a request after the seal is refused and stays in the account: the next report is
// incomplete with a higher revision; earlier reports are separate values; concurrent seals
// finalize once.
func TestLedgerLateAfterSealStaysInTheAccount(t *testing.T) {
	_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	}, 4)
	if c.Report() != nil {
		t.Fatal("a report before the seal")
	}
	post(t, context.Background(), url, request)
	var wg sync.WaitGroup
	reports := make([]*LedgerReport, 8)
	for i := range reports {
		wg.Add(1)
		go func(i int) { defer wg.Done(); reports[i] = seal(c) }(i)
	}
	wg.Wait()
	for _, r := range reports {
		if !r.TotalComplete || r.Revision != 0 || r.Counts[EntryComplete] != 1 {
			t.Fatalf("concurrent seals: %+v", r)
		}
	}
	first := reports[0]
	if code, _ := post(t, context.Background(), url, request); code != http.StatusForbidden {
		t.Fatalf("after the seal: %d", code)
	}
	after := c.Report()
	if after.TotalComplete || after.Counts["late"] != 1 || after.Revision != 1 || !strings.Contains(strings.Join(after.Reasons, ","), "late_requests") {
		t.Fatalf("late request disappeared: %+v", after)
	}
	if again := seal(c); again.Revision != 1 || again.TotalComplete {
		t.Fatalf("a later seal: %+v", again)
	}
	if !first.TotalComplete || first.Counts["late"] != 0 {
		t.Fatalf("an earlier report value changed: %+v", first)
	}
}

// The scanner accepts exactly what encoding/json accepts, on every one-byte deletion, insertion
// and replacement of a few seed documents.
func TestLedgerScannerMatchesEncodingJSON(t *testing.T) {
	seeds := []string{`{"usage":{"prompt_tokens":5,"completion_tokens":7}}`, `[1,-0.5e+3,"a\"\\é",true,false,null,{}]`,
		`{"a":{"b":[[],{}]},"c":"x"}`, `-12.5E-3`, `"s"`, `0`}
	alphabet := []byte(`{}[]",:\-+.0123456789eEtfnulrsa ` + "\n\x01")
	check := func(doc []byte) {
		var s scanner
		for _, c := range doc {
			s.step(c)
		}
		if got, want := s.finish(), json.Valid(doc); got != want {
			t.Fatalf("%q: scanner %v, encoding/json %v", doc, got, want)
		}
	}
	n := 0
	for _, seed := range seeds {
		b := []byte(seed)
		for i := 0; i <= len(b); i++ {
			if i < len(b) {
				check(append(append([]byte{}, b[:i]...), b[i+1:]...))
				n++
			}
			for _, c := range alphabet {
				check(append(append(append([]byte{}, b[:i]...), c), b[i:]...))
				if i < len(b) {
					r := append([]byte{}, b...)
					r[i] = c
					check(r)
				}
				n += 2
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d cases", n)
	}
}

// C7BE-28: a recognized usage key must be exact and unique (also inside the details blocks);
// duplicates and case aliases make the usage unusable, in JSON and in a stream. Other keys are
// ignored.
func TestLedgerUsageMembersAreUnambiguous(t *testing.T) {
	for _, u := range []string{
		`{"prompt_tokens":5,"prompt_tokens":500,"completion_tokens":7}`,
		`{"prompt_tokens":5,"PROMPT_TOKENS":500,"completion_tokens":7}`,
		`{"prompt_tokens":5,"completion_tokens":7,"completion_tokens":null,"completion_tokens":70}`,
		`{"prompt_tokens":5,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":1,"cached_tokens":2}}`,
		`{"prompt_tokens":5,"completion_tokens":7,"completion_tokens_details":{"Reasoning_Tokens":2}}`,
		`{"prompt_tokens":5,"completion_tokens":7,"prompt_tokens_details":{},"Prompt_Tokens_Details":{}}`,
	} {
		_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"usage":`+u+`}`) }, 4)
		post(t, context.Background(), url, request)
		if r := seal(c); r.TotalComplete || r.Counts[EntryUnreadable] != 1 {
			t.Errorf("json %s: %+v", u, r)
		}
		_, c, url, _ = ledgerProxy(t, sse(`data: {"usage":`+u+"}\n\n", "data: [DONE]\n\n"), 4)
		post(t, context.Background(), url, request)
		if r := seal(c); r.TotalComplete || r.Counts[EntryUnreadable] != 1 {
			t.Errorf("stream %s: %+v", u, r)
		}
	}
	// unrelated keys, null details and non-object details do not block a complete usage
	ok := `{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"prompt_tokens_details":null,"completion_tokens_details":3,"extra":{"prompt_tokens":9}}`
	_, c, url, _ := ledgerProxy(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"usage":`+ok+`}`) }, 4)
	post(t, context.Background(), url, request)
	if r := seal(c); !r.TotalComplete || r.Components["prompt"] != 5 || r.ComponentsN["cached"] != 0 {
		t.Fatalf("control: %+v", r)
	}
}
