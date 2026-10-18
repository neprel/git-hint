package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/store"
)

// Declaration is a client-declared association of a completed local change (the latest
// snapshot of a repository against its parent) with one plan of a saved recommendation set.
// It freezes the set ID and digest, the plan ID and both snapshot refs; export never resolves
// "latest" again. It declares an association only, not a correct realization or an effect.
type Declaration struct {
	RepoID             string `json:"repo_id"`
	BeforeSnapshotID   string `json:"before_snapshot_id"`
	AfterSnapshotID    string `json:"after_snapshot_id"`
	SetID              string `json:"recommendation_set_id"`
	SetDigest          string `json:"recommendation_set_digest"`
	PlanID             string `json:"plan_id"`
	PlanSnapshotDigest string `json:"plan_snapshot_digest"`
}

// ErrDeclarationConflict means another declaration exists for the same change.
var ErrDeclarationConflict = errors.New("a different declaration exists for this change")

func declarationFile(repoID, afterID string) string { return repoID + "-" + afterID + ".json" }

// Declare validates the saved set with the existing importer and records the declaration of
// planID for the latest change of the plan's repository. It reads local state only (no
// network). A repeat of the same declaration is accepted unchanged (created = false); a
// different declaration for the same change is ErrDeclarationConflict.
func Declare(s *store.Store, raw []byte, planID string, now time.Time) (d Declaration, created bool, err error) {
	cur, err := CurrentState(s, now)
	if err != nil {
		return d, false, err
	}
	im, err := Import(raw, cur)
	if err != nil {
		return d, false, err
	}
	if im.Result.Document != "accepted" {
		return d, false, fmt.Errorf("the recommendation set is not accepted: %s", im.Result.Document)
	}
	doc := im.Doc
	var plan map[string]any
	for _, p := range list(obj(doc, "payload")["plans"]) {
		if pm, _ := p.(map[string]any); pm != nil && pm["plan_id"] == planID {
			plan = pm
		}
	}
	if plan == nil {
		return d, false, fmt.Errorf("plan %s is not in the recommendation set", planID)
	}
	repos := list(plan["repo_ids"])
	if len(repos) != 1 {
		// one change set per repository with a complete group would be needed; the client
		// does not build cross-repository declarations yet
		return d, false, fmt.Errorf("plan %s spans %d repositories: a cross-repository declaration is not supported", planID, len(repos))
	}
	repoID, _ := repos[0].(string)
	var r *store.Repo
	for i := range s.Cfg.Repos {
		if s.RepoID(s.Cfg.Repos[i]) == repoID {
			r = &s.Cfg.Repos[i]
		}
	}
	if r == nil {
		return d, false, fmt.Errorf("plan %s is for a repository that is not configured here", planID)
	}
	latest, err := LoadLatest(s, repoID)
	if err != nil {
		return d, false, err
	}
	if latest == nil || len(latest.Record.ParentSnapshotIDs) == 0 {
		return d, false, fmt.Errorf("%s: no completed change (no snapshot after an earlier one)", r.Name)
	}
	parent := latest.Record.ParentSnapshotIDs[0]
	var planSnap map[string]any
	for _, x := range list(plan["snapshots"]) {
		if xm, _ := x.(map[string]any); xm != nil && xm["repo_id"] == repoID {
			planSnap = xm
		}
	}
	if planSnap == nil || planSnap["snapshot_id"] != parent {
		return d, false, fmt.Errorf("%s: the plan was made for another snapshot than the parent of the latest snapshot", r.Name)
	}
	st, err := loadExportState(s)
	if err != nil {
		return d, false, err
	}
	digest, _ := planSnap["digest"].(string)
	known := false
	for _, x := range st.SnapshotDigests[parent] {
		known = known || x == digest
	}
	if !known {
		return d, false, fmt.Errorf("%s: the plan's snapshot digest was never exported for the parent snapshot", r.Name)
	}
	d = Declaration{RepoID: repoID, BeforeSnapshotID: parent, AfterSnapshotID: latest.Record.SnapshotID,
		SetID: str(doc["document_id"]), SetDigest: str(doc["content_digest"]), PlanID: planID, PlanSnapshotDigest: digest}
	var old Declaration
	name := declarationFile(repoID, d.AfterSnapshotID)
	switch err := s.ReadJSON(&old, "state", "declarations", name); {
	case err == nil:
		if old != d {
			return old, false, ErrDeclarationConflict
		}
		return d, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return d, false, err
	}
	if err := s.WriteJSON(d, "state", "declarations", name); err != nil {
		return d, false, err
	}
	return d, true, nil
}

// Declarations returns the recorded declarations of a repository, sorted by after snapshot.
func Declarations(s *store.Store, repoID string) ([]Declaration, error) {
	files, err := filepath.Glob(filepath.Join(s.Path("state", "declarations"), repoID+"-*.json"))
	if err != nil {
		return nil, err
	}
	var out []Declaration
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var d Declaration
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("declaration %s: %w", filepath.Base(f), err)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AfterSnapshotID < out[j].AfterSnapshotID })
	return out, nil
}

// planApplication is the canonical payload field of a declared change set.
func (d Declaration) planApplication() map[string]any {
	return map[string]any{
		"recommendation_set": map[string]any{"document_id": d.SetID, "content_digest": d.SetDigest},
		"plan_id":            d.PlanID,
		"declaration":        "client_declared_association",
	}
}

func obj(m map[string]any, k string) map[string]any { o, _ := m[k].(map[string]any); return o }
