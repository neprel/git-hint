package runner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testClient opens one connection per request: pooled keep-alive connections can leave an
// accepted connection without a request, which a server shutdown waits for up to 5 s.
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func TestModelProxy(t *testing.T) {
	var upstream atomic.Int64
	var gotAuth atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		gotAuth.Store(r.Header.Get("Authorization") + " " + r.URL.Path)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := StartLive(LiveConfig{BaseURL: up.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	lv.ContainerHost = "127.0.0.1" // the test reaches the proxy directly
	c := lv.NewTrial(2)
	base := lv.BaseURL(c)
	post := func(u string) int {
		resp, err := testClient.Post(u, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(base + "/chat/completions"); code != 200 {
		t.Fatalf("first request %d", code)
	}
	if got := gotAuth.Load().(string); got != "Bearer lan-secret-word /v1/chat/completions" {
		t.Fatalf("upstream saw %q", got)
	}
	post(base + "/chat/completions")
	if code := post(base + "/chat/completions"); code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", code)
	}
	if c.Requests() != 2 || !c.LimitHit() || c.Retries() != 1 || upstream.Load() != 2 {
		t.Fatalf("counter %d hit %v retries %d upstream %d", c.Requests(), c.LimitHit(), c.Retries(), upstream.Load())
	}
	resp, _ := testClient.Get(base + "/models")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "local-coder") || upstream.Load() != 2 {
		t.Fatalf("model list %s (upstream %d)", b, upstream.Load())
	}
	if code := post(strings.Replace(base, c.token, "unknown", 1) + "/chat/completions"); code != http.StatusForbidden {
		t.Fatalf("unknown trial %d", code)
	}
	for _, v := range append(lv.AgentArgs(c), lv.AgentEnv(c)...) {
		if strings.Contains(v, "lan-secret-word") {
			t.Fatal("the key reached the runner arguments or environment")
		}
	}
	if _, err := StartLive(LiveConfig{BaseURL: up.URL + "/v1", Model: "m", KeyEnv: "HINT_TEST_NO_SUCH_KEY"}); err == nil {
		t.Fatal("a proxy without a key started")
	}
}

func TestProxyChecksTheModelOfEveryRequest(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := StartLive(LiveConfig{BaseURL: up.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	lv.ContainerHost = "127.0.0.1"
	post := func(c *Counter, body string) {
		resp, err := testClient.Post(lv.BaseURL(c)+"/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	ok := `{"model":"local-coder","messages":[{"role":"user","content":"x"}]}`
	cases := []struct {
		bodies []string
		want   string
	}{
		{nil, "not_observed"},
		{[]string{ok, ok}, "verified"},
		{[]string{ok, `{"model":"other-model","messages":[]}`}, "mismatch"}, // a runtime switch or subagent
		{[]string{ok, `not json`}, "unverified"},
		{[]string{`{"messages":[]}`}, "unverified"}, // no model field
	}
	for _, tc := range cases {
		c := lv.NewTrial(10)
		for _, b := range tc.bodies {
			post(c, b)
		}
		if got := c.ModelCheck(); got != tc.want {
			t.Fatalf("%v: %s, want %s", tc.bodies, got, tc.want)
		}
	}
	// the upstream receives every body unchanged
	if bodies[0] != ok || bodies[len(bodies)-1] != `{"messages":[]}` {
		t.Fatalf("forwarded bodies changed: %q", bodies)
	}
}

func TestLaunchDigestIgnoresPerTrialValues(t *testing.T) {
	a := RunOptions{Agent: "opencode", AgentArgs: []string{"version=2.0.26"}, Model: "openai/local-coder",
		Env: []string{"OPENAI_BASE_URL=http://h:1/t/aaa", "OPENAI_API_KEY=hint-proxy"}}
	b := a
	b.Env = []string{"OPENAI_API_KEY=other", "OPENAI_BASE_URL=http://h:1/t/bbb"}
	c := a
	c.Model = "openai/other"
	if LaunchDigest(a) != LaunchDigest(b) || LaunchDigest(a) == LaunchDigest(c) || !strings.HasPrefix(LaunchDigest(a), "sha256:") {
		t.Fatal("the launch digest depends on per-trial values or ignores the model")
	}
}


func TestProxyRefusesAmbiguousModelFields(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	t.Setenv("HINT_TEST_LAN_KEY", "lan-secret-word")
	lv, err := StartLive(LiveConfig{BaseURL: up.URL + "/v1", Model: "expected", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	lv.ContainerHost = "127.0.0.1"
	post := func(c *Counter, body string) {
		resp, err := testClient.Post(lv.BaseURL(c)+"/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	ok := `{"model":"expected","messages":[]}`
	for _, tc := range []struct {
		bodies []string
		want   string
	}{
		{[]string{`{"model":"other","Model":"expected"}`}, "unverified"}, // C7BE-3: Go alias, upstream sees "other"
		{[]string{`{"Model":"expected","model":"other"}`}, "unverified"},
		{[]string{`{"MODEL":"expected"}`}, "unverified"},
		{[]string{`{"model":"expected","model":"expected"}`}, "unverified"}, // a duplicate key
		{[]string{`{"model":"expected","model":"other"}`}, "unverified"},
		{[]string{`{"model":7}`}, "unverified"},
		{[]string{`{"model":null}`}, "unverified"},     // null is no selection (not another model)
		{[]string{ok, `{"model":null}`}, "unverified"}, // verified + null: never verified, no mismatch
		{[]string{ok, `{"model":null}`, `{"model":"other"}`}, "mismatch"},
		{[]string{`[{"model":"expected"}]`}, "unverified"},
		{[]string{`{"model":"expected"} {"model":"other"}`}, "unverified"}, // trailing data
		{[]string{`{"messages":[{"model":"other"}],"model":"expected"}`}, "verified"}, // nested keys are not the selection
		{[]string{ok, `{"model":"other","Model":"expected"}`, ok}, "unverified"}, // mixed: never verified
		{[]string{ok, `{"model":"other"}`, `{"Model":"expected"}`}, "mismatch"},
	} {
		c := lv.NewTrial(20)
		for _, b := range tc.bodies {
			post(c, b)
		}
		if got := c.ModelCheck(); got != tc.want {
			t.Fatalf("%v: %s, want %s", tc.bodies, got, tc.want)
		}
	}
	if bodies[0] != `{"model":"other","Model":"expected"}` {
		t.Fatalf("the forwarded body changed: %q", bodies[0])
	}
}
