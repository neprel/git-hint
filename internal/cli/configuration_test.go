package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

func TestAdapterDeclaredFamilyAndConfiguration(t *testing.T) {
	shop, _ := setup(t)
	s, _ := store.Open(shop)
	cfgDigest := "sha256:" + strings.Repeat("c", 64)
	t.Setenv("HINT_TASK_ID", "HINTCANARY_TASK_7")
	t.Setenv("HINT_TASK_FAMILY", "HINTCANARY_FAMILY_billing")
	t.Setenv("HINT_MODEL", "HINTCANARY_MODEL_local")
	t.Setenv("HINT_CONFIG_REFS", "setting_override:"+cfgDigest)
	must(t, run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop, "--agent-version", "2.0.26"))
	obs, _, _ := project.ReadSpool(s)
	srcs, _ := project.Sources(s)
	fam := activity.FamilyRef(s.Key(), "HINTCANARY_FAMILY_billing")
	for _, o := range obs {
		if o.Correlation["task_family_id"] != fam {
			t.Fatalf("record without the declared family: %+v", o)
		}
	}
	src := srcs[obs[0].SourceID]
	c := src.Configuration
	if c == nil || !slices.Contains(src.Capabilities, "correlation.task_family_id") {
		t.Fatalf("source: %+v", src)
	}
	// the effective model is not reported by the plugin: unknown, the declared one is provenance
	if c.Model.State != "unknown" || c.Declared.Model.Ref != activity.ModelRef(s.Key(), "HINTCANARY_MODEL_local") {
		t.Fatalf("model: %+v %+v", c.Model, c.Declared)
	}
	// a direct run's limits are unknown to HINT: never inferred as unbounded
	if c.Budget.MaxRequests.State != "unknown" || c.Budget.MaxSeconds.State != "unknown" {
		t.Fatalf("budget: %+v", c.Budget)
	}
	if c.Harness.Kind != "direct" || c.Harness.Version.State != "not_applicable" || c.Agent.Version.Value != "2.0.26" {
		t.Fatalf("harness/agent: %+v", c)
	}
	if c.ConfigRefs.State != "known" || len(c.ConfigRefs.Refs) != 1 || c.ConfigRefs.Refs[0] != activity.ConfigRef(s.Key(), "setting_override", cfgDigest) {
		t.Fatalf("config refs: %+v", c.ConfigRefs)
	}
	// the export is v0.2 with the configuration and the family; raw labels never leave
	out := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out))
	all := readAll(t, out)
	noCanary(t, "export", all, shop, "HINTCANARY_TASK_7", "HINTCANARY_FAMILY_billing", "HINTCANARY_MODEL_local")
	var act map[string]any
	files, _ := filepath.Glob(filepath.Join(out, "*.json"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var d map[string]any
		if json.Unmarshal(b, &d) == nil && d["document_type"] == "activity_stream" {
			act = d
		}
	}
	if act == nil || act["hint_version"] != "0.2.0" || !strings.Contains(string(all), `"task_family_id"`) || !strings.Contains(string(all), `"configuration"`) {
		t.Fatalf("export: %v", act["hint_version"])
	}
}

func TestAdapterRefusesBadDeclarationsBeforeWriting(t *testing.T) {
	shop, _ := setup(t)
	s, _ := store.Open(shop)
	for _, env := range []map[string]string{
		{"HINT_TASK_FAMILY": "fam"}, // a family without a task
		{"HINT_TASK_ID": "t1", "HINT_TASK_FAMILY": "bad\x01family"},
		{"HINT_TASK_ID": "t1", "HINT_CONFIG_REFS": "secret_file:sha256:" + strings.Repeat("a", 64)},
		{"HINT_TASK_ID": "t1", "HINT_MODEL": strings.Repeat("m", 201)},
	} {
		for _, k := range []string{"HINT_TASK_ID", "HINT_TASK_FAMILY", "HINT_MODEL", "HINT_CONFIG_REFS"} {
			t.Setenv(k, env[k])
		}
		if r := run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop); r.code == 0 || !strings.Contains(r.stderr, "nothing was written") {
			t.Fatalf("%v: %+v", env, r)
		}
		if obs, _, _ := project.ReadSpool(s); len(obs) != 0 {
			t.Fatalf("%v: records written", env)
		}
	}
}

func TestLegacySourceIdentityIsUnchangedWithoutDeclarations(t *testing.T) {
	key := []byte("k")
	got := activity.OpenCodeSource(key, activity.SourceOptions{AgentVersion: "2.0.26", TaskDeclared: true})
	if got.SourceID != hint.DeriveID(key, "source", "opencode-plugin", "2.0.26", activity.Version, "declared-task") || got.Configuration != nil {
		t.Fatalf("legacy identity changed: %+v", got)
	}
	a := activity.Configuration{}
	a.Agent.Name, a.Harness.Kind = "opencode", "direct"
	b := a
	b.Declared = &activity.Declared{Model: &activity.RefState{State: "known", Ref: "x"}}
	if a.Digest() != b.Digest() {
		t.Fatal("declared settings changed the effective identity")
	}
	c := a
	c.Model = activity.ModelState{State: "known", Ref: "y", Provenance: "launch_parameter"}
	if c.Digest() == a.Digest() || activity.OpenCodeSource(key, activity.SourceOptions{Configuration: &a}).SourceID == activity.OpenCodeSource(key, activity.SourceOptions{Configuration: &c}).SourceID {
		t.Fatal("a changed effective configuration kept the identity")
	}
}

func TestKnownEmptyConfigRefsStayKnown(t *testing.T) {
	b, _ := json.Marshal(activity.ConfigRefs{State: "known"})
	u, _ := json.Marshal(activity.ConfigRefs{State: "unknown"})
	if string(b) != `{"refs":[],"state":"known"}` || string(u) != `{"state":"unknown"}` {
		t.Fatalf("known empty: %s, unknown: %s", b, u)
	}
	shop, _ := setup(t)
	t.Setenv("HINT_TASK_ID", "t1")
	t.Setenv("HINT_CONFIG_REFS", "none")
	must(t, run(t, shop, openCodeEvents(shop), "adapter", "opencode", "--project", shop))
	must(t, run(t, shop, "", "export", "--out", t.TempDir())) // the v0.2 package validates
	must(t, run(t, shop, "", "eval", "work", "start", "u", "--family", "f"))
	must(t, run(t, shop, "", "eval", "report", "-o", filepath.Join(t.TempDir(), "r.json"))) // the report validates
}
