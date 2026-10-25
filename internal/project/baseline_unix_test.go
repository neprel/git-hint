//go:build unix

package project

import (
	"context"
	"encoding/json"
	"errors"
	githint "github.com/neprel/git-hint"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func baselineFixture(t *testing.T) (definitionFixture, *BaselineCollection) {
	t.Helper()
	f, _ := captureFixture(t)
	rid := f.s.RepoID(f.s.Cfg.Repos[0])
	b, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: rid})
	if e != nil {
		t.Fatal("actual strict baseline", e)
	}
	return f, b
}
func baselinePlan(t *testing.T, f definitionFixture, b *BaselineCollection) definitionFixture {
	t.Helper()
	pkg, e := BuildPackage(f.s, ExportOptions{All: true, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	if e = MarkExported(f.s, pkg, "baseline-fixture", "", time.Now()); e != nil {
		t.Fatal(e)
	}
	srv := &testkit.Server{Docs: map[string][]byte{}}
	for _, d := range pkg.Docs {
		srv.Docs[d.ID] = d.Raw
	}
	f.request.ExactSetBytes = srv.RecommendationFor(f.s.Cfg.ProjectNamespace)
	if len(f.request.ExactSetBytes) == 0 {
		t.Fatal("no normal plan fixture")
	}
	var doc map[string]any
	_ = json.Unmarshal(f.request.ExactSetBytes, &doc)
	f.request.PlanID = obj(doc, "payload")["plans"].([]any)[0].(map[string]any)["plan_id"].(string)
	f.request.Label = "baseline-bound-unit"
	ref := b.Ref()
	f.request.Baseline = &ref
	return f
}
func TestVerifiedBaselineActualPlanDefinitionSourceV2(t *testing.T) {
	f, b := baselineFixture(t)
	v := b.Identity()
	if !baselineAcceptanceMatches(b.Acceptance(), b.Ref(), v) || v.Authority != "unknown" {
		t.Fatal("missing actual process checkpoint")
	}
	loaded, e := LoadBaselineObservation(context.Background(), f.s, b.Ref())
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(loaded.Identity(), v) || loaded.Binding().Observation != b.Ref() {
		t.Fatal("factual reload drift")
	}
	local := loaded.Snapshot()
	if local.Record.Digest != v.SnapshotDigest || local.Record.SnapshotID != v.SnapshotID {
		t.Fatal("wrong native triple")
	}
	found := false
	for _, ix := range local.Index {
		if ix.Path == "main.go" {
			for _, p := range mustSelected(t, loaded) {
				if p.path == ix.Path && p.digest == "sha256:"+ix.Hash && p.mode == "100644" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("retained modes/content/native index join missing")
	}
	// The normal transformer must produce the same snapshot used by the plan.
	_, receipt, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	projection, e := receipt.ProjectSnapshot(*local)
	state, _ := projection.State()
	if e != nil || state != "complete" {
		t.Fatal("normal snapshot projection", e)
	}
	f = baselinePlan(t, f, b)
	def := writeDefinition(t, f)
	h, e := LoadWorkDefinitionInputs(context.Background(), f.s, def)
	if e != nil {
		t.Fatal(e)
	}
	if h.Identity().Kind != WorkDefinitionInputsV2 || *h.Identity().Baseline != loaded.Binding() {
		t.Fatal("v2 baseline binding")
	}
	a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, def), time.Now())
	if e != nil {
		t.Fatal("actual same-source v2", e)
	}
	src, e := LoadCaptureSource(context.Background(), f.s, *a.Source())
	if e != nil {
		t.Fatal(e)
	}
	if src.Identity().Kind != CaptureSourceV2 || src.Identity().Definition.Baseline.Physical != v.Physical {
		t.Fatal("wrong v2 physical binding")
	}
	copy := loaded.Identity()
	copy.Objects[0].Roles[0] = "changed"
	if reflect.DeepEqual(copy, loaded.Identity()) {
		t.Fatal("mutable identity")
	}
	s1 := loaded.Snapshot()
	s1.Index = map[string]snapshot.IndexEntry{}
	if len(loaded.Snapshot().Index) == 0 {
		t.Fatal("mutable snapshot")
	}
	raw, _ := loaded.Object(v.Physical.TreeDigest)
	raw[0] = '!'
	again, _ := loaded.Object(v.Physical.TreeDigest)
	if raw[0] == again[0] {
		t.Fatal("mutable object")
	}
	if _, e = json.Marshal(b.Acceptance()); e == nil {
		t.Fatal("serializable acceptance")
	}
	zero := BaselineAcceptance{}
	if baselineAcceptanceMatches(&zero, b.Ref(), v) {
		t.Fatal("zero checkpoint")
	}
	foreign := *b.Acceptance()
	foreign.checkpoint.issuer = &preworkIssuer{1}
	if baselineAcceptanceMatches(&foreign, b.Ref(), v) {
		t.Fatal("foreign issuer")
	}
	foreign = *b.Acceptance()
	foreign.checkpoint = mintPreworkAcceptance(preworkSelectedSource)
	if baselineAcceptanceMatches(&foreign, b.Ref(), v) {
		t.Fatal("cross-kind issuer")
	}
	// Baseline reservation joins the SAME destructive retention barrier.
	if _, e = f.l.Forget([]string{def.RecordRef}); !errors.Is(e, library.ErrDefinitionReferences) {
		t.Fatal("retention", e)
	}
}

type selectedTestFact struct{ path, mode, digest string }

func mustSelected(t *testing.T, b *BaselineInputs) []selectedTestFact {
	t.Helper()
	a, _, _, _ := b.pair.Receipts()
	out := []selectedTestFact{}
	for _, p := range a.Selected {
		if p.State == "regular_present" {
			out = append(out, selectedTestFact{p.Path, p.Mode, p.ObjectDigest})
		}
	}
	return out
}
func TestVerifiedBaselineSourceV2PhysicalDrift(t *testing.T) {
	for _, kind := range []string{"bytes", "mode", "delete", "extra", "git_state"} {
		t.Run(kind, func(t *testing.T) {
			f, b := baselineFixture(t)
			f = baselinePlan(t, f, b)
			def := writeDefinition(t, f)
			root := f.s.RepoRoot(f.s.Cfg.Repos[0])
			file := filepath.Join(root, "main.go")
			switch kind {
			case "bytes":
				testkit.WriteFile(t, root, "main.go", "package main\nfunc Value()int{return 2}\n")
			case "mode":
				if e := os.Chmod(file, 0755); e != nil {
					t.Fatal(e)
				}
			case "delete":
				if e := os.Remove(file); e != nil {
					t.Fatal(e)
				}
			case "extra":
				testkit.WriteFile(t, root, "extra.go", "package main\n")
			case "git_state":
				testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "--allow-empty", "-qm", "new head")
			}
			a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, def), time.Now())
			if e == nil || a != nil {
				t.Fatal("changed physical baseline minted success", kind)
			}
			if kind == "mode" {
				next, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: b.Identity().RepoID})
				if e != nil {
					t.Fatal(e)
				}
				if next.Identity().SnapshotID != b.Identity().SnapshotID || next.Identity().SnapshotDigest != b.Identity().SnapshotDigest || next.Identity().Physical.TreeDigest == b.Identity().Physical.TreeDigest {
					t.Fatal("mode drift must differ physically despite same structural triple")
				}
			}
		})
	}
}
func TestVerifiedBaselineRefusalAndLatePublication(t *testing.T) {
	for _, phase := range []string{"foreign", "cancel", "pointer", "observation_commit_end", "pointer_publish_end", "unlock_end"} {
		t.Run(phase, func(t *testing.T) {
			f, _ := captureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rid := f.s.RepoID(f.s.Cfg.Repos[0])
			if phase == "foreign" {
				rid = strings.Repeat("b", 32)
			}
			if phase == "cancel" {
				cancel()
			}
			ctx = context.WithValue(ctx, baselineObserveKey{}, func(p string) {
				if phase == "pointer" && p == "observation_commit_end" {
					if e := f.s.WriteJSON(latestRef{strings.Repeat("c", 32)}, "state", "snapshots", rid, "latest.json"); e != nil {
						t.Fatal(e)
					}
				}
				if p == phase {
					cancel()
				}
			})
			b, e := CollectVerifiedBaseline(ctx, f.s, BaselineCollectRequest{RepoID: rid})
			if e == nil || b != nil {
				t.Fatal("failed/late collection succeeded", phase)
			}
			// Crash/late publication may retain immutable facts, never a recovered token.
			entries, e := f.s.ReadStateDirBounded(context.Background(), 4, "state", library.BaselineObservationStateDir)
			if e == nil {
				for _, entry := range entries {
					raw, e := f.s.ReadStateBounded(context.Background(), captureRecordMax, "state", library.BaselineObservationStateDir, entry.Name())
					if e != nil {
						t.Fatal(e)
					}
					ref := BaselineObservationRef{strings.TrimSuffix(entry.Name(), ".json"), library.Digest(raw)}
					if _, e = LoadBaselineObservation(context.Background(), f.s, ref); e != nil {
						t.Fatal("factual retained data", e)
					}
				}
			}
		})
	}
}
func TestVerifiedBaselineRepeatCurrentAndImmutableCycle(t *testing.T) {
	f, a := baselineFixture(t)
	old := a.Snapshot()
	raw, e := os.ReadFile(f.s.Path("state", "snapshots", old.Record.RepoID, old.Record.SnapshotID+".json"))
	if e != nil {
		t.Fatal(e)
	}
	b, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: old.Record.RepoID})
	if e != nil {
		t.Fatal(e)
	}
	if a.Ref() == b.Ref() || a.Identity().SnapshotDigest != b.Identity().SnapshotDigest {
		t.Fatal("fresh observation/unchanged native reuse")
	}
	root := f.s.RepoRoot(f.s.Cfg.Repos[0])
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value()int{return 2}\n")
	if _, e = CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: old.Record.RepoID}); e != nil {
		t.Fatal(e)
	}
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value() int {return 1}\n")
	c, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: old.Record.RepoID})
	if e != nil {
		t.Fatal(e)
	}
	if c.Identity().SnapshotID == old.Record.SnapshotID {
		t.Fatal("A/B/A reused original revision ID")
	}
	after, e := os.ReadFile(f.s.Path("state", "snapshots", old.Record.RepoID, old.Record.SnapshotID+".json"))
	if e != nil || string(after) != string(raw) {
		t.Fatal("immutable ancestor overwritten")
	}
}

func TestVerifiedBaselineSelectedScopeDoesNotClaimAppCensus(t *testing.T) {
	for _, extra := range []string{".hint/generated-local.go", ".git/unselected-local-metadata"} {
		t.Run(extra, func(t *testing.T) {
			f, b := baselineFixture(t)
			f = baselinePlan(t, f, b)
			def := writeDefinition(t, f)
			root := f.s.RepoRoot(f.s.Cfg.Repos[0])
			testkit.WriteFile(t, root, extra, "synthetic unselected bytes\n")
			a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, def), time.Now())
			// An unselected .git entry can change observed directory metadata. Refusal
			// then proves drift in those observed facts, never a complete .git census.
			if e != nil {
				if extra != ".git/unselected-local-metadata" || !errors.Is(e, ErrBaselineObservation) || a != nil {
					t.Fatal(e)
				}
			} else {
				h, e := LoadCaptureSource(context.Background(), f.s, *a.Source())
				if e != nil {
					t.Fatal(e)
				}
				if h.Identity().Authority != "unknown" || h.Identity().Definition.Baseline.Physical.Selection != baselineSelection {
					t.Fatal("scope or authority promotion")
				}
			}
			base, e := LoadBaselineObservation(context.Background(), f.s, b.Ref())
			if e != nil {
				t.Fatal(e)
			}
			for _, p := range mustSelected(t, base) {
				if strings.HasPrefix(p.path, ".git/") || strings.HasPrefix(p.path, ".hint/") {
					t.Fatal("silently widened capture population")
				}
			}
		})
	}
}
func TestVerifiedBaselineOriginalEndpointAfterPointer(t *testing.T) {
	f, _ := captureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	reached := false
	ctx = context.WithValue(ctx, baselineObserveKey{}, func(p string) {
		if p == "pointer_publish_end" {
			reached = true
			<-ctx.Done()
		}
	})
	c, e := CollectVerifiedBaseline(ctx, f.s, BaselineCollectRequest{RepoID: f.s.RepoID(f.s.Cfg.Repos[0])})
	if !reached {
		t.Fatal("positive fixture did not reach publication")
	}
	if e == nil || c != nil {
		t.Fatal("late published prefix became success")
	}
	entries, e := f.s.ReadStateDirBounded(context.Background(), 2, "state", library.BaselineObservationStateDir)
	if e != nil || len(entries) != 1 {
		t.Fatal("retained prefix", e)
	}
	raw, e := f.s.ReadStateBounded(context.Background(), captureRecordMax, "state", library.BaselineObservationStateDir, entries[0].Name())
	if e != nil {
		t.Fatal(e)
	}
	var v BaselineObservationIdentity
	if captureDecode(raw, &v) != nil || v.State != "prepared_baseline_publication_unknown" {
		t.Fatal("stored prefix claims publication")
	}
	ref := BaselineObservationRef{v.RecordRef, library.Digest(raw)}
	h, e := LoadBaselineObservation(context.Background(), f.s, ref)
	if e != nil {
		t.Fatal("factual crash reload", e)
	}
	latest, e := LoadLatest(f.s, v.RepoID)
	if e != nil || latest.Record.SnapshotID != h.Identity().SnapshotID || latest.Record.Digest != h.Identity().SnapshotDigest {
		t.Fatal("pointed native triple mismatch", e)
	}
}
func TestVerifiedBaselineCorruptLoadedIndexCannotReuseStateKey(t *testing.T) {
	f, b := baselineFixture(t)
	local := b.Snapshot()
	for id, ix := range local.Index {
		if ix.Path == "main.go" {
			ix.Hash = strings.Repeat("0", 64)
			local.Index[id] = ix
		}
	}
	if e := f.s.WriteJSON(local, "state", "snapshots", local.Record.RepoID, local.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	if c, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: local.Record.RepoID}); e == nil || c != nil {
		t.Fatal("stale saved StateKey bypassed same-core graph check")
	}
}
func TestVerifiedBaselineDefinitionExactIndexAndRef(t *testing.T) {
	for _, kind := range []string{"index", "ref"} {
		t.Run(kind, func(t *testing.T) {
			f, b := baselineFixture(t)
			f = baselinePlan(t, f, b)
			if kind == "ref" {
				f.request.Baseline.RecordDigest = "sha256:" + strings.Repeat("0", 64)
			} else {
				local := b.Snapshot()
				for id, ix := range local.Index {
					if ix.Path == "main.go" {
						ix.Hash = strings.Repeat("0", 64)
						local.Index[id] = ix
					}
				}
				if e := f.s.WriteJSON(local, "state", "snapshots", local.Record.RepoID, local.Record.SnapshotID+".json"); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, time.Now()); e == nil {
				t.Fatal("definition accepted unrelated baseline", kind)
			}
		})
	}
}

func TestVerifiedBaselineCooperativeStateLockBusy(t *testing.T) {
	f, _ := captureFixture(t)
	lock, e := f.s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Unlock()
	c, e := CollectVerifiedBaseline(context.Background(), f.s, BaselineCollectRequest{RepoID: f.s.RepoID(f.s.Cfg.Repos[0])})
	if c != nil || !errors.Is(e, store.ErrBusy) {
		t.Fatal("cooperative writer lock bypass", e)
	}
	if _, e = os.Lstat(f.s.Path("state", library.BaselineObservationStateDir)); !os.IsNotExist(e) {
		t.Fatal("busy operation allocated retained objects")
	}
}

func TestVerifiedBaselineLoaderRejectsForeignAndChangedObjects(t *testing.T) {
	for _, kind := range []string{"namespace", "physical", "snapshot", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f, b := baselineFixture(t)
			v := b.Identity()
			ref := b.Ref()
			switch kind {
			case "namespace":
				v.Namespace = strings.Repeat("b", 32)
			case "physical":
				v.Physical.TreeDigest = "sha256:" + strings.Repeat("0", 64)
			case "snapshot":
				v.SnapshotDigest = "sha256:" + strings.Repeat("0", 64)
			case "missing":
				hash := strings.TrimPrefix(v.Physical.TreeDigest, "sha256:")
				if e := os.Remove(f.s.Path("state", "evals", "objects", hash[:2], hash[2:])); e != nil {
					t.Fatal(e)
				}
			}
			if kind != "missing" {
				raw, e := hint.Canonical(v)
				if e != nil {
					t.Fatal(e)
				}
				if e = f.s.WriteState(raw, "state", library.BaselineObservationStateDir, ref.RecordRef+".json"); e != nil {
					t.Fatal(e)
				}
				ref.RecordDigest = library.Digest(raw)
			}
			if _, e := LoadBaselineObservation(context.Background(), f.s, ref); e == nil {
				t.Fatal("changed exact baseline loaded", kind)
			}
		})
	}
}

func TestVerifiedBaselineFreshStoreBeforeAnyCollection(t *testing.T) {
	f, _ := captureFixture(t)
	root := filepath.Join(filepath.Dir(f.s.Root), "fresh")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "entry.go", "package fresh\nfunc Entry()int{return 1}\n")
	testkit.Git(t, root, "add", "entry.go")
	testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "-qm", "fresh")
	s, _, e := store.Init(root, store.InitOptions{})
	if e != nil {
		t.Fatal(e)
	}
	b, e := CollectVerifiedBaseline(context.Background(), s, BaselineCollectRequest{RepoID: s.RepoID(s.Cfg.Repos[0])})
	if e != nil {
		t.Fatal("first collection", e)
	}
	if b.Identity().ParentObject != nil || len(b.Snapshot().Record.ParentSnapshotIDs) != 0 {
		t.Fatal("fabricated predecessor")
	}
	latest, e := LoadLatest(s, b.Identity().RepoID)
	if e != nil || latest.Record.Digest != b.Identity().SnapshotDigest {
		t.Fatal("new current publication", e)
	}
}

func TestBaselineDeclaredSourceFamilyAndBytes(t *testing.T) {
	entries, e := os.ReadDir(".")
	if e != nil {
		t.Fatal(e)
	}
	actual := []string{}
	for _, f := range entries {
		if captureSourceFamily(f.Name()) || f.Name() == "state.go" || f.Name() == "collect.go" {
			actual = append(actual, f.Name())
		}
	}
	embedded, e := fs.ReadDir(baselineSources, ".")
	if e != nil {
		t.Fatal(e)
	}
	names := []string{}
	fixture := fstest.MapFS{}
	for _, f := range embedded {
		names = append(names, f.Name())
		raw, e := fs.ReadFile(baselineSources, f.Name())
		if e != nil {
			t.Fatal(e)
		}
		fixture[f.Name()] = &fstest.MapFile{Data: raw}
	}
	sort.Strings(actual)
	sort.Strings(names)
	if !reflect.DeepEqual(actual, names) {
		t.Fatal("baseline shared source omission", actual, names)
	}
	base := baselineSourceIdentity()
	if captureSourceIdentityFrom(fixture, githint.CaptureSourceAssets) != base {
		t.Fatal("identity recipe mismatch")
	}
	for _, name := range names {
		old := fixture[name]
		fixture[name] = &fstest.MapFile{Data: append(append([]byte{}, old.Data...), []byte("\n// changed declared dependency\n")...)}
		if captureSourceIdentityFrom(fixture, githint.CaptureSourceAssets) == base {
			t.Fatal("baseline dependency bytes unbound", name)
		}
		fixture[name] = old
	}
	fixture["baseline_unlisted.go"] = &fstest.MapFile{Data: []byte("package project\n")}
	if captureSourceIdentityFrom(fixture, githint.CaptureSourceAssets) == base {
		t.Fatal("added family bytes unbound")
	}
}
