package project

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/snapshot"
)

func chainCollect(t *testing.T, f memFixture, at time.Duration, full bool) *snapshot.Local {
	t.Helper()
	rs, e := Collect(f.s, CollectOptions{Full: full, Now: func() time.Time { return memNow.Add(at) }})
	if e != nil || len(rs) != 1 {
		t.Fatal(e)
	}
	return rs[0].Snapshot
}

func TestCollectChainKeepsEarlierSnapshotsImmutable(t *testing.T) {
	f, a, _, _ := projectionFixture(t)
	repo := f.s.RepoID(f.s.Cfg.Repos[0])
	source := filepath.Join(f.s.RepoRoot(f.s.Cfg.Repos[0]), "main.go")
	original, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	path := func(id string) string { return f.s.Path("state", "snapshots", repo, id+".json") }
	savedA, e := os.ReadFile(path(a.Record.SnapshotID))
	if e != nil {
		t.Fatal(e)
	}
	// consecutive unchanged collects, incremental and full scan: exact previous snapshot
	for i, full := range []bool{false, true} {
		u := chainCollect(t, f, time.Duration(i+1)*time.Second, full)
		if u.Record.SnapshotID != a.Record.SnapshotID || u.Record.Digest != a.Record.Digest {
			t.Fatal("an unchanged collect changed the snapshot", full)
		}
	}
	if e = os.WriteFile(source, append(append([]byte(nil), original...), []byte("\n// synthetic change\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	b := chainCollect(t, f, 10*time.Second, false)
	if e = os.WriteFile(source, original, 0600); e != nil {
		t.Fatal(e)
	}
	again := chainCollect(t, f, 20*time.Second, false)
	if after, _ := os.ReadFile(path(a.Record.SnapshotID)); !bytes.Equal(after, savedA) {
		t.Fatal("A-B-A overwrote the original snapshot")
	}
	if len(map[string]bool{a.Record.SnapshotID: true, b.Record.SnapshotID: true, again.Record.SnapshotID: true}) != 3 || again.StateKey != a.StateKey {
		t.Fatal("A-B-A must give three distinct revisions")
	}
	if b.Record.ParentSnapshotIDs[0] != a.Record.SnapshotID || again.Record.ParentSnapshotIDs[0] != b.Record.SnapshotID {
		t.Fatal("wrong parent chain")
	}
	latest, e := LoadLatest(f.s, repo)
	if e != nil || latest.Record.SnapshotID != again.Record.SnapshotID || latest.Record.Digest != again.Record.Digest {
		t.Fatal("latest must be the new A revision")
	}
	for _, s := range []*snapshot.Local{a, b, again} {
		stored, e := LoadSnapshot(f.s, repo, s.Record.SnapshotID)
		if e != nil || stored.Record.Digest != s.Record.Digest {
			t.Fatal("a stored revision is not readable with its digest")
		}
	}
	// the package of the new chain still builds and validates
	if _, _, e = BuildPackageWithReceipt(f.s, ExportOptions{All: true, Now: memNow.Add(30 * time.Second)}); e != nil {
		t.Fatal("export after A-B-A", e)
	}
	// the key never enters a stored snapshot
	for _, s := range []*snapshot.Local{a, b, again} {
		raw, _ := os.ReadFile(path(s.Record.SnapshotID))
		if bytes.Contains(raw, f.s.Key()) {
			t.Fatal("key bytes in a stored snapshot")
		}
	}
}

func TestSaveSnapshotRefusesACollisionBeforeTheLatestPointer(t *testing.T) {
	f, a, _, _ := projectionFixture(t)
	repo := f.s.RepoID(f.s.Cfg.Repos[0])
	source := filepath.Join(f.s.RepoRoot(f.s.Cfg.Repos[0]), "main.go")
	original, _ := os.ReadFile(source)
	if e := os.WriteFile(source, append(append([]byte(nil), original...), []byte("\n// collide\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	// plant a different record under the ID the next collect will derive
	res, e := Collect(f.s, CollectOptions{Now: func() time.Time { return memNow.Add(time.Second) }})
	if e != nil {
		t.Fatal(e)
	}
	next := res[0].Snapshot
	nextPath := f.s.Path("state", "snapshots", repo, next.Record.SnapshotID+".json")
	latestPath := f.s.Path("state", "snapshots", repo, "latest.json")
	// roll back the latest pointer and replace the stored body with another record (a different state key)
	if e = f.s.WriteJSON(latestRef{a.Record.SnapshotID}, "state", "snapshots", repo, "latest.json"); e != nil {
		t.Fatal(e)
	}
	planted := *a
	planted.Record.SnapshotID = next.Record.SnapshotID
	if e = f.s.WriteJSON(&planted, "state", "snapshots", repo, next.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	plantedBytes, _ := os.ReadFile(nextPath)
	latestBytes, _ := os.ReadFile(latestPath)
	if _, e = Collect(f.s, CollectOptions{Now: func() time.Time { return memNow.Add(2 * time.Second) }}); !errors.Is(e, ErrSnapshotConflict) {
		t.Fatal("a colliding stored snapshot was not refused", e)
	}
	if after, _ := os.ReadFile(nextPath); !bytes.Equal(after, plantedBytes) {
		t.Fatal("the stored record was overwritten")
	}
	if after, _ := os.ReadFile(latestPath); !bytes.Equal(after, latestBytes) {
		t.Fatal("the latest pointer moved on a conflict")
	}
}

func TestSaveSnapshotReturnsTheSameRevisionAfterACrashBeforeLatest(t *testing.T) {
	f, a, _, _ := projectionFixture(t)
	repo := f.s.RepoID(f.s.Cfg.Repos[0])
	source := filepath.Join(f.s.RepoRoot(f.s.Cfg.Repos[0]), "main.go")
	original, _ := os.ReadFile(source)
	if e := os.WriteFile(source, append(append([]byte(nil), original...), []byte("\n// crash\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	first := chainCollect(t, f, time.Second, false)
	firstBytes, _ := os.ReadFile(f.s.Path("state", "snapshots", repo, first.Record.SnapshotID+".json"))
	// simulate a crash after the snapshot file and before the latest pointer
	if e := f.s.WriteJSON(latestRef{a.Record.SnapshotID}, "state", "snapshots", repo, "latest.json"); e != nil {
		t.Fatal(e)
	}
	retry := chainCollect(t, f, 5*time.Second, false)
	if retry.Record.SnapshotID != first.Record.SnapshotID || retry.Record.Digest != first.Record.Digest {
		t.Fatal("the retry must return the stored revision of the same inputs")
	}
	if after, _ := os.ReadFile(f.s.Path("state", "snapshots", repo, first.Record.SnapshotID+".json")); !bytes.Equal(after, firstBytes) {
		t.Fatal("the stored revision changed")
	}
	if latest, e := LoadLatest(f.s, repo); e != nil || latest.Record.SnapshotID != first.Record.SnapshotID {
		t.Fatal("latest pointer not completed", e)
	}
}

// plantedNext collects a change, rolls latest back to a and returns paths plus the stored next record.
func plantedNext(t *testing.T) (memFixture, *snapshot.Local, string, string, snapshot.Local) {
	t.Helper()
	f, a, _, _ := projectionFixture(t)
	repo := f.s.RepoID(f.s.Cfg.Repos[0])
	source := filepath.Join(f.s.RepoRoot(f.s.Cfg.Repos[0]), "main.go")
	original, _ := os.ReadFile(source)
	if e := os.WriteFile(source, append(append([]byte(nil), original...), []byte("\n// reseal\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	next := chainCollect(t, f, time.Second, false)
	nextPath := f.s.Path("state", "snapshots", repo, next.Record.SnapshotID+".json")
	latestPath := f.s.Path("state", "snapshots", repo, "latest.json")
	if e := f.s.WriteJSON(latestRef{a.Record.SnapshotID}, "state", "snapshots", repo, "latest.json"); e != nil {
		t.Fatal(e)
	}
	var stored snapshot.Local
	if e := f.s.ReadJSON(&stored, "state", "snapshots", repo, next.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	return f, next, nextPath, latestPath, stored
}

func refusesUnchanged(t *testing.T, f memFixture, nextPath, latestPath string) {
	t.Helper()
	before, _ := os.ReadFile(nextPath)
	latest, _ := os.ReadFile(latestPath)
	if _, e := Collect(f.s, CollectOptions{Now: func() time.Time { return memNow.Add(3 * time.Second) }}); !errors.Is(e, ErrSnapshotConflict) {
		t.Fatal("altered stored snapshot adopted", e)
	}
	if after, _ := os.ReadFile(nextPath); !bytes.Equal(after, before) {
		t.Fatal("stored record changed")
	}
	if after, _ := os.ReadFile(latestPath); !bytes.Equal(after, latest) {
		t.Fatal("latest moved on a conflict")
	}
}

func TestSaveSnapshotRefusesResealedGraphUnderTheSameInputs(t *testing.T) {
	f, _, nextPath, latestPath, stored := plantedNext(t)
	stored.Entities[0].Stability = stored.Entities[0].Stability + "-altered"
	var err error
	if stored.Record.Digest, err = snapshot.Digest(stored.Record, stored.Entities, stored.Relations); err != nil {
		t.Fatal(err)
	}
	if e := f.s.WriteJSON(&stored, "state", "snapshots", stored.Record.RepoID, stored.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	refusesUnchanged(t, f, nextPath, latestPath)
}

func TestSaveSnapshotRefusesResealedPrivateIndex(t *testing.T) {
	f, _, nextPath, latestPath, stored := plantedNext(t)
	id := stored.Entities[0].ID
	ix := stored.Index[id]
	ix.Hash, ix.Path = "altered-hash", "altered/path.go"
	stored.Index[id] = ix
	if e := f.s.WriteJSON(&stored, "state", "snapshots", stored.Record.RepoID, stored.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	refusesUnchanged(t, f, nextPath, latestPath)
}

func TestSaveSnapshotRefusesAnInvalidStoredCaptureTime(t *testing.T) {
	f, _, nextPath, latestPath, stored := plantedNext(t)
	stored.Record.CapturedAt = "2026-10-24T12:30:01+02:00"
	var err error
	if stored.Record.Digest, err = snapshot.Digest(stored.Record, stored.Entities, stored.Relations); err != nil {
		t.Fatal(err)
	}
	if e := f.s.WriteJSON(&stored, "state", "snapshots", stored.Record.RepoID, stored.Record.SnapshotID+".json"); e != nil {
		t.Fatal(e)
	}
	refusesUnchanged(t, f, nextPath, latestPath)
}

func TestSaveSnapshotNeverFollowsASymlinkedSnapshotPath(t *testing.T) {
	f, _, nextPath, latestPath, _ := plantedNext(t)
	foreign := filepath.Join(t.TempDir(), "foreign.json")
	if e := os.WriteFile(foreign, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(nextPath); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(foreign, nextPath); e != nil {
		t.Fatal(e)
	}
	latest, _ := os.ReadFile(latestPath)
	if _, e := Collect(f.s, CollectOptions{Now: func() time.Time { return memNow.Add(3 * time.Second) }}); e == nil {
		t.Fatal("a symlinked snapshot path was accepted")
	}
	if b, _ := os.ReadFile(foreign); string(b) != "{}" {
		t.Fatal("foreign target changed")
	}
	if fi, e := os.Lstat(nextPath); e != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if after, _ := os.ReadFile(latestPath); !bytes.Equal(after, latest) {
		t.Fatal("latest moved")
	}
}
