// Package snapshot turns a repository scan into immutable HINT snapshot records and keeps
// the local index (paths, names, content hashes) that never leaves the machine.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

// Record is the HINT Snapshot record.
type Record struct {
	SnapshotID        string   `json:"snapshot_id"`
	RepoID            string   `json:"repo_id"`
	CommitRef         *string  `json:"commit_ref"`
	ParentSnapshotIDs []string `json:"parent_snapshot_ids"`
	CapturedAt        string   `json:"captured_at"`
	WorktreeState     string   `json:"worktree_state"`
	Scanner           Scanner  `json:"scanner"`
	Languages         []string `json:"languages"`
	Digest            string   `json:"digest"`
}

// Scanner names the tool that produced a snapshot.
type Scanner struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EntityRecord is the HINT Entity record.
type EntityRecord struct {
	scan.Entity
	RepoID     string `json:"repo_id"`
	SnapshotID string `json:"snapshot_id"`
}

// RelationRecord is the HINT Relation record.
type RelationRecord struct {
	RelationID string `json:"relation_id"`
	scan.Relation
	SnapshotID string `json:"snapshot_id"`
}

// IndexEntry is the local view of one entity. It is never exported.
type IndexEntry struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
	Line int    `json:"line,omitempty"`
	Hash string `json:"hash,omitempty"`
}

// Local is one stored snapshot: the exported records plus the local index.
type Local struct {
	Record    Record                `json:"record"`
	Entities  []EntityRecord        `json:"entities"`
	Relations []RelationRecord      `json:"relations"`
	StateKey  string                `json:"state_key"`
	Index     map[string]IndexEntry `json:"index"`
	Ambiguous map[string][]string   `json:"ambiguous,omitempty"`
	Stats     scan.Stats            `json:"stats"`
}

// GitState describes the work tree at scan time.
type GitState struct {
	Head  string // commit hash, "" for a repository without commits
	State string // clean, dirty, unknown
}

// ReadGitState reads HEAD and the work tree state with system Git.
func ReadGitState(root string) GitState {
	g := GitState{State: "unknown"}
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", "HEAD").Output(); err == nil {
		g.Head = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output(); err == nil {
		g.State = "clean"
		if len(strings.TrimSpace(string(out))) > 0 {
			g.State = "dirty"
		}
	}
	return g
}

// SuccessorIDVersion versions the identity recipe of a changed snapshot with a predecessor.
const SuccessorIDVersion = "successor-v1"

// ID returns the snapshot ID of a state. A first snapshot keeps the original recipe
// (repository and state key). A changed snapshot with a predecessor also binds that
// predecessor, so a return to an earlier state is a new immutable revision with its own
// parent link, never the earlier record's identity.
func ID(key []byte, repoID, stateKey string, prev *Local) string {
	if prev == nil {
		return hint.DeriveID(key, "snapshot", repoID, stateKey)
	}
	return hint.DeriveID(key, "snapshot", repoID, stateKey, SuccessorIDVersion, prev.Record.SnapshotID)
}

// Build makes a snapshot from a scan. When the state did not change since prev, it
// returns prev unchanged (same snapshot ID and digest), so that a recommendation made for
// that snapshot still matches.
func Build(prev *Local, res *scan.Result, repoID string, key []byte, g GitState, now time.Time) (*Local, bool, error) {
	stateKey, err := stateKeyOf(res, g)
	if err != nil {
		return nil, false, err
	}
	if prev != nil && prev.StateKey == stateKey {
		return prev, false, nil
	}
	snapID := ID(key, repoID, stateKey, prev)
	l := &Local{StateKey: stateKey, Index: map[string]IndexEntry{}, Ambiguous: res.Ambiguous, Stats: res.Stats}
	// snapshot-local IDs for collided symbols, in a deterministic order
	ordinal := 0
	var extra []scan.Relation
	ents := make([]scan.Entity, len(res.Entities))
	copy(ents, res.Entities)
	for i := range ents {
		if ents[i].LocalKey == "" {
			continue
		}
		ordinal++
		ents[i].ID = hint.DeriveID(key, "local", snapID, *ents[i].ParentID, strconv.Itoa(ordinal))
		extra = append(extra, scan.Relation{Kind: "contains", Source: *ents[i].ParentID, Target: ents[i].ID,
			Resolution: "syntax", Evidence: "compiler"})
	}
	for _, e := range ents {
		l.Entities = append(l.Entities, EntityRecord{Entity: e, RepoID: repoID, SnapshotID: snapID})
		l.Index[e.ID] = IndexEntry{Kind: e.Kind, Path: e.Path, Name: e.Name, Line: e.Line, Hash: e.Hash}
	}
	seen := map[string]bool{}
	for _, r := range append(append([]scan.Relation{}, res.Relations...), extra...) {
		id := hint.DeriveID(key, "relation", snapID, r.Kind, r.Source, r.Target)
		if seen[id] {
			continue
		}
		seen[id] = true
		l.Relations = append(l.Relations, RelationRecord{RelationID: id, Relation: r, SnapshotID: snapID})
	}
	sort.Slice(l.Entities, func(i, j int) bool { return l.Entities[i].ID < l.Entities[j].ID })
	sort.Slice(l.Relations, func(i, j int) bool { return l.Relations[i].RelationID < l.Relations[j].RelationID })
	var commit *string
	if g.Head != "" {
		c := hint.DeriveID(key, "commit", repoID, g.Head)
		commit = &c
	}
	parents := []string{}
	if prev != nil {
		parents = append(parents, prev.Record.SnapshotID)
	}
	langs := res.Languages
	if langs == nil {
		langs = []string{}
	}
	l.Record = Record{SnapshotID: snapID, RepoID: repoID, CommitRef: commit, ParentSnapshotIDs: parents,
		CapturedAt: now.UTC().Truncate(time.Second).Format(time.RFC3339), WorktreeState: g.State,
		Scanner: Scanner{Name: "hint-scan", Version: scan.Version}, Languages: langs}
	if l.Record.Digest, err = Digest(l.Record, l.Entities, l.Relations); err != nil {
		return nil, false, err
	}
	return l, true, nil
}

// Digest computes the snapshot digest of hint-data §10 for one snapshot.
func Digest(rec Record, ents []EntityRecord, rels []RelationRecord) (string, error) {
	rec.Digest = "sha256:" + strings.Repeat("0", 64) // replaced; SnapshotDigest drops the field
	payload, err := Plain(map[string]any{"snapshots": []any{rec}, "entities": ents, "relations": rels})
	if err != nil {
		return "", err
	}
	return hint.SnapshotDigest(payload, rec.SnapshotID)
}

// Plain round-trips a value through JSON into plain maps (the form of a parsed document).
func Plain(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

// stateKey hashes everything that makes up the snapshot content: entities (with their
// local identity for collided symbols), relations, the commit and the work tree state.
func stateKeyOf(res *scan.Result, g GitState) (string, error) {
	type ent struct {
		scan.Entity
		Local string `json:"local,omitempty"`
		Hash  string `json:"hash,omitempty"`
	}
	es := make([]ent, 0, len(res.Entities))
	for _, e := range res.Entities {
		es = append(es, ent{Entity: e, Local: e.LocalKey, Hash: e.Hash})
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].ID != es[j].ID {
			return es[i].ID < es[j].ID
		}
		return es[i].Local < es[j].Local
	})
	rels := append([]scan.Relation{}, res.Relations...)
	sort.Slice(rels, func(i, j int) bool {
		return rels[i].Kind+rels[i].Source+rels[i].Target < rels[j].Kind+rels[j].Source+rels[j].Target
	})
	b, err := json.Marshal(map[string]any{"e": es, "r": rels, "head": g.Head, "state": g.State, "langs": res.Languages,
		"scanner": scan.Version})
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// Name returns the local display name of an entity: path, or path#qualified name.
func (e IndexEntry) Display() string {
	switch {
	case e.Kind == "repository":
		return "(repository root)"
	case e.Name != "" && e.Line > 0:
		return fmt.Sprintf("%s:%d %s", e.Path, e.Line, e.Name)
	case e.Name != "":
		return e.Path + " " + e.Name
	default:
		return e.Path
	}
}
