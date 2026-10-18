package mcpserver

import (
	"fmt"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// bigSnap is a synthetic snapshot: one package directory with n files that all import it
// and each other's directory (a dense cycle), each with one declaration.
func bigSnap(n int) *ctxSnap {
	l := &snapshot.Local{Record: snapshot.Record{SnapshotID: fmt.Sprintf("%032x", 1)}, Index: map[string]snapshot.IndexEntry{}}
	id := func(k string, i int) string { return fmt.Sprintf("%s%031x", k, i) }
	repo, dir := id("a", 0), id("b", 0)
	add := func(e scan.Entity, ix snapshot.IndexEntry) {
		l.Entities = append(l.Entities, snapshot.EntityRecord{Entity: e})
		l.Index[e.ID] = ix
	}
	add(scan.Entity{ID: repo, Kind: "repository"}, snapshot.IndexEntry{Kind: "repository"})
	add(scan.Entity{ID: dir, Kind: "directory", ParentID: &repo}, snapshot.IndexEntry{Kind: "directory", Path: "p"})
	for i := range n {
		f, d := id("c", i), id("d", i)
		add(scan.Entity{ID: f, Kind: "file", ParentID: &dir}, snapshot.IndexEntry{Kind: "file", Path: fmt.Sprintf("p/f%04d.go", i)})
		add(scan.Entity{ID: d, Kind: "function", ParentID: &f}, snapshot.IndexEntry{Kind: "function", Path: fmt.Sprintf("p/f%04d.go", i), Name: fmt.Sprintf("p.F%d", i), Line: 3})
		l.Relations = append(l.Relations, snapshot.RelationRecord{Relation: scan.Relation{Kind: "imports", Source: f, Target: dir}})
	}
	return newCtxSnap(store.Repo{Name: "r"}, l, true)
}

func TestContextWalkStaysBoundedOnALargeCycle(t *testing.T) {
	c := bigSnap(2000)
	seed := fmt.Sprintf("d%031x", 7)
	rd := &sourceReader{cache: map[string]fileState{}}
	for _, lim := range []ContextLimits{{Depth: 2, Entities: 100, Bytes: 64 << 10, MaxMillis: 5000}, {Depth: 2, Entities: 100, Bytes: 4096, MaxMillis: 5000}} {
		out := &ContextOut{Omitted: ContextOmitted{Reasons: []string{}}}
		t0 := time.Now()
		(&handlers{}).ctxWalk(c, seed, lim, newCtxAdder(out, lim, time.Now, time.Now().Add(time.Hour)), rd, out)
		if len(out.Items) > lim.Entities || out.Omitted.Entities == 0 || time.Since(t0) > 2*time.Second {
			t.Fatalf("limits %v: %d items, omitted %v, %v", lim, len(out.Items), out.Omitted, time.Since(t0))
		}
		seen := map[string]bool{}
		for _, it := range out.Items {
			if seen[it.Ref] {
				t.Fatalf("item twice: %v", it.Ref)
			}
			seen[it.Ref] = true
		}
	}
	// a passed deadline stops the walk after the start and says so
	out := &ContextOut{Omitted: ContextOmitted{Reasons: []string{}}}
	lim := ContextLimits{Depth: 2, Entities: 100, Bytes: 64 << 10}
	(&handlers{}).ctxWalk(c, seed, lim, newCtxAdder(out, lim, time.Now, time.Now().Add(-time.Second)), rd, out)
	if len(out.Items) != 1 || out.Omitted.Reasons[0] != "time" || out.Omitted.Entities == 0 {
		t.Fatalf("deadline: %d items, %v", len(out.Items), out.Omitted)
	}
}
