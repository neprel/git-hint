package pipeline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
)

// record maps a runner account to the wire record as WriteLedger does (synthetic key and
// references) for the checker.
func record(t *testing.T, r *runner.LedgerReport) activity.ModelLedger {
	t.Helper()
	key := []byte("k")
	m := ledgerRecord(*r)
	run := activity.RunRef(key, "trial-1")
	m.LedgerRef, m.SealedAt = activity.LedgerRef(key, run), r.SealedAt
	m.Binding = activity.LedgerBinding{RunRef: run, TaskID: hint.DeriveID(key, "task", "t"), TaskFamilyID: hint.DeriveID(key, "family", "f"),
		ConfigurationDigest: "sha256:" + strings.Repeat("a", 64), UnitRef: hint.DeriveID(key, "unit", "u"), AttemptRef: hint.DeriveID(key, "attempt", "a")}
	ledgerEntries(&m, key, *r)
	return m
}

func proxy(t *testing.T, up string, h http.HandlerFunc) (*runner.Live, *runner.Counter, string) {
	t.Helper()
	base := up
	if h != nil {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		base = srv.URL
	}
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: base + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lv.Close() })
	lv.ContainerHost = "127.0.0.1"
	c := lv.NewTrial(10)
	return lv, c, lv.BaseURL(c) + "/chat/completions"
}

func send(ctx context.Context, url string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"local-coder","messages":[]}`))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// Real proxy traffic of every producible termination (complete stream, upstream error status,
// truncated stream, a request whose agent goes away, a request open at the drain deadline):
// the produced entries pass the public checker, with exact totals including partial values.
func TestProducedEntriesPassTheChecker(t *testing.T) {
	release := make(chan struct{})
	_, c, url := proxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Test") {
		case "error":
			w.WriteHeader(503)
			io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
			return
		case "hang":
			<-release
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100}}}`+"\n\n")
		w.(http.Flusher).Flush()
		if r.Header.Get("X-Test") == "cut" {
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	defer close(release)
	do := func(kind string, ctx context.Context) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"local-coder","messages":[]}`))
		req.Header.Set("X-Test", kind)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	do("ok", context.Background())
	do("error", context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	do("cut", ctx)
	time.Sleep(100 * time.Millisecond)
	go do("hang", context.Background())
	for c.Requests() < 4 {
		time.Sleep(5 * time.Millisecond)
	}
	r := c.Seal(200*time.Millisecond, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	m := record(t, r)
	if p := activity.LedgerProblems(m); len(p) != 0 {
		t.Fatalf("the produced ledger fails the checker: %v\n%+v", p, m.Entries)
	}
	terms := map[string]string{}
	for _, e := range m.Entries {
		terms[e.Termination] = e.State
	}
	want := map[string]string{"response_ended": "complete", "error_status": "error", "seal_timeout": "unknown"}
	for k, v := range want {
		if terms[k] != v {
			t.Fatalf("terminations %v, want %v", terms, want)
		}
	}
	// the agent going away mid-stream ends the handler by the proxy's abort or by the lost
	// request context, depending on timing: both are cancellation paths, recorded as observed
	if terms["handler_abort"] != "cancelled" && terms["context_lost"] != "cancelled" {
		t.Fatalf("no cancellation entry: %v", terms)
	}
	// the totals include the error entry's and the cancelled entry's partial usage
	if *m.Components["prompt"] != 120+5+120 || m.ComponentCoverage["prompt"] != 3 {
		t.Fatalf("totals %v %v", m.Components, m.ComponentCoverage)
	}
}

func TestProducedProxyErrorAndEmptyLedgersPassTheChecker(t *testing.T) {
	_, c, url := proxy(t, "http://127.0.0.1:1", nil)
	send(context.Background(), url)
	m := record(t, c.Seal(time.Second, runner.BypassEnforced, runner.ScopeIsolatedContainer))
	if p := activity.LedgerProblems(m); len(p) != 0 || m.Entries[0].Termination != "proxy_error" {
		t.Fatalf("proxy error ledger: %v %+v", p, m.Entries)
	}
	_, empty, _ := proxy(t, "http://127.0.0.1:1", nil)
	e := record(t, empty.Seal(0, runner.BypassEnforced, runner.ScopeIsolatedContainer))
	if p := activity.LedgerProblems(e); len(p) != 0 || e.Entries != nil || e.SealedMs != nil || e.AnchorUTC != "" {
		t.Fatalf("an empty ledger must be aggregate-only and valid: %v %+v", p, e)
	}
}

// The account's agent_run is copied to the record as recorded: over real proxy traffic an
// enclosure from the anchor to the seal passes the public checker, the same outcome without
// entries carries no offsets, and a refusal before the launch with an admitted request is
// refused by the checker (the mapping never repairs a record).
func TestProducedAgentRunPassesTheChecker(t *testing.T) {
	_, c, url := proxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	send(context.Background(), url)
	r := c.Seal(time.Second, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	zero, code := int64(0), 0
	digest := "sha256:" + strings.Repeat("b", 64)
	r.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentExited, ExitCode: &code, EnclosureStartMs: &zero, EnclosureEndMs: r.SealedMs, ExecutionDigest: digest}
	m := record(t, r)
	if p := activity.LedgerProblems(m); len(p) != 0 || m.AgentRun == nil || *m.AgentRun.EnclosureEndMs != *m.SealedMs {
		t.Fatalf("the produced agent_run fails the checker: %v %+v", p, m.AgentRun)
	}
	r.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentRefusedBeforeLaunch, ExecutionDigest: digest}
	if p := activity.LedgerProblems(record(t, r)); len(p) == 0 {
		t.Fatal("a refusal before the launch with an admitted request passed")
	}
	_, empty, _ := proxy(t, "http://127.0.0.1:1", nil)
	e := empty.Seal(0, runner.BypassEnforced, runner.ScopeIsolatedContainer)
	e.AgentRun = &runner.ReportAgentRun{Outcome: runner.AgentRefusedBeforeLaunch, ExecutionDigest: digest}
	if p := activity.LedgerProblems(record(t, e)); len(p) != 0 {
		t.Fatalf("a refusal before the launch without requests is refused: %v", p)
	}
}
