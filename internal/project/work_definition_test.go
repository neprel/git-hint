package project

// Every source/qualification statement is SYNTHETIC stored-input test data.
// No model, engine, network or actual qualification/completion runs here.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/evaltest/qfixture"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type definitionFixture struct {
	s       *store.Store
	l       *library.Library
	request DefinitionRequest
	now     time.Time
}

func definitionFixtureNew(t *testing.T) definitionFixture {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value() int {return 1}\n")
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
	if e = MarkExported(s, p, "fixture", "", now); e != nil {
		t.Fatal(e)
	}
	// Call the fixture document generator directly. No httptest server/listener exists.
	fake := &testkit.Server{Docs: map[string][]byte{}}
	for _, d := range p.Docs {
		fake.Docs[d.ID] = d.Raw
	}
	raw := fake.RecommendationFor(s.Cfg.ProjectNamespace)
	if raw == nil {
		t.Fatal("no synthetic recommendation")
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	plan := obj(doc, "payload")["plans"].([]any)[0].(map[string]any)["plan_id"].(string)
	cfg := activity.Configuration{}
	cfg.Agent.Name = "opencode"
	cfg.Agent.Version = activity.VersionState{State: "known", Value: "2.0.26"}
	cfg.Model = activity.ModelState{State: "unknown"}
	cfg.Harness.Kind = "direct"
	cfg.Harness.Version = activity.VersionState{State: "unknown"}
	cfg.ContextPolicy.State = "disabled"
	cfg.ConfigRefs.State = "known"
	cfg.Budget.MaxRequests.State = "unknown"
	cfg.Budget.MaxSeconds.State = "unknown"
	l := library.Open(s, 0)
	q := qfixture.QualifiedIO(t, l, nil, nil)
	return definitionFixture{s, l, DefinitionRequest{Label: "synthetic-unit-secret", Family: "synthetic-family-secret", PlanID: plan, ExactSetBytes: raw, DeclaredConfiguration: cfg, QualifiedIO: qualify.ResolveIORequest{EvalName: q.EvalName, ManifestDigest: q.ManifestDigest, PreviousManifestDigest: q.PreviousManifestDigest}}, now}
}
func writeDefinition(t *testing.T, f definitionFixture) WorkDefinitionRef {
	t.Helper()
	ref, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, f.now)
	if e != nil {
		t.Fatal(e)
	}
	return ref
}
func TestWorkDefinitionDurableActualResolverPositiveAndReplay(t *testing.T) {
	f := definitionFixtureNew(t)
	ref := writeDefinition(t, f)
	raw, e := os.ReadFile(f.s.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json"))
	if e != nil {
		t.Fatal(e)
	}
	if library.Digest(raw) != ref.RecordDigest {
		t.Fatal("wrong persisted bytes")
	}
	again := writeDefinition(t, f)
	if again != ref {
		t.Fatal("non-idempotent replay")
	}
	after, _ := os.ReadFile(f.s.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json"))
	if !bytes.Equal(after, raw) {
		t.Fatal("changed replay bytes")
	}
	h, e := LoadWorkDefinitionInputs(context.Background(), f.s, ref)
	if e != nil {
		t.Fatal(e)
	}
	v := h.Identity()
	if v.Basis != "stored_input_consistency_only" || v.ResolutionState != "resolved_inputs" || v.Kind != WorkDefinitionInputsVersion || v.PlanBinding.PlanID != f.request.PlanID || v.QualifiedInputIdentity.State != "resolved_inputs" || len(v.QualifiedObjects) == 0 {
		t.Fatalf("wrong input record: %+v", v)
	}
	v.QualifiedInputIdentity.MandatoryChecks[0] = "changed"
	if h.Identity().QualifiedInputIdentity.MandatoryChecks[0] == "changed" {
		t.Fatal("mutable identity")
	}
	if _, e := LoadWork(f.s, f.request.Label); !errors.Is(e, ErrWorkUnknown) {
		t.Fatal("definition created a work unit")
	}
	for _, secret := range []string{f.request.Label, f.request.Family, f.s.Root, "Uppercase stdin."} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("local record copied forbidden source text %q", secret)
		}
	}
	// Later real-shaped unit history cannot recertify creation; exact reload stays input-only.
	if _, e := WorkStart(f.s, f.request.Label, f.request.ExactSetBytes, f.request.PlanID, f.request.Family, &f.request.DeclaredConfiguration, f.now); e != nil {
		t.Fatal(e)
	}
	if _, e = WriteWorkDefinitionInputs(context.Background(), f.s, f.request, f.now); !errors.Is(e, ErrWorkDefinition) {
		t.Fatal("later create replay accepted", e)
	}
	if _, e = LoadWorkDefinitionInputs(context.Background(), f.s, ref); e != nil {
		t.Fatal("later input consistency unreadable", e)
	}
}
func TestWorkDefinitionExactRequestAndSourceFailures(t *testing.T) {
	for _, mode := range []string{"family", "configuration", "set-bytes", "qualified-version", "record-mutation", "missing-source", "foreign-root", "cancelled", "busy"} {
		t.Run(mode, func(t *testing.T) {
			f := definitionFixtureNew(t)
			ref := writeDefinition(t, f)
			ctx := context.Background()
			switch mode {
			case "family":
				f.request.Family = "different"
			case "configuration":
				f.request.DeclaredConfiguration.ContextPolicy.State = "unknown"
			case "set-bytes":
				f.request.ExactSetBytes = append(f.request.ExactSetBytes, ' ')
			case "qualified-version":
				f.request.QualifiedIO.EvalName = "latest"
			case "record-mutation":
				p := f.s.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json")
				b, _ := os.ReadFile(p)
				b = append(b, ' ')
				os.WriteFile(p, b, 0600)
				if _, e := LoadWorkDefinitionInputs(ctx, f.s, ref); !errors.Is(e, ErrWorkDefinition) {
					t.Fatal(e)
				}
				return
			case "missing-source":
				p := f.s.Path("state", "evals", "manifests", "eval", f.request.QualifiedIO.EvalName+".json")
				os.Remove(p)
				if _, e := LoadWorkDefinitionInputs(ctx, f.s, ref); !errors.Is(e, ErrWorkDefinition) {
					t.Fatal(e)
				}
				return
			case "foreign-root":
				other := *f.s
				other.Root = t.TempDir()
				os.MkdirAll(other.Path("state", library.WorkDefinitionStateDir), 0700)
				b, _ := os.ReadFile(f.s.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json"))
				os.WriteFile(other.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json"), b, 0600)
				if _, e := LoadWorkDefinitionInputs(ctx, &other, ref); !errors.Is(e, ErrWorkDefinition) {
					t.Fatal(e)
				}
				return
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "busy":
				lock, e := f.s.LockState(false)
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Unlock()
			}
			_, e := WriteWorkDefinitionInputs(ctx, f.s, f.request, f.now)
			if !errors.Is(e, ErrWorkDefinition) {
				t.Fatal("invalid request accepted", e)
			}
			if strings.Contains(e.Error(), f.s.Root) || strings.Contains(e.Error(), f.request.Label) {
				t.Fatal("error leaks source identity")
			}
		})
	}
}
func TestWorkDefinitionPriorAndCorruptHistoryRefuses(t *testing.T) {
	for _, mode := range []string{"unit", "own-membership", "orphan-membership", "corrupt-work", "corrupt-spool", "conflicting-spool", "unreadable-sources", "orphan-run"} {
		t.Run(mode, func(t *testing.T) {
			f := definitionFixtureNew(t)
			switch mode {
			case "unit":
				if _, e := WorkStart(f.s, f.request.Label, f.request.ExactSetBytes, f.request.PlanID, f.request.Family, &f.request.DeclaredConfiguration, f.now); e != nil {
					t.Fatal(e)
				}
			case "own-membership":
				os.MkdirAll(f.s.Path(membershipPath(f.s, f.request.Label)...), 0700)
			case "orphan-membership":
				os.MkdirAll(f.s.Path("state", membershipDir, strings.Repeat("a", 32)), 0700)
			case "corrupt-work":
				f.s.WriteState([]byte(`{}`), "state", "work", strings.Repeat("a", 32)+".json")
			case "corrupt-spool":
				f.s.WriteState([]byte("not-json\n"), "state", "activity", spoolFile)
			case "conflicting-spool":
				a := activity.Observation{ObservationID: strings.Repeat("a", 32)}
				b, _ := json.Marshal(a)
				a.Op = "different"
				c, _ := json.Marshal(a)
				f.s.WriteState(append(append(append(b, '\n'), c...), '\n'), "state", "activity", spoolFile)
			case "unreadable-sources":
				f.s.WriteState([]byte(`{"bad":true}`), "state", "activity", sourcesFile)
			case "orphan-run":
				f.s.WriteState([]byte(`{"not_units":[]}`), "state", "evals", "work", "jb-synthetic.json")
			}
			if _, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, f.now); !errors.Is(e, ErrWorkDefinition) {
				t.Fatal("history accepted", e)
			}
			if _, e := os.Stat(f.s.Path("state", library.WorkDefinitionStateDir)); !os.IsNotExist(e) {
				t.Fatal("refused history wrote a definition reservation")
			}
		})
	}
}
func TestWorkDefinitionBlocksDestructiveRetentionBeforeAnyDeletion(t *testing.T) {
	f := definitionFixtureNew(t)
	ref := writeDefinition(t, f)
	for _, action := range []string{"prune", "forget", "purge", "garbage"} {
		t.Run(action, func(t *testing.T) {
			l := library.Open(f.s, 0)
			var e error
			switch action {
			case "prune":
				_, e = l.Prune(f.now.AddDate(10, 0, 0), 1)
			case "forget":
				_, e = l.Forget([]string{f.request.QualifiedIO.EvalName})
			case "purge":
				_, e = l.Purge()
			case "garbage":
				_, e = l.CollectGarbage()
			}
			if !errors.Is(e, library.ErrDefinitionReferences) {
				t.Fatal("retention did not refuse", e)
			}
			if _, e = LoadWorkDefinitionInputs(context.Background(), f.s, ref); e != nil {
				t.Fatal("retention changed referenced data", e)
			}
		})
	}
	if _, e := LoadWorkDefinitionInputs(context.Background(), f.s, WorkDefinitionRef{RecordRef: ref.RecordRef, RecordDigest: hint.EvalRef(f.s.Key(), "bad")}); !errors.Is(e, ErrWorkDefinition) {
		t.Fatal("invalid digest accepted")
	}
}
func TestWorkDefinitionPlanValidatorParityWithoutLiveScan(t *testing.T) {
	f := definitionFixtureNew(t)
	base, e := CurrentState(f.s, f.now)
	if e != nil {
		t.Fatal(e)
	}
	a, e := resolvePlan(f.s, f.request.ExactSetBytes, f.request.PlanID, f.now)
	if e != nil {
		t.Fatal(e)
	}
	b, e := resolvePlanForBase(f.s, f.request.ExactSetBytes, f.request.PlanID, base, func() (*exportState, error) { return loadExportState(f.s) })
	if e != nil || a != b {
		t.Fatal("legacy helper output changed", e)
	}
	for _, raw := range [][]byte{[]byte(`{}`), f.request.ExactSetBytes} {
		id := strings.Repeat("f", 32)
		_, a := resolvePlan(f.s, raw, id, f.now)
		_, b := resolvePlanForBase(f.s, raw, id, base, func() (*exportState, error) { return loadExportState(f.s) })
		if a == nil || b == nil || a.Error() != b.Error() {
			t.Fatal("legacy reason order changed", a, b)
		}
	}
	// Actual worktree changes do not enter the STORED-input basis or trigger a scan.
	testkit.WriteFile(t, f.s.Root, "main.go", "not valid Go; source canary\n")
	if _, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, f.now); e != nil {
		t.Fatal("stored input writer used live source", e)
	}
}

func TestWorkDefinitionImmutableReloadRefusesInventedAuthorityAndShape(t *testing.T) {
	for _, mode := range []string{"unknown-field", "accepted-state", "scope-upgrade", "snapshot-count", "absence-flag", "source-identity", "inventory"} {
		t.Run(mode, func(t *testing.T) {
			f := definitionFixtureNew(t)
			ref := writeDefinition(t, f)
			p := f.s.Path("state", library.WorkDefinitionStateDir, ref.RecordRef+".json")
			raw, _ := os.ReadFile(p)
			var value WorkDefinitionIdentity
			json.Unmarshal(raw, &value)
			switch mode {
			case "accepted-state":
				value.ResolutionState = "accepted"
			case "scope-upgrade":
				value.Basis = "whole_project_work"
			case "snapshot-count":
				for len(value.SnapshotInputs) < 17 {
					value.SnapshotInputs = append(value.SnapshotInputs, value.SnapshotInputs[0])
				}
			case "absence-flag":
				value.ObservedAbsentDefaults = []string{"complete_outside_work"}
			case "source-identity":
				value.WriterSourceIdentity = library.Digest([]byte("caller"))
			case "inventory":
				value.QualifiedObjects[0].Bytes++
			}
			value.DefinitionBindingDigest, _ = definitionBinding(value)
			raw, _ = hint.Canonical(value)
			if mode == "unknown-field" {
				var v map[string]any
				json.Unmarshal(raw, &v)
				v["qualified"] = true
				raw, _ = hint.Canonical(v)
			}
			os.WriteFile(p, raw, 0600)
			ref.RecordDigest = library.Digest(raw)
			if _, e := LoadWorkDefinitionInputs(context.Background(), f.s, ref); !errors.Is(e, ErrWorkDefinition) {
				t.Fatal("invented record state accepted", e)
			}
		})
	}
}
func TestWorkDefinitionAllowsUnrelatedCleanUnitAndRefusesMissingStoredFacts(t *testing.T) {
	for _, mode := range []string{"unrelated-unit", "missing-exports", "missing-snapshot", "bad-base-digest", "unknown-plan", "bad-configuration"} {
		t.Run(mode, func(t *testing.T) {
			f := definitionFixtureNew(t)
			switch mode {
			case "unrelated-unit":
				if _, e := WorkStart(f.s, "unrelated", nil, "", "other-family", &f.request.DeclaredConfiguration, f.now); e != nil {
					t.Fatal(e)
				}
			case "missing-exports":
				os.Remove(f.s.Path("state", "exports", "state.json"))
			case "missing-snapshot":
				r := f.s.Cfg.Repos[0]
				var latest latestRef
				f.s.ReadJSON(&latest, "state", "snapshots", f.s.RepoID(r), "latest.json")
				os.Remove(f.s.Path("state", "snapshots", f.s.RepoID(r), latest.SnapshotID+".json"))
			case "bad-base-digest":
				r := f.s.Cfg.Repos[0]
				var latest latestRef
				f.s.ReadJSON(&latest, "state", "snapshots", f.s.RepoID(r), "latest.json")
				p := f.s.Path("state", "snapshots", f.s.RepoID(r), latest.SnapshotID+".json")
				raw, _ := os.ReadFile(p)
				var snap map[string]any
				json.Unmarshal(raw, &snap)
				snap["record"].(map[string]any)["digest"] = library.Digest([]byte("wrong"))
				raw, _ = json.Marshal(snap)
				os.WriteFile(p, raw, 0600)
			case "unknown-plan":
				f.request.PlanID = strings.Repeat("f", 32)
			case "bad-configuration":
				f.request.DeclaredConfiguration.Agent.Version.Value = f.s.Root
			}
			_, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, f.now)
			if mode == "unrelated-unit" {
				if e != nil {
					t.Fatal("unrelated clean unit blocked inputs", e)
				}
			} else if !errors.Is(e, ErrWorkDefinition) {
				t.Fatal("missing/invalid stored input accepted", e)
			}
		})
	}
}
