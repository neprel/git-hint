package snapshot

import (
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

// Change is the HINT Change record.
type Change struct {
	ChangeID        string       `json:"change_id"`
	Operation       string       `json:"operation"`
	Level           string       `json:"level"`
	Before          []string     `json:"before"`
	After           []string     `json:"after"`
	Candidates      []Candidate  `json:"candidates,omitempty"`
	MatchMethod     string       `json:"match_method"`
	MatchConfidence *float64     `json:"match_confidence,omitempty"`
	Provenance      ChangeSource `json:"provenance"`
}

// Candidate is one plausible match of an AMBIGUOUS change.
type Candidate struct {
	Before string  `json:"before"`
	After  string  `json:"after"`
	Score  float64 `json:"score"`
}

// ChangeSource is the provenance of a change.
type ChangeSource struct {
	Source  string `json:"source"`
	Tool    string `json:"tool"`
	Version string `json:"version"`
}

// SnapshotRef names a snapshot by ID and digest.
type SnapshotRef struct {
	SnapshotID string `json:"snapshot_id"`
	Digest     string `json:"digest"`
}

// ChangeSet is the payload of a change_set document.
type ChangeSet struct {
	RepoID         string      `json:"repo_id"`
	BeforeSnapshot SnapshotRef `json:"before_snapshot"`
	AfterSnapshot  SnapshotRef `json:"after_snapshot"`
	Changes        []Change    `json:"changes"`
}

// LocalChange is a change with local names for the preview (never exported).
type LocalChange struct {
	Change
	BeforeNames []string
	AfterNames  []string
}

func artifact(kind string) bool { return kind == "file" }
func symbol(kind string) bool {
	return kind != "file" && kind != "directory" && kind != "repository" && kind != "contract"
}

// Diff compares two snapshots of one repository. Files are matched by ID (same path),
// then by content hash (rename: match_method hash). Symbols keep their stable ID across
// a file rename or a move inside the package; a changed parent file is a MOVE. Two
// equally good matches give AMBIGUOUS, never a guess. Snapshot-local IDs are not compared.
func Diff(key []byte, before, after *Local) (ChangeSet, []LocalChange) {
	cs := ChangeSet{RepoID: after.Record.RepoID,
		BeforeSnapshot: SnapshotRef{before.Record.SnapshotID, before.Record.Digest},
		AfterSnapshot:  SnapshotRef{after.Record.SnapshotID, after.Record.Digest}}
	prov := ChangeSource{Source: "entity_matcher", Tool: "hint-scan", Version: scan.Version}
	one := 1.0
	var out []LocalChange
	add := func(op, level, method string, conf *float64, b, a []string, cands []Candidate) {
		id := hint.DeriveID(key, "change", before.Record.SnapshotID, after.Record.SnapshotID, op,
			strings.Join(b, ","), strings.Join(a, ","))
		c := Change{ChangeID: id, Operation: op, Level: level, Before: nonNil(b), After: nonNil(a), Candidates: cands,
			MatchMethod: method, MatchConfidence: conf, Provenance: prov}
		lc := LocalChange{Change: c}
		for _, x := range b {
			lc.BeforeNames = append(lc.BeforeNames, before.Index[x].Display())
		}
		for _, x := range a {
			lc.AfterNames = append(lc.AfterNames, after.Index[x].Display())
		}
		out = append(out, lc)
	}
	ids := func(l *Local, pred func(string) bool) []string {
		var r []string
		for id, e := range l.Index {
			if pred(e.Kind) && !strings.HasPrefix(stability(l, id), "snapshot_local") {
				r = append(r, id)
			}
		}
		sort.Strings(r)
		return r
	}
	// files
	bFiles, aFiles := ids(before, artifact), ids(after, artifact)
	aSet, bSet := set(aFiles), set(bFiles)
	var gone, fresh []string
	for _, id := range bFiles {
		if !aSet[id] {
			gone = append(gone, id)
		} else if before.Index[id].Hash != after.Index[id].Hash {
			add("MODIFY", "artifact", "key", &one, []string{id}, []string{id}, nil)
		}
	}
	for _, id := range aFiles {
		if !bSet[id] {
			fresh = append(fresh, id)
		}
	}
	byHash := map[string][]string{}
	for _, id := range fresh {
		byHash[after.Index[id].Hash] = append(byHash[after.Index[id].Hash], id)
	}
	matched := map[string]bool{}
	var unmatched []string
	for _, id := range gone {
		cands := byHash[before.Index[id].Hash]
		var free []string
		for _, c := range cands {
			if !matched[c] {
				free = append(free, c)
			}
		}
		switch {
		case len(free) == 1:
			matched[free[0]] = true
			add("RENAME", "artifact", "hash", &one, []string{id}, free, nil)
		case len(free) > 1:
			var cs []Candidate
			for _, c := range free {
				cs = append(cs, Candidate{Before: id, After: c, Score: 1})
				matched[c] = true
			}
			add("AMBIGUOUS", "artifact", "hash", &one, []string{id}, free, cs)
		default:
			unmatched = append(unmatched, id)
		}
	}
	// A renamed and edited file: the new file holds most of the old file's symbols
	// (stable IDs). Jaccard similarity of the symbol sets >= 0.5; two equal best
	// candidates give AMBIGUOUS.
	bKids, aKids := children(before), children(after)
	for _, id := range unmatched {
		var best []Candidate
		for _, c := range fresh {
			if matched[c] {
				continue
			}
			j := jaccard(bKids[id], aKids[c])
			if j < 0.5 {
				continue
			}
			switch {
			case len(best) == 0 || j > best[0].Score:
				best = []Candidate{{Before: id, After: c, Score: round2(j)}}
			case j == best[0].Score:
				best = append(best, Candidate{Before: id, After: c, Score: round2(j)})
			}
		}
		switch len(best) {
		case 0:
			add("DELETE", "artifact", "none", nil, []string{id}, nil, nil)
		case 1:
			matched[best[0].After] = true
			conf := best[0].Score
			add("RENAME", "artifact", "containment", &conf, []string{id}, []string{best[0].After}, nil)
		default:
			var afters []string
			for _, c := range best {
				afters = append(afters, c.After)
				matched[c.After] = true
			}
			conf := best[0].Score
			add("AMBIGUOUS", "artifact", "containment", &conf, []string{id}, afters, best)
		}
	}
	for _, id := range fresh {
		if !matched[id] {
			add("ADD", "artifact", "none", nil, nil, []string{id}, nil)
		}
	}
	// symbols with stable IDs
	bSyms, aSyms := ids(before, symbol), ids(after, symbol)
	aS, bS := set(aSyms), set(bSyms)
	for _, id := range bSyms {
		if !aS[id] {
			add("DELETE", "symbol", "none", nil, []string{id}, nil, nil)
			continue
		}
		bp, ap := parentOf(before, id), parentOf(after, id)
		switch {
		case bp != ap && !renamedTo(out, bp, ap):
			add("MOVE", "symbol", "key", &one, []string{id}, []string{id}, nil)
		case before.Index[id].Hash != after.Index[id].Hash:
			add("MODIFY", "symbol", "key", &one, []string{id}, []string{id}, nil)
		}
	}
	for _, id := range aSyms {
		if !bS[id] {
			add("ADD", "symbol", "none", nil, nil, []string{id}, nil)
		}
	}
	for _, c := range out {
		cs.Changes = append(cs.Changes, c.Change)
	}
	if cs.Changes == nil {
		cs.Changes = []Change{}
	}
	return cs, out
}

// renamedTo reports whether the parent file of a symbol was renamed from b to a; the
// symbol then stays in the same file and is not a MOVE.
func renamedTo(out []LocalChange, b, a string) bool {
	for _, c := range out {
		if c.Operation == "RENAME" && len(c.Before) == 1 && c.Before[0] == b && len(c.After) == 1 && c.After[0] == a {
			return true
		}
	}
	return false
}

// entity finds an entity record by ID (entities are sorted by ID).
func (l *Local) entity(id string) *EntityRecord {
	i := sort.Search(len(l.Entities), func(i int) bool { return l.Entities[i].ID >= id })
	if i < len(l.Entities) && l.Entities[i].ID == id {
		return &l.Entities[i]
	}
	return nil
}

func parentOf(l *Local, id string) string {
	if e := l.entity(id); e != nil && e.ParentID != nil {
		return *e.ParentID
	}
	return ""
}

func stability(l *Local, id string) string {
	if e := l.entity(id); e != nil {
		return e.Stability
	}
	return ""
}

func set(l []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range l {
		m[x] = true
	}
	return m
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// children maps a file ID to the stable symbol IDs that it contains directly.
func children(l *Local) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	for _, e := range l.Entities {
		if e.ParentID == nil || e.Stability != "stable" || !symbol(e.Kind) {
			continue
		}
		if m[*e.ParentID] == nil {
			m[*e.ParentID] = map[string]bool{}
		}
		m[*e.ParentID][e.ID] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }
