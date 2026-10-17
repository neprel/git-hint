package runner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
		resp, err := http.Post(u, "application/json", strings.NewReader(`{}`))
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
	resp, _ := http.Get(base + "/models")
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
