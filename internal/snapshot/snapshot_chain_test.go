package snapshot

import (
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

func chainSource(hash string) *scan.Result {
	return &scan.Result{Entities: []scan.Entity{{ID: "11111111111111111111111111111111", Kind: "file", Hash: hash}}}
}

func TestSnapshotChainReturnToAnEarlierStateIsANewRevision(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	repo, g := "22222222222222222222222222222222", GitState{State: "dirty"}
	a, changed, err := Build(nil, chainSource("a"), repo, key, g, time.Unix(1000, 0))
	if err != nil || !changed {
		t.Fatal(err)
	}
	// The genesis recipe is unchanged: repository and state key only.
	if a.Record.SnapshotID != hint.DeriveID(key, "snapshot", repo, a.StateKey) || len(a.Record.ParentSnapshotIDs) != 0 {
		t.Fatal("genesis identity changed")
	}
	same, changed, err := Build(a, chainSource("a"), repo, key, g, time.Unix(1500, 0))
	if err != nil || changed || same != a {
		t.Fatal("an unchanged state must return the previous snapshot unchanged")
	}
	b, _, err := Build(a, chainSource("b"), repo, key, g, time.Unix(1001, 0))
	if err != nil {
		t.Fatal(err)
	}
	again, changed, err := Build(b, chainSource("a"), repo, key, g, time.Unix(1002, 0))
	if err != nil || !changed {
		t.Fatal(err)
	}
	ids := map[string]bool{a.Record.SnapshotID: true, b.Record.SnapshotID: true, again.Record.SnapshotID: true}
	if len(ids) != 3 || again.StateKey != a.StateKey {
		t.Fatal("A-B-A must give three distinct snapshot IDs for two equal states")
	}
	if b.Record.ParentSnapshotIDs[0] != a.Record.SnapshotID || again.Record.ParentSnapshotIDs[0] != b.Record.SnapshotID {
		t.Fatal("wrong parent chain")
	}
	if again.Record.SnapshotID != hint.DeriveID(key, "snapshot", repo, a.StateKey, SuccessorIDVersion, b.Record.SnapshotID) {
		t.Fatal("successor identity recipe changed")
	}
	// The same state and predecessor always give the same successor (deterministic rebuild).
	rebuilt, _, err := Build(b, chainSource("a"), repo, key, g, time.Unix(2000, 0))
	if err != nil || rebuilt.Record.SnapshotID != again.Record.SnapshotID || rebuilt.Record.Digest == again.Record.Digest {
		t.Fatal("successor identity must not depend on the capture time; the digest does")
	}
	for _, e := range again.Entities {
		if e.SnapshotID != again.Record.SnapshotID || e.ID != "11111111111111111111111111111111" {
			t.Fatal("stable entity identity changed or snapshot link wrong")
		}
	}
}
