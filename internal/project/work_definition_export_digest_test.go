package project

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/testkit"
)

// This tests stored-input parity, not actual original-start or issuer authority.
// The export alias comes from normal Collect/BuildPackage/MarkExported, not a
// fabricated accepted digest or a hand-edited snapshot/export journal.
func TestWorkDefinitionFrozenBaseKeepsRepositoryExportDigestAliases(t *testing.T) {
	f := definitionFixtureNew(t)
	f.now = f.now.Add(17 * time.Second)
	testkit.WriteFile(t, f.s.Root, "main.go", "package main\nfunc Value() int {return 2}\n")
	if _, e := Collect(f.s, CollectOptions{Now: func() time.Time { return f.now }}); e != nil {
		t.Fatal(e)
	}
	f.s.Cfg.Policy.Profile = "coarse"
	f.s.Cfg.Policy.TimeResolutionSeconds = 3600
	if e := f.s.SaveConfig(); e != nil {
		t.Fatal(e)
	}
	p, e := BuildPackage(f.s, ExportOptions{All: true, Now: f.now})
	if e != nil {
		t.Fatal(e)
	}
	if e = MarkExported(f.s, p, "synthetic-coarsened-export", "", f.now); e != nil {
		t.Fatal(e)
	}
	local, e := LoadLatest(f.s, f.s.RepoID(f.s.Cfg.Repos[0]))
	if e != nil || local == nil {
		t.Fatal("latest unavailable", e)
	}
	if p.SnapshotDigests[local.Record.SnapshotID] == local.Record.Digest {
		t.Fatal("fixture did not produce a real exported alias")
	}
	fake := &testkit.Server{Docs: map[string][]byte{}}
	for _, d := range p.Docs {
		fake.Docs[d.ID] = d.Raw
	}
	f.request.ExactSetBytes = fake.RecommendationFor(f.s.Cfg.ProjectNamespace)
	var doc map[string]any
	if e = json.Unmarshal(f.request.ExactSetBytes, &doc); e != nil {
		t.Fatal(e)
	}
	f.request.PlanID = obj(doc, "payload")["plans"].([]any)[0].(map[string]any)["plan_id"].(string)
	live, e := CurrentState(f.s, f.now)
	if e != nil {
		t.Fatal(e)
	}
	liveImport, e := Import(f.request.ExactSetBytes, live)
	if e != nil || liveImport.Result.Document != "accepted" || selectedPlanImportStatus(liveImport.Result.Plans, f.request.PlanID) != "accept" {
		t.Fatal("live importer refused actual export", e, liveImport)
	}
	ref := writeDefinition(t, f)
	h, e := LoadWorkDefinitionInputs(context.Background(), f.s, ref)
	if e != nil {
		t.Fatal(e)
	}
	id := h.Identity()
	objects := map[string][]byte{}
	l := library.Open(f.s, 0)
	get := func(d string) []byte {
		t.Helper()
		b, err := l.ResolveBoundedRead(context.Background(), "object", d, definitionMetadataMax)
		if err != nil {
			t.Fatal("retained object missing")
		}
		objects[d] = b
		return b
	}
	exports := get(id.ExportStateObject)
	for _, in := range id.SnapshotInputs {
		get(in.SnapshotObject)
		get(in.LatestRefObject)
	}
	at, e := time.Parse(time.RFC3339Nano, id.InputValidatedAt)
	if e != nil {
		t.Fatal(e)
	}
	frozen, _, e := definitionBase(f.s, at, exports, id.SnapshotInputs, objects)
	if e != nil {
		t.Fatal(e)
	}
	frozenImport, e := Import(f.request.ExactSetBytes, frozen)
	if e != nil || frozenImport.Result.Document != "accepted" || selectedPlanImportStatus(frozenImport.Result.Plans, f.request.PlanID) != "accept" {
		t.Fatal("frozen importer lost real repository export alias", e, frozenImport)
	}
	// Factual reload stays on the recorded basis and validation time, never a new scan.
	testkit.WriteFile(t, f.s.Root, "main.go", "not Go; current physical state differs\n")
	again, e := LoadWorkDefinitionInputs(context.Background(), f.s, ref)
	if e != nil || again.Identity().PlanBinding != id.PlanBinding || again.Identity().InputValidatedAt != id.InputValidatedAt {
		t.Fatal("factual reload adopted live state", e)
	}
}
