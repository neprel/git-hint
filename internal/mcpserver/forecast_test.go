package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/testkit"
)

func TestRecommendForecastPolicyMapsExistingQueryWithoutDefaults(t *testing.T) {
	srv := testkit.NewServer()
	defer srv.Close()
	shop, _ := project_(t, "--server", srv.URL, "--mcp-upload", "allow")
	cs := session(t, mcpserver.Options{Dir: shop})
	mustCall(t, cs, "hint_collect", map[string]any{})
	for _, args := range []map[string]any{{}, {"forecast_horizon_tasks": 1},
		{"forecast_horizon_tasks": 100000, "forecast_threshold": "1.000000",
			"assumed_cost_tokens_lower": 0, "assumed_cost_tokens_upper": int64(1000000000000)}} {
		got := mustCall(t, cs, "hint_recommend", args)
		if got["status"] != "sent" {
			t.Fatalf("%v: %v", args, got)
		}
	}
	if len(srv.Queries) != 3 {
		t.Fatalf("queries: %v", srv.Queries)
	}
	if len(srv.Queries[0]) != 1 || srv.Queries[0].Get("project_namespace") == "" {
		t.Fatalf("implicit policy: %v", srv.Queries[0])
	}
	if len(srv.Queries[1]) != 2 || srv.Queries[1].Get("forecast_horizon_tasks") != "1" {
		t.Fatalf("implicit threshold/cost: %v", srv.Queries[1])
	}
	q := srv.Queries[2]
	for key, want := range map[string]string{"forecast_horizon_tasks": "100000", "forecast_threshold": "1.000000",
		"assumed_cost_tokens_lower": "0", "assumed_cost_tokens_upper": "1000000000000"} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if len(q) != 5 {
		t.Fatalf("unexpected policy keys: %v", q)
	}
}

func TestRecommendForecastPolicyInvalidBeforeAnyHTTPRequest(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	shop, _ := project_(t, "--server", srv.URL, "--mcp-upload", "allow")
	cs := session(t, mcpserver.Options{Dir: shop})
	mustCall(t, cs, "hint_collect", map[string]any{})
	bad := []map[string]any{
		{"forecast_threshold": "0.9"}, {"assumed_cost_tokens_lower": 1, "assumed_cost_tokens_upper": 2},
		{"forecast_horizon_tasks": 0}, {"forecast_horizon_tasks": 100001}, {"forecast_horizon_tasks": -1},
		{"forecast_horizon_tasks": 1.5}, {"forecast_horizon_tasks": "1"}, {"forecast_horizon_tasks": 1, "forecast_threshold": 0.9},
		{"forecast_horizon_tasks": 1, "assumed_cost_tokens_lower": 0}, {"forecast_horizon_tasks": 1, "assumed_cost_tokens_upper": 1},
		{"forecast_horizon_tasks": 1, "assumed_cost_tokens_lower": 2, "assumed_cost_tokens_upper": 1},
		{"forecast_horizon_tasks": 1, "assumed_cost_tokens_lower": -1, "assumed_cost_tokens_upper": 1},
		{"forecast_horizon_tasks": 1, "assumed_cost_tokens_lower": 0, "assumed_cost_tokens_upper": int64(1000000000001)},
		{"forecast_horizon_tasks": 1, "producer_evidence": true},
	}
	for _, threshold := range []string{"0", "0.000000", "0.1234567", "1.000001", "1e-2", "", "NaN"} {
		bad = append(bad, map[string]any{"forecast_horizon_tasks": 1, "forecast_threshold": threshold})
	}
	for _, key := range []string{"forecast_horizon_tasks", "forecast_threshold", "assumed_cost_tokens_lower", "assumed_cost_tokens_upper"} {
		bad = append(bad, map[string]any{key: nil})
	}
	for _, args := range bad {
		if _, errText := call(t, cs, "hint_recommend", args); errText == "" {
			t.Errorf("invalid policy accepted: %v", args)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid policy caused %d HTTP requests", requests.Load())
	}
}

func TestRecommendForecastPolicyDoesNotGrantUploadConsent(t *testing.T) {
	srv := testkit.NewServer()
	defer srv.Close()
	shop, _ := project_(t, "--server", srv.URL)
	cs := session(t, mcpserver.Options{Dir: shop})
	mustCall(t, cs, "hint_collect", map[string]any{})
	got := mustCall(t, cs, "hint_recommend", map[string]any{"forecast_horizon_tasks": 10})
	if got["status"] != "approval_required" || got["side_effect"] != "none" || len(srv.Posted()) != 0 || len(srv.Queries) != 0 {
		t.Fatalf("forecast policy bypassed consent: %v", got)
	}
	shop2, _ := project_(t, "--mcp-upload", "allow")
	t.Setenv("HINT_SERVER_URL", srv.URL)
	cs2 := session(t, mcpserver.Options{Dir: shop2})
	mustCall(t, cs2, "hint_collect", map[string]any{})
	if _, message := call(t, cs2, "hint_recommend", map[string]any{"forecast_horizon_tasks": 10}); !strings.Contains(message, "server_unconfigured") {
		t.Fatalf("forecast policy used environment server: %q", message)
	}
	if len(srv.Posted()) != 0 || len(srv.Queries) != 0 {
		t.Fatal("forecast request reached an unconfigured server")
	}
}

func TestRecommendForecastPolicySchemaUsesOptionalSingleTypes(t *testing.T) {
	shop, _ := project_(t)
	cs := session(t, mcpserver.Options{Dir: shop})
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name != "hint_recommend" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		if len(properties) != 4 {
			t.Fatalf("unexpected arguments: %s", raw)
		}
		for key, want := range map[string]string{"forecast_horizon_tasks": "integer", "forecast_threshold": "string",
			"assumed_cost_tokens_lower": "integer", "assumed_cost_tokens_upper": "integer"} {
			property := properties[key].(map[string]any)
			if property["type"] != want {
				t.Errorf("%s: %v", key, property)
			}
		}
		if required, ok := schema["required"].([]any); ok && len(required) != 0 {
			t.Errorf("policy gained required arguments: %v", required)
		}
		return
	}
	t.Fatal("hint_recommend missing")
}
