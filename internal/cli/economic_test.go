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
