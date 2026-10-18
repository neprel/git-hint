package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func TestForecastFlagsMapToTheRequest(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL,
		"--forecast-horizon", "400", "--forecast-threshold", "0.95", "--assumed-cost", "150000:250000"))
	if len(srv.Queries) != 2 {
		t.Fatalf("queries: %v", srv.Queries)
	}
	for k := range srv.Queries[0] {
		if k != "project_namespace" {
			t.Errorf("a request without flags carries %s", k)
		}
	}
	q := srv.Queries[1]
	for k, want := range map[string]string{"forecast_horizon_tasks": "400", "forecast_threshold": "0.95",
		"assumed_cost_tokens_lower": "150000", "assumed_cost_tokens_upper": "250000"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestInvalidForecastFlagsAreUsageErrorsBeforeAnyNetwork(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer()
	defer srv.Close()
	for _, args := range [][]string{
		{"--forecast-threshold", "0.9"},
		{"--assumed-cost", "1:2"},
		{"--forecast-horizon", "0"},
		{"--forecast-horizon", "100001"},
		{"--forecast-horizon", "1e3"},
		{"--forecast-horizon", ""},
		{"--forecast-horizon", "5", "--forecast-threshold", "0"},
		{"--forecast-horizon", "5", "--forecast-threshold", "0.000000"},
		{"--forecast-horizon", "5", "--forecast-threshold", "1.5"},
		{"--forecast-horizon", "5", "--forecast-threshold", "0.1234567"},
		{"--forecast-horizon", "5", "--assumed-cost", "3:2"},
		{"--forecast-horizon", "5", "--assumed-cost", "5"},
		{"--forecast-horizon", "5", "--assumed-cost", "0:1000000000001"},
	} {
		r := run(t, shop, "", append([]string{"recommend", "--yes", "--server", srv.URL}, args...)...)
		if r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}
	if len(srv.Posted()) != 0 || len(srv.Queries) != 0 {
		t.Fatalf("an invalid policy reached the network")
	}
	// boundary values are accepted
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL, "--forecast-horizon", "100000", "--forecast-threshold", "1.000000",
		"--assumed-cost", "0:1000000000000"))
}

// declSetup fetches a set whose plans cover the shop repository only, then records a later
// snapshot of shop (a completed local change).
func declSetup(t *testing.T) (shop string, srv *testkit.Server, planID, otherPlan string) {
	t.Helper()
	shop, _ = setup(t)
	s, err := store.Open(shop)
	if err != nil {
		t.Fatal(err)
	}
	var shopID string
	for _, r := range s.Cfg.Repos {
		if filepath.Base(s.RepoRoot(r)) == filepath.Base(shop) {
			shopID = s.RepoID(r)
		}
	}
	srv = testkit.NewServer()
	srv.Mutate = func(doc map[string]any) {
		for _, p := range doc["payload"].(map[string]any)["plans"].([]any) {
			pm := p.(map[string]any)
			pm["repo_ids"] = []any{shopID}
			var keep []any
			for _, x := range pm["snapshots"].([]any) {
				if x.(map[string]any)["repo_id"] == shopID {
					keep = append(keep, x)
				}
			}
			pm["snapshots"] = keep
		}
	}
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	saved, err := project.LatestRecommendation(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Payload struct {
			Plans []struct {
				PlanID string `json:"plan_id"`
			} `json:"plans"`
		} `json:"payload"`
	}
	b, _ := os.ReadFile(saved)
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Payload.Plans) != 2 {
		t.Fatalf("saved set: %v", err)
	}
	change(t, shop, "func DeclaredChange() int { return 1 }\n")
	return shop, srv, doc.Payload.Plans[0].PlanID, doc.Payload.Plans[1].PlanID
}

func change(t *testing.T, shop, code string) {
	t.Helper()
	f := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
	b, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(b, []byte("\n"+code)...), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, run(t, shop, "", "collect", "--full"))
}

// changeSets returns the change_set documents of an export directory.
func changeSets(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var d map[string]any
		if json.Unmarshal(b, &d) == nil && d["document_type"] == "change_set" {
			out = append(out, d)
		}
	}
	return out
}

func TestDeclarePlanFreezesTheAssociationAndExportsANewDocument(t *testing.T) {
	shop, srv, planID, otherPlan := declSetup(t)
	defer srv.Close()
	d1 := filepath.Join(t.TempDir(), "plain")
	must(t, run(t, shop, "", "export", "--out", d1))
	plain := changeSets(t, d1)
	if len(plain) != 1 || plain[0]["payload"].(map[string]any)["plan_application"] != nil {
		t.Fatalf("plain export: %v", plain)
	}
	// changes stays read-only without the flag
	must(t, run(t, shop, "", "changes"))
	if _, err := os.Stat(filepath.Join(shop, ".hint", "state", "declarations")); err == nil {
		t.Fatal("changes without --declare-plan wrote state")
	}
	r := must(t, run(t, shop, "", "changes", "--declare-plan", planID))
	if !strings.Contains(r.stdout, "Recorded") || !strings.Contains(r.stdout, "association only") {
		t.Fatalf("declare: %s", r.stdout)
	}
	if r := must(t, run(t, shop, "", "changes", "--declare-plan", planID)); !strings.Contains(r.stdout, "Already recorded") {
		t.Fatalf("repeat: %s", r.stdout)
	}
	if r := run(t, shop, "", "changes", "--declare-plan", otherPlan); r.code != 1 || !strings.Contains(r.stderr, "different declaration") {
		t.Fatalf("a different plan for the same change must not overwrite: %+v", r)
	}
	d2 := filepath.Join(t.TempDir(), "declared")
	must(t, run(t, shop, "", "export", "--out", d2))
	declared := changeSets(t, d2)
	if len(declared) != 1 {
		t.Fatalf("declared export: %d change sets", len(declared))
	}
	cs := declared[0]
	pa, _ := cs["payload"].(map[string]any)["plan_application"].(map[string]any)
	if cs["hint_version"] != "0.2.0" || pa == nil || pa["plan_id"] != planID || pa["declaration"] != "client_declared_association" {
		t.Fatalf("declared change set: %v", cs)
	}
	if cs["document_id"] == plain[0]["document_id"] {
		t.Fatal("the declared change set reuses the plain document ID")
	}
	frozen := cs["payload"].(map[string]any)["after_snapshot"]
	// a later change: the declaration keeps its frozen snapshots; the new change is plain
	change(t, shop, "func LaterChange() int { return 2 }\n")
	d3 := filepath.Join(t.TempDir(), "later")
	must(t, run(t, shop, "", "export", "--out", d3))
	var nDeclared, nPlain int
	for _, c := range changeSets(t, d3) {
		p := c["payload"].(map[string]any)
		if p["plan_application"] != nil {
			nDeclared++
			b1, _ := json.Marshal(p["after_snapshot"])
			b2, _ := json.Marshal(frozen)
			if string(b1) != string(b2) || c["document_id"] != cs["document_id"] {
				t.Errorf("the declared change set changed after a later snapshot")
			}
		} else {
			nPlain++
		}
	}
	if nDeclared != 1 || nPlain != 1 {
		t.Fatalf("later export: %d declared, %d plain", nDeclared, nPlain)
	}
	// the server accepts the declared document
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
}

func TestDeclarePlanRefusals(t *testing.T) {
	shop, srv, planID, _ := declSetup(t)
	defer srv.Close()
	if r := run(t, shop, "", "changes", "--from", "x.json"); r.code != 2 {
		t.Errorf("--from without --declare-plan: exit %d", r.code)
	}
	if r := run(t, shop, "", "changes", "--declare-plan", "nothex"); r.code != 2 {
		t.Errorf("bad plan ID: exit %d", r.code)
	}
	if r := run(t, shop, "", "changes", "--declare-plan", strings.Repeat("0", 32)); r.code != 1 || !strings.Contains(r.stderr, "not in the recommendation set") {
		t.Errorf("unknown plan: %+v", r)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"hint_version":"0.1.0"}`), 0o600)
	if r := run(t, shop, "", "changes", "--declare-plan", planID, "--from", bad); r.code != 1 || !strings.Contains(r.stderr, "not accepted") {
		t.Errorf("invalid set file: %+v", r)
	}
}

func TestACrossRepositoryPlanIsNotDeclared(t *testing.T) {
	shop, _ := setup(t)
	srv := testkit.NewServer() // its plans span both repositories
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	s, _ := store.Open(shop)
	saved, _ := project.LatestRecommendation(s)
	b, _ := os.ReadFile(saved)
	var doc struct {
		Payload struct {
			Plans []struct {
				PlanID string `json:"plan_id"`
			} `json:"plans"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(b, &doc)
	change(t, shop, "func X() int { return 3 }\n")
	if r := run(t, shop, "", "changes", "--declare-plan", doc.Payload.Plans[0].PlanID); r.code != 1 || !strings.Contains(r.stderr, "not supported") {
		t.Fatalf("cross-repository declaration: %+v", r)
	}
}
