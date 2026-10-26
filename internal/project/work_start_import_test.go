package project

// Synthetic importer/start regressions only. The genuine optimizer composition
// oracle is separate; this fixture creates no engine/model/completion authority.
import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type startImportFixture struct {
	s    *store.Store
	raw  []byte
	plan string
	now  time.Time
	cfg  activity.Configuration
}

func newStartImportFixture(t *testing.T) startImportFixture {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value() int { return 1 }\n")
	testkit.Git(t, root, "add", "main.go")
	testkit.Git(t, root, "commit", "-qm", "synthetic baseline")
	s, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	if _, e = Collect(s, CollectOptions{Now: func() time.Time { return now }}); e != nil {
		t.Fatal(e)
	}
	p, e := BuildPackage(s, ExportOptions{All: true, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	if e = MarkExported(s, p, "synthetic", "", now); e != nil {
		t.Fatal(e)
	}
	fake := &testkit.Server{Docs: map[string][]byte{}}
	for _, d := range p.Docs {
		fake.Docs[d.ID] = d.Raw
	}
	raw := fake.RecommendationFor(s.Cfg.ProjectNamespace)
	var d map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		t.Fatal("synthetic document unavailable")
	}
	id := obj(d, "payload")["plans"].([]any)[0].(map[string]any)["plan_id"].(string)
	return startImportFixture{s: s, raw: raw, plan: id, now: now}
}

func mutateStartImport(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var d map[string]any
	if e := json.Unmarshal(raw, &d); e != nil {
		t.Fatal(e)
	}
	edit(d)
	var e error
	d["content_digest"], e = hint.Digest(d["payload"])
	if e != nil {
		t.Fatal(e)
	}
	raw, e = json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func TestWorkStartRejectsSelectedImportRefusalBeforePersistence(t *testing.T) {
	for _, kind := range []string{"snapshot_mismatch", "expired", "unresolved_id"} {
		t.Run(kind, func(t *testing.T) {
			f := newStartImportFixture(t)
			switch kind {
			case "snapshot_mismatch":
				testkit.WriteFile(t, f.s.Root, "main.go", "package main\nfunc Value() int { return 999 }\n")
			case "expired":
				f.raw = mutateStartImport(t, f.raw, func(d map[string]any) {
					obj(d, "payload")["plans"].([]any)[0].(map[string]any)["expires_at"] = "2026-01-02T00:00:00Z"
				})
			case "unresolved_id":
				// Remove a real target from the current scan; keep the received plan valid.
				testkit.WriteFile(t, f.s.Root, "main.go", "package main\n")
			}
			cur, e := CurrentState(f.s, f.now)
			if e != nil {
				t.Fatal(e)
			}
			im, e := Import(f.raw, cur)
			if e != nil || im.Result.Document != "accepted" || selectedPlanImportStatus(im.Result.Plans, f.plan) != "reject" {
				t.Fatal("fixture did not reach selected-only refusal", e, im)
			}
			found := false
			for _, r := range im.Result.Plans {
				if r.PlanID == f.plan {
					for _, c := range r.Codes {
						found = found || c == kind
					}
				}
			}
			if !found {
				t.Fatal("wrong refusal reason", im.Result)
			}
			if _, e = WorkStart(f.s, "must-not-exist", f.raw, f.plan, "synthetic", &f.cfg, f.now); e == nil {
				t.Fatal("rejected selected plan started")
			}
			if _, e = LoadWork(f.s, "must-not-exist"); !errors.Is(e, ErrWorkUnknown) {
				t.Fatal("refusal persisted original", e)
			}
		})
	}
}

func TestWorkStartAcceptedAndRenderOnlyRemainDeclaredAssociations(t *testing.T) {
	for _, status := range []string{"accept", "render_only", "partial"} {
		t.Run(status, func(t *testing.T) {
			f := newStartImportFixture(t)
			if status == "render_only" {
				var d map[string]any
				if e := json.Unmarshal(f.raw, &d); e != nil {
					t.Fatal(e)
				}
				f.plan = obj(d, "payload")["plans"].([]any)[1].(map[string]any)["plan_id"].(string)
			} else if status == "partial" {
				f.raw = mutateStartImport(t, f.raw, func(d map[string]any) {
					obj(d, "payload")["plans"].([]any)[0].(map[string]any)["steps"].([]any)[0].(map[string]any)["support"] = "unsupported"
				})
			}
			pr, e := resolvePlan(f.s, f.raw, f.plan, f.now)
			if e != nil || pr.ImportStatus != status {
				t.Fatal("wrong selected status", e, pr.ImportStatus)
			}
			u, e := WorkStart(f.s, "declared-only", f.raw, f.plan, "synthetic", &f.cfg, f.now)
			if e != nil || u == nil || len(u.Revisions) != 1 || u.State != "open" {
				t.Fatal("declared association refused", e)
			}
		})
	}
}

func TestWorkStartGateDoesNotRejectRetrospectiveDeclaration(t *testing.T) {
	f := newStartImportFixture(t)
	testkit.WriteFile(t, f.s.Root, "main.go", "package main\nfunc Value() int { return 999 }\n")
	if _, e := Collect(f.s, CollectOptions{Now: func() time.Time { return f.now.Add(time.Second) }}); e != nil {
		t.Fatal(e)
	}
	pr, e := resolvePlan(f.s, f.raw, f.plan, f.now)
	if e != nil || pr.ImportStatus != "reject" {
		t.Fatal("after-state did not reject old selected plan", e, pr.ImportStatus)
	}
	if _, made, e := Declare(f.s, f.raw, f.plan, f.now); e != nil || !made {
		t.Fatal("retrospective parent association changed", e)
	}
}

func TestSelectedPlanImportStatusRequiresExactlyOneCorrespondence(t *testing.T) {
	for _, rs := range [][]hint.PlanResult{nil, {{PlanID: "other", Status: "accept"}}, {{PlanID: "selected", Status: "accept"}, {PlanID: "selected", Status: "reject"}}} {
		if selectedPlanImportStatus(rs, "selected") != "" {
			t.Fatal("missing or repeated result admitted")
		}
	}
}
