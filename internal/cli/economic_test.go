package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func economicFixtureBytes(t *testing.T) []byte {
	t.Helper()
	raw, e := os.ReadFile("../api/testdata/economic-sidecar.synthetic.json")
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestEconomicRecommendReceivedNoStoreOrPlan(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "received.json")
	if e := os.WriteFile(file, []byte(strings.ReplaceAll(string(economicFixtureBytes(t)), `"code":{}`, `"code":{"synthetic":"HINT_RAW_PROVENANCE_SENTINEL"}`)), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"recommend", "--received", file, "--project-namespace", "11111111111111111111111111111111", "--economic-horizon", "10", "--economic-threshold", "0.95"}
	r := must(t, run(t, dir, "", args...))
	for _, text := range []string{"COLLECT_MORE_EVIDENCE (product_ready=false)", "canonical full-task tokens=37", "coverage=1/1", "attempts=2", "10 accepted completions", "conservative_cost_bounds", "whole_project_cost_population_missing", "Whole-project cost, effect, savings, and a winner remain unknown"} {
		if !strings.Contains(r.stdout, text) {
			t.Error("missing", text, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "HINT_RAW_PROVENANCE_SENTINEL") {
		t.Fatal("opaque provenance rendered")
	}
	files := listFiles(t, dir)
	if len(files) != 1 {
		t.Fatal("offline renderer wrote state", files)
	}
	unknown := strings.ReplaceAll(string(economicFixtureBytes(t)), `"canonical_tokens":37`, `"canonical_tokens":null`)
	unknown = strings.ReplaceAll(unknown, `"counter_semantics":"hint-task-tokens-v1"`, `"counter_semantics":"unknown"`)
	unknown = strings.ReplaceAll(unknown, `"coverage":[1,1]`, `"coverage":[0,1]`)
	os.WriteFile(file, []byte(unknown), 0600)
	if r := must(t, run(t, dir, "", args...)); !strings.Contains(r.stdout, "canonical full-task tokens=unknown; coverage=0/1") {
		t.Fatal(r.stdout)
	}
	for _, extra := range [][]string{{"--yes"}, {"--server", "http://127.0.0.1:1"}, {"--retry"}, {"--forecast-horizon", "10"}, {"--assumed-cost", "1:2"}} {
		if r := run(t, dir, "", append(append([]string{}, args...), extra...)...); r.code == 0 {
			t.Fatal("mixed received flags accepted", extra)
		}
	}
	wrong := append([]string{}, args...)
	wrong[4] = "44444444444444444444444444444444"
	if r := run(t, dir, "", wrong...); r.code == 0 {
		t.Fatal("wrong namespace")
	}
}
func TestEconomicRecommendUsesNormalConsentUploadAndNoPlanCache(t *testing.T) {
	shop, _ := setup(t)
	s, e := store.Open(shop)
	if e != nil {
		t.Fatal(e)
	}
	backend := testkit.NewServer()
	defer backend.Close()
	raw := strings.ReplaceAll(string(economicFixtureBytes(t)), "11111111111111111111111111111111", s.Cfg.ProjectNamespace)
	var gets, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets.Add(1)
			q := r.URL.Query()
			if q.Get("economic_horizon") != "10" || q.Get("economic_threshold") != "0.95" || q.Get("economic_version") != "economic-policy-v1" || q.Get("economic_objective") != "canonical_model_tokens" || q.Get("economic_horizon_unit") != "accepted_completion" || q.Get("economic_risk") != "conservative_cost_bounds" || q.Get("economic_correctness_guard") != "paired_accepted_fraction_lower_bound_nonnegative" || len(q) != 8 {
				t.Error(q)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, raw)
			return
		}
		posts.Add(1)
		backend.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	args := []string{"recommend", "--server", srv.URL, "--economic-horizon", "10", "--economic-threshold", "0.95"}
	if r := run(t, shop, "", args...); r.code == 0 || posts.Load() != 0 || gets.Load() != 0 {
		t.Fatal("approval bypass", r)
	}
	r := must(t, run(t, shop, "", append(args, "--yes")...))
	if gets.Load() != 1 || posts.Load() == 0 || !strings.Contains(r.stdout, "canonical full-task tokens=37") {
		t.Fatal(r, gets.Load(), posts.Load())
	}
	if _, e := project.LatestRecommendation(s); e == nil {
		t.Fatal("sidecar saved as recommendation")
	}
}
func TestEconomicRecommendInvalidFlagsBeforeStore(t *testing.T) {
	for _, args := range [][]string{{"--economic-horizon", "10"}, {"--economic-threshold", "0.95"}, {"--economic-horizon", "0", "--economic-threshold", "0.95"}, {"--economic-horizon", "10", "--economic-threshold", "NaN"}, {"--economic-horizon", "10", "--economic-threshold", "0.95", "--forecast-horizon", "10"}, {"--economic-horizon", "10", "--economic-threshold", "0.95", "--assumed-cost", "1:2"}, {"--received", "file.json"}, {"--project-namespace", "11111111111111111111111111111111"}} {
		r := run(t, t.TempDir(), "", append([]string{"recommend"}, args...)...)
		if r.code == 0 || strings.Contains(r.stderr, "not initialized") {
			t.Fatal(args, r)
		}
	}
}

func TestEconomicReceivedUnknownAttemptInventories(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "received.json")
	for _, tc := range []struct{ coverage, attempts, want string }{
		{`[1,3]`, `null`, "coverage=1/3; attempts=unknown"},
		{`[0,3]`, `null`, "coverage=0/3; attempts=unknown"},
		{`[0,3]`, `3`, "coverage=0/3; attempts=3"},
		{`[1,2]`, `null`, "coverage=1/2; attempts=unknown"},
		{`[1,2]`, `2`, "coverage=1/2; attempts=2"},
	} {
		raw := strings.ReplaceAll(string(economicFixtureBytes(t)), `"canonical_tokens":37`, `"canonical_tokens":null`)
		raw = strings.ReplaceAll(raw, `"counter_semantics":"hint-task-tokens-v1"`, `"counter_semantics":"unknown"`)
		raw = strings.ReplaceAll(raw, `"coverage":[1,1]`, `"coverage":`+tc.coverage)
		raw = strings.ReplaceAll(raw, `"attempts":2`, `"attempts":`+tc.attempts)
		if e := os.WriteFile(file, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		r := must(t, run(t, dir, "", "recommend", "--received", file, "--project-namespace", "11111111111111111111111111111111", "--economic-horizon", "10", "--economic-threshold", "0.95"))
		if !strings.Contains(r.stdout, "canonical full-task tokens=unknown") || !strings.Contains(r.stdout, tc.want) {
			t.Fatal(r.stdout)
		}
	}
}

func TestEconomicEmptyRetryFetchesOnlyAndLegacyStaysIdle(t *testing.T) {
	shop, _ := setup(t)
	s, e := store.Open(shop)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("HINT_SERVER_TOKEN", "synthetic-existing-namespace-token")
	var gets, posts atomic.Int32
	raw := strings.ReplaceAll(string(economicFixtureBytes(t)), "11111111111111111111111111111111", s.Cfg.ProjectNamespace)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts.Add(1)
			w.WriteHeader(400)
			return
		}
		gets.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-existing-namespace-token" || r.URL.Query().Get("project_namespace") != s.Cfg.ProjectNamespace || r.URL.Query().Get("economic_horizon") != "10" {
			t.Error("request binding")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, raw)
	}))
	defer srv.Close()
	legacy := must(t, run(t, shop, "", "recommend", "--retry", "--server", srv.URL))
	if !strings.Contains(legacy.stdout, "No queued packages.") || gets.Load() != 0 || posts.Load() != 0 {
		t.Fatal("legacy empty retry changed", legacy)
	}
	r := must(t, run(t, shop, "", "recommend", "--retry", "--server", srv.URL, "--economic-horizon", "10", "--economic-threshold", "0.95"))
	if gets.Load() != 1 || posts.Load() != 0 || !strings.Contains(r.stdout, "canonical full-task tokens=37") || strings.Contains(r.stdout, "Send ") || strings.Contains(r.stdout, "Sent ") {
		t.Fatal("economic empty retry uploads or requests send consent", r, gets.Load(), posts.Load())
	}
	if _, e := project.LatestRecommendation(s); e == nil {
		t.Fatal("sidecar saved as plan")
	}
}
