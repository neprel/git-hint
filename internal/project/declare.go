package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/snapshot"
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
	// Work is the closed successful attempt of a declared work unit (empty for a plain
	// declaration); AfterDigest is the local digest of the after snapshot, frozen at declaration.
	Work        WorkRef `json:"work,omitempty"`
	AfterDigest string  `json:"after_digest,omitempty"`
	// AfterTree is the captured canonical tree of the after state (local library tree ID; only
	// for --after latest); empty: no artifact (a transport binding is then unavailable).
	AfterTree     string         `json:"after_tree,omitempty"`
	AfterTreeProv TreeProvenance `json:"after_tree_provenance,omitempty"`
}

// WorkRef names the exact closed successful attempt of a work unit.
type WorkRef struct {
	UnitRef    string `json:"unit_ref,omitempty"`
	Revision   int    `json:"revision,omitempty"`
	AttemptRef string `json:"attempt_ref,omitempty"`
}

// ErrDeclarationConflict means another declaration exists for the same change.
var ErrDeclarationConflict = errors.New("a different declaration exists for this change")

func declarationFile(repoID, afterID string) string { return repoID + "-" + afterID + ".json" }

// Declare validates the saved set with the existing importer and records the declaration of
// planID for the latest change of the plan's repository. It reads local state only (no
// network). A repeat of the same declaration is accepted unchanged (created = false); a
// different declaration for the same change is ErrDeclarationConflict.
func Declare(s *store.Store, raw []byte, planID string, now time.Time) (d Declaration, created bool, err error) {
	pr, err := resolvePlan(s, raw, planID, now)
	if err != nil {
		return d, false, err
	}
	latest, err := LoadLatest(s, pr.RepoID)
	if err != nil {
		return d, false, err
	}
	if latest == nil || len(latest.Record.ParentSnapshotIDs) == 0 {
		return d, false, fmt.Errorf("%s: no completed change (no snapshot after an earlier one)", pr.RepoName)
	}
	parent := latest.Record.ParentSnapshotIDs[0]
	if pr.SnapshotID != parent {
		return d, false, fmt.Errorf("%s: the plan was made for another snapshot than the parent of the latest snapshot", pr.RepoName)
	}
	d = Declaration{RepoID: pr.RepoID, BeforeSnapshotID: parent, AfterSnapshotID: latest.Record.SnapshotID,
		SetID: pr.SetID, SetDigest: pr.SetDigest, PlanID: planID, PlanSnapshotDigest: pr.SnapshotDigest}
	return record(s, d)
}

// record stores a declaration once: a repeat is unchanged, another one for the same change is
// ErrDeclarationConflict (never overwritten).
func record(s *store.Store, d Declaration) (Declaration, bool, error) {
	var old Declaration
	name := declarationFile(d.RepoID, d.AfterSnapshotID)
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

// planRef is a plan of a saved recommendation set, checked by the existing importer: one
// configured repository and a snapshot digest that was exported.
type planRef struct {
	SetID, SetDigest, PlanID, RepoID, RepoName, SnapshotID, SnapshotDigest string
}

func resolvePlan(s *store.Store, raw []byte, planID string, now time.Time) (planRef, error) {
	var pr planRef
	cur, err := CurrentState(s, now)
	if err != nil {
		return pr, err
	}
	im, err := Import(raw, cur)
	if err != nil {
		return pr, err
	}
	if im.Result.Document != "accepted" {
		return pr, fmt.Errorf("the recommendation set is not accepted: %s", im.Result.Document)
	}
	doc := im.Doc
	var plan map[string]any
	for _, p := range list(obj(doc, "payload")["plans"]) {
		if pm, _ := p.(map[string]any); pm != nil && pm["plan_id"] == planID {
			plan = pm
		}
	}
	if plan == nil {
		return pr, fmt.Errorf("plan %s is not in the recommendation set", planID)
	}
	repos := list(plan["repo_ids"])
	if len(repos) != 1 {
		// one change set per repository with a complete group would be needed; the client
		// does not build cross-repository declarations yet
		return pr, fmt.Errorf("plan %s spans %d repositories: a cross-repository declaration is not supported", planID, len(repos))
	}
	pr.RepoID, _ = repos[0].(string)
	for _, r := range s.Cfg.Repos {
		if s.RepoID(r) == pr.RepoID {
			pr.RepoName = r.Name
		}
	}
	if pr.RepoName == "" {
		return pr, fmt.Errorf("plan %s is for a repository that is not configured here", planID)
	}
	for _, x := range list(plan["snapshots"]) {
		if xm, _ := x.(map[string]any); xm != nil && xm["repo_id"] == pr.RepoID {
			pr.SnapshotID, pr.SnapshotDigest = str(xm["snapshot_id"]), str(xm["digest"])
		}
	}
	st, err := loadExportState(s)
	if err != nil {
		return pr, err
	}
	known := false
	for _, x := range st.SnapshotDigests[pr.SnapshotID] {
		known = known || x == pr.SnapshotDigest
	}
	if pr.SnapshotID == "" || !known {
		return pr, fmt.Errorf("%s: the plan's snapshot digest was never exported for that snapshot", pr.RepoName)
	}
	pr.SetID, pr.SetDigest, pr.PlanID = str(doc["document_id"]), str(doc["content_digest"]), planID
	return pr, nil
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
	pa := map[string]any{
		"recommendation_set": map[string]any{"document_id": d.SetID, "content_digest": d.SetDigest},
		"plan_id":            d.PlanID,
		"declaration":        "client_declared_association",
	}
	if d.Work.UnitRef != "" {
		pa["work"] = map[string]any{"unit_ref": d.Work.UnitRef, "revision": d.Work.Revision, "attempt_ref": d.Work.AttemptRef}
	}
	return pa
}

func obj(m map[string]any, k string) map[string]any { o, _ := m[k].(map[string]any); return o }

// DeclareWork declares the change of a closed successful work unit attempt as the application
// of the unit's plan. before = the unit's frozen base snapshot of the plan's repository (the
// plan snapshot); after = an exact stored snapshot ID or "latest", resolved once now and saved;
// after must descend from before through stored single-parent links (a merge parent, a missing
// snapshot or a broken chain is refused). Nothing is written on a refusal.
func DeclareWork(s *store.Store, raw []byte, planID, label, after string, now time.Time) (Declaration, bool, error) {
	var d Declaration
	if after == "" {
		return d, false, errors.New("--after is required with --work-unit: a snapshot ID or latest")
	}
	u, err := LoadWork(s, label)
	if err != nil {
		return d, false, err
	}
	if u.PlanID == "" || u.PlanID != planID {
		return d, false, errors.New("the work unit was not started for this plan")
	}
	rev, attempt, ok := u.ClosedAttempt()
	if !ok {
		return d, false, errors.New("the work unit has no closed successful attempt (git hint eval work close --outcome completed)")
	}
	pr, err := resolvePlan(s, raw, planID, now)
	if err != nil {
		return d, false, err
	}
	if pr.RepoID != u.PlanRepo || pr.SnapshotID != u.PlanSnap {
		return d, false, errors.New("the plan's snapshot is not the work unit's base")
	}
	var a *snapshot.Local
	if after == "latest" {
		a, err = LoadLatest(s, pr.RepoID)
	} else {
		a, err = LoadSnapshot(s, pr.RepoID, after)
	}
	if err != nil || a == nil {
		return d, false, fmt.Errorf("%s: the after snapshot %q is not stored", pr.RepoName, after)
	}
	// lineage: single-parent links from after back to the base
	cur, seen := a, map[string]bool{}
	for cur.Record.SnapshotID != u.PlanSnap {
		if seen[cur.Record.SnapshotID] || len(cur.Record.ParentSnapshotIDs) != 1 {
			return d, false, fmt.Errorf("%s: the after snapshot does not descend from the base through single-parent links", pr.RepoName)
		}
		seen[cur.Record.SnapshotID] = true
		if cur, err = LoadSnapshot(s, pr.RepoID, cur.Record.ParentSnapshotIDs[0]); err != nil {
			return d, false, fmt.Errorf("%s: a snapshot between the base and the after snapshot is not stored", pr.RepoName)
		}
	}
	if a.Record.SnapshotID == u.PlanSnap {
		return d, false, errors.New("the after snapshot is the base: there is no change to declare")
	}
	d = Declaration{RepoID: pr.RepoID, BeforeSnapshotID: u.PlanSnap, AfterSnapshotID: a.Record.SnapshotID, SetID: pr.SetID, SetDigest: pr.SetDigest,
		PlanID: planID, PlanSnapshotDigest: pr.SnapshotDigest, Work: WorkRef{UnitRef: u.UnitRef, Revision: rev, AttemptRef: attempt.Ref},
		AfterDigest: a.Record.Digest}
	if after == "latest" { // only the current state can be captured; a historical ID has no artifact
		var repo store.Repo
		for _, r := range s.Cfg.Repos {
			if s.RepoID(r) == pr.RepoID {
				repo = r
			}
		}
		if d.AfterTree, d.AfterTreeProv, err = captureAfter(context.Background(), s, repo, a, now); err != nil {
			return d, false, err
		}
	}
	// one work attempt has one declared change: another after state for it is a conflict
	others, err := Declarations(s, d.RepoID)
	if err != nil {
		return d, false, err
	}
	for _, o := range others {
		if o.Work == d.Work && o.AfterSnapshotID != d.AfterSnapshotID {
			return o, false, ErrDeclarationConflict
		}
	}
	// idempotent: the same association with the same captured tree keeps the stored declaration
	var old Declaration
	if err := s.ReadJSON(&old, "state", "declarations", declarationFile(d.RepoID, d.AfterSnapshotID)); err == nil {
		cmp := old
		cmp.AfterTreeProv = d.AfterTreeProv
		if cmp == d {
			return old, false, nil
		}
		return old, false, ErrDeclarationConflict
	}
	return record(s, d)
}

// VariantPlan returns the frozen plan binding for a refactor variant of work unit `label`: the
// declaration of the unit's closed successful attempt for plan planID, which must carry a
// captured after tree. Ambiguous or missing declarations refuse.
func VariantPlan(s *store.Store, label, planID string) (map[string]any, string, error) {
	u, err := LoadWork(s, label)
	if err != nil {
		return nil, "", err
	}
	if err := u.Revise(s, time.Now()); err != nil { // record late activity first (C7BE-8)
		return nil, "", err
	}
	rev, attempt, ok := u.ClosedAttempt()
	if !ok {
		return nil, "", errors.New("the work unit has no closed successful attempt without later activity: a plan binding is unavailable")
	}
	ds, err := Declarations(s, u.PlanRepo)
	if err != nil {
		return nil, "", err
	}
	var found []Declaration
	for _, d := range ds {
		if d.Work.UnitRef == u.UnitRef && d.Work.Revision == rev && d.Work.AttemptRef == attempt.Ref && d.PlanID == planID {
			found = append(found, d)
		}
	}
	switch {
	case len(found) == 0:
		return nil, "", errors.New("no declaration of this work unit's change for the plan (git hint changes --declare-plan --work-unit --after latest)")
	case len(found) > 1:
		return nil, "", errors.New("several declarations of this work unit's change: the binding is ambiguous")
	case found[0].AfterTree == "":
		return nil, "", errors.New("the declared after state has no captured tree (only --after latest captures it): a plan binding is unavailable")
	}
	d := found[0]
	return map[string]any{"recommendation_set_id": d.SetID, "set_digest": d.SetDigest, "plan_id": d.PlanID, "unit_ref": d.Work.UnitRef,
		"revision": d.Work.Revision, "attempt_ref": d.Work.AttemptRef, "after_snapshot_id": d.AfterSnapshotID, "after_tree": d.AfterTree}, d.AfterTree, nil
}
