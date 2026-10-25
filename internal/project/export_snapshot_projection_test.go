package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
)

func projectionFixture(t *testing.T) (memFixture, *snapshot.Local, *Package, *LocalProjectionReceipt) {
	t.Helper()
	f := membershipFixture(t)
	local, e := LoadLatest(f.s, f.s.RepoID(f.s.Cfg.Repos[0]))
	if e != nil || local == nil {
		t.Fatal(e)
	}
	p, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	return f, local, p, r
}
func projectionStateFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			h := sha256.Sum256(b)
			rel, _ := filepath.Rel(root, p)
			out[rel] = hex.EncodeToString(h[:])
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func projected(t *testing.T, r *LocalProjectionReceipt, local snapshot.Local) *LocalSnapshotProjection {
	t.Helper()
	p, e := r.ProjectSnapshot(local)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestSnapshotProjectionNormalParityNoStateOrPackageMutation(t *testing.T) {
	f, local, p, r := projectionFixture(t)
	local.Index["PRIVATE-INDEX-CANARY"] = snapshot.IndexEntry{Hash: "PRIVATE-CONTENT-CANARY"}
	local.StateKey = "PRIVATE-STATE-CANARY"
	input, _ := json.Marshal(local)
	pkgBefore, _ := json.Marshal(p)
	before := projectionStateFiles(t, f.s.Path("state"))
	result := projected(t, r, *local)
	if state, reason := result.State(); state != "complete" {
		t.Fatal(state, reason)
	}
	meta := result.Metadata()
	if meta.Namespace != f.s.Cfg.ProjectNamespace || meta.Ref.SnapshotID != local.Record.SnapshotID || meta.Ref.RepoID != local.Record.RepoID || meta.Ref.Digest != p.SnapshotDigests[local.Record.SnapshotID] || meta.SourceSnapshotDigest != local.Record.Digest {
		t.Fatal("projection bindings differ")
	}
	found := false
	for _, d := range p.Docs {
		if d.Type == "repository_snapshot" {
			var doc map[string]any
			json.Unmarshal(d.Raw, &doc)
			expected, _ := json.Marshal(doc["payload"])
			if !bytes.Equal(expected, result.Payload()) {
				t.Fatal("normal transformer byte parity")
			}
			found = true
		}
		if d.Type == "export_policy" && meta.PolicyDigest != d.Digest {
			t.Fatal("wrong frozen policy digest")
		}
	}
	if !found {
		t.Fatal("normal snapshot fixture absent")
	}
	for _, bad := range []string{"PRIVATE-INDEX-CANARY", "PRIVATE-CONTENT-CANARY", "PRIVATE-STATE-CANARY", "\"index\"", "\"state_key\"", "\"hash\""} {
		if bytes.Contains(result.Payload(), []byte(bad)) {
			t.Fatal("private local data escaped")
		}
	}
	if _, e := json.Marshal(result); e == nil {
		t.Fatal("projection silently serialized as public wire")
	}
	if !reflect.DeepEqual(before, projectionStateFiles(t, f.s.Path("state"))) {
		t.Fatal("projection wrote state/current/latest")
	}
	pkgAfter, _ := json.Marshal(p)
	inputAfter, _ := json.Marshal(local)
	if !bytes.Equal(pkgBefore, pkgAfter) || !bytes.Equal(input, inputAfter) {
		t.Fatal("projection changed package or input")
	}
	fresh, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil || !reflect.DeepEqual(p, fresh) {
		t.Fatal("normal export bytes changed", e)
	}
}

func TestSnapshotProjectionFrozenConfigAndCopiedResults(t *testing.T) {
	f, local, p, r := projectionFixture(t)
	first := projected(t, r, *local)
	bytesBefore := first.Payload()
	meta := first.Metadata()
	f.s.Cfg.ProjectNamespace = strings.Repeat("f", 32)
	f.s.Cfg.Repos[0].Name = "changed"
	f.s.Cfg.Policy.Profile = "aggregate"
	f.s.Cfg.Policy.Fields = map[string][]string{"Entity": {"entity_id"}}
	p.Policy["profile"] = "aggregate"
	p.Policy["fields"].(map[string][]string)["Entity"][0] = "changed"
	second := projected(t, r, *local)
	if !bytes.Equal(bytesBefore, second.Payload()) || second.Metadata() != meta {
		t.Fatal("mutable config/package affected frozen projection")
	}
	returned := first.Payload()
	returned[0] = 'X'
	m := first.Metadata()
	m.Ref.Digest = "changed"
	local.Entities[0].ID = "changed"
	local.Index["later"] = snapshot.IndexEntry{Hash: "PRIVATE-LATER"}
	if !bytes.Equal(first.Payload(), bytesBefore) || first.Metadata() != meta {
		t.Fatal("caller mutation changed retained result")
	}
	if _, e := r.ProjectSnapshot(*local); e == nil {
		t.Fatal("mutated native digest accepted")
	}
}

func TestSnapshotProjectionExactTripleNoSnapshotIDMemo(t *testing.T) {
	_, local, _, r := projectionFixture(t)
	a := projected(t, r, *local)
	local.Record.CapturedAt = memNow.Add(time.Hour).Format(time.RFC3339)
	var e error
	local.Record.Digest, e = snapshot.Digest(local.Record, local.Entities, local.Relations)
	if e != nil {
		t.Fatal(e)
	}
	b := projected(t, r, *local)
	if a.Metadata().Ref.SnapshotID != b.Metadata().Ref.SnapshotID || a.Metadata().Ref.Digest == b.Metadata().Ref.Digest {
		t.Fatal("same-ID different timestamp payload conflated")
	}
	if a.Metadata().SourceSnapshotDigest == b.Metadata().SourceSnapshotDigest {
		t.Fatal("native input binding conflated")
	}
	var first, last map[string]any
	json.Unmarshal(a.Payload(), &first)
	json.Unmarshal(b.Payload(), &last)
	before := first["snapshots"].([]any)[0].(map[string]any)["captured_at"]
	after := last["snapshots"].([]any)[0].(map[string]any)["captured_at"]
	if before == after || after != local.Record.CapturedAt {
		t.Fatal("returned payload reused an earlier captured_at")
	}

}

func TestSnapshotProjectionKeepsCapturedGitPopulationOutOfCurrent(t *testing.T) {
	f, _, p, r := projectionFixture(t)
	repo := f.s.Cfg.Repos[0]
	id := f.s.RepoID(repo)
	graph, e := scan.Scan(scan.Options{Ctx: context.Background(), Root: f.s.RepoRoot(repo), RepoID: id, Key: f.s.Key(), Files: []string{"main.go", ".git/config"}})
	if e != nil {
		t.Fatal(e)
	}
	local, _, e := snapshot.Build(nil, graph, id, f.s.Key(), snapshot.GitState{State: "unknown"}, memNow.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	state := projectionStateFiles(t, f.s.Path("state"))
	result := projected(t, r, *local)
	gitID := hint.FileID(f.s.Key(), id, ".git/config")
	if !bytes.Contains(result.Payload(), []byte(gitID)) {
		t.Fatal("receipt silently filtered captured .git graph")
	}
	for _, d := range p.Docs {
		if bytes.Contains(d.Raw, []byte(local.Record.SnapshotID)) {
			t.Fatal("later captured graph entered ordinary package")
		}
	}
	if !reflect.DeepEqual(state, projectionStateFiles(t, f.s.Path("state"))) {
		t.Fatal("historical projection changed current state")
	}
}

func TestSnapshotProjectionOmissionsAggregateAndScope(t *testing.T) {
	f, local, _, _ := projectionFixture(t)
	var keep []string
	for _, s := range f.s.Cfg.Policy.RecordTypes {
		if s != "Entity" && s != "Relation" {
			keep = append(keep, s)
		}
	}
	f.s.Cfg.Policy.RecordTypes = keep
	_, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	result := projected(t, r, *local)
	if state, _ := result.State(); state != "incomplete" || len(result.Payload()) == 0 {
		t.Fatal("omitted graph became complete or valid package refused")
	}
	f.s.Cfg.Policy.Profile = "aggregate"
	_, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	result = projected(t, r, *local)
	if state, _ := result.State(); state != "unavailable" || result.Payload() != nil {
		t.Fatal("aggregate yielded geometry")
	}
	f.s.Cfg.Policy.Profile = "detailed"
	f.s.Cfg.Policy.RecordTypes = []string{"Metric"}
	_, r, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	result = projected(t, r, *local)
	if state, _ := result.State(); state != "unavailable" {
		t.Fatal("omitted snapshot yielded geometry")
	}
	_, local, _, r = projectionFixture(t)
	local.Record.RepoID = strings.Repeat("f", 32)
	if _, e = r.ProjectSnapshot(*local); e == nil {
		t.Fatal("foreign repo accepted")
	}
}

func TestSnapshotProjectionBoundsInvalidAndZero(t *testing.T) {
	_, local, _, r := projectionFixture(t)
	original := *local
	local.Record.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, e := r.ProjectSnapshot(*local); e == nil {
		t.Fatal("wrong digest accepted")
	}
	*local = original
	local.Entities = make([]snapshot.EntityRecord, maxSnapshotProjectionRecords+1)
	if state, _ := projected(t, r, *local).State(); state != "unavailable" {
		t.Fatal("record cap escaped")
	}
	*local = original
	local.Record.Languages = []string{strings.Repeat("x", maxProjectionDocumentBytes+1)}
	if state, _ := projected(t, r, *local).State(); state != "unavailable" {
		t.Fatal("byte cap escaped")
	}
	for _, zero := range []*LocalProjectionReceipt{nil, {}} {
		p, e := zero.ProjectSnapshot(original)
		if e != nil {
			t.Fatal(e)
		}
		if state, _ := p.State(); state != "unavailable" || p.Payload() != nil {
			t.Fatal("zero context accepted")
		}
	}
	var zero *LocalSnapshotProjection
	if state, _ := zero.State(); state != "unavailable" || zero.Payload() != nil || zero.Metadata() != (SnapshotProjectionMetadata{}) {
		t.Fatal("zero projection")
	}
}

func TestSnapshotProjectionRejectsRoleOverloadAndInvalidNumbers(t *testing.T) {
	_, local, _, r := projectionFixture(t)
	local.Record.Scanner.Name = "prelaunch_observed"
	local.Record.Digest, _ = snapshot.Digest(local.Record, local.Entities, local.Relations)
	if _, e := r.ProjectSnapshot(*local); e == nil {
		t.Fatal("scanner.name used as a purpose role")
	}
	_, local, _, r = projectionFixture(t)
	local.Entities[0].Features = map[string]scan.Quantity{"size_bytes": scan.Known("bytes", math.NaN(), "synthetic")}
	local.Record.Digest, _ = snapshot.Digest(local.Record, local.Entities, local.Relations)
	if _, e := r.ProjectSnapshot(*local); e == nil {
		t.Fatal("invalid quantity escaped")
	}
}

func TestSnapshotProjectionOptionalFeaturesOmittedIncomplete(t *testing.T) {
	f, local, _, _ := projectionFixture(t)
	fields, e := hint.RecordFieldsFor("0.1")
	if e != nil {
		t.Fatal(e)
	}
	var keep []string
	for _, field := range fields["Entity"] {
		if field != "features" {
			keep = append(keep, field)
		}
	}
	f.s.Cfg.Policy.Fields = map[string][]string{"Entity": keep}
	p, r, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	before, _ := json.Marshal(p)
	result := projected(t, r, *local)
	if state, _ := result.State(); state != "incomplete" {
		t.Fatal("missing optional geometry features marked complete")
	}
	var body map[string]any
	json.Unmarshal(result.Payload(), &body)
	for _, raw := range body["entities"].([]any) {
		if _, ok := raw.(map[string]any)["features"]; ok {
			t.Fatal("omitted field escaped")
		}
	}
	after, _ := json.Marshal(p)
	if !bytes.Equal(before, after) {
		t.Fatal("incomplete projection mutated package")
	}
}

func TestSnapshotProjectionFrozenConfiguredScopeAfterCorruptLatest(t *testing.T) {
	f, local, _, _ := projectionFixture(t)
	configured := f.s.RepoID(f.s.Cfg.Repos[0])
	foreign := strings.Repeat("f", 32)
	original, _ := json.Marshal(local)
	var good snapshot.Local
	if e := json.Unmarshal(original, &good); e != nil {
		t.Fatal(e)
	}
	local.Record.RepoID = foreign
	for i := range local.Entities {
		local.Entities[i].RepoID = foreign
	}
	var e error
	local.Record.Digest, e = snapshot.Digest(local.Record, local.Entities, local.Relations)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.WriteJSON(local, "state", "snapshots", configured, local.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	before := projectionStateFiles(t, f.s.Path("state"))
	plain, e := BuildPackage(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	pkg, receipt, e := BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(plain, pkg) {
		t.Fatal("configured scope fix changed ordinary package behavior")
	}
	if _, e = receipt.ProjectSnapshot(*local); e == nil {
		t.Fatal("resealed foreign latest authorized a foreign projection")
	}
	p, e := receipt.ProjectSnapshot(good)
	if e != nil {
		t.Fatal("actual configured scope lost", e)
	}
	if state, _ := p.State(); state != "complete" || p.Metadata().Ref.RepoID != configured {
		t.Fatal("configured identity replaced by stored foreign value")
	}
	if !reflect.DeepEqual(before, projectionStateFiles(t, f.s.Path("state"))) {
		t.Fatal("projection repaired or changed stored state")
	}
}
