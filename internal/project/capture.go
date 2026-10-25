package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// TreeProvenance is the local record of an after-state capture (never sent).
type TreeProvenance struct {
	Method     string `json:"method,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
	Head       string `json:"head,omitempty"`
	State      string `json:"worktree_state,omitempty"`
}

// captureAfter captures the work tree of a repository as the declared after state of the
// stored snapshot snap: the canonical tree (paths, modes, content, symlink targets) is stored,
// the structural snapshot is derived from that same stored tree (materialized privately and
// scanned with its own file list) and must have snap's ID; a second read of the work tree and
// of the Git state must equal the first. This bounds mutations during the capture; it is not
// an atomic snapshot. Any difference, read error or unsupported tree refuses (nothing stored
// in the declaration).
func captureAfter(ctx context.Context, s *store.Store, r store.Repo, snap *snapshot.Local, now time.Time) (string, TreeProvenance, error) {
	root := s.RepoRoot(r)
	l := library.Open(s, 0)
	g0 := snapshot.ReadGitState(root)
	t1, err := trees.FromWorktree(ctx, l, root, trees.Limits{})
	if err != nil {
		return "", TreeProvenance{}, fmt.Errorf("the after state cannot be captured: %w", err)
	}
	tmp, err := os.MkdirTemp(s.Path("cache"), "capture-")
	if err != nil {
		if err = os.MkdirAll(s.Path("cache"), 0o700); err == nil {
			tmp, err = os.MkdirTemp(s.Path("cache"), "capture-")
		}
		if err != nil {
			return "", TreeProvenance{}, err
		}
	}
	defer os.RemoveAll(tmp)
	if err := trees.Materialize(l, t1, tmp); err != nil {
		return "", TreeProvenance{}, fmt.Errorf("the after state cannot be captured: %w", err)
	}
	tree, err := trees.Load(l, t1)
	if err != nil {
		return "", TreeProvenance{}, err
	}
	files := []string{}
	for _, f := range tree.Files {
		if f.Mode != "120000" {
			files = append(files, f.Path)
		}
	}
	res, err := scan.Scan(scan.Options{Ctx: ctx, Root: tmp, RepoID: snap.Record.RepoID, Key: s.Key(), Files: files})
	if err != nil {
		return "", TreeProvenance{}, fmt.Errorf("the captured tree cannot be scanned: %w", err)
	}
	derived, _, err := snapshot.Build(nil, res, snap.Record.RepoID, s.Key(), g0, now)
	if err != nil {
		return "", TreeProvenance{}, err
	}
	// The state key, not the snapshot ID: a successor ID also binds its predecessor.
	if derived.StateKey != snap.StateKey {
		return "", TreeProvenance{}, errors.New("the captured work tree is not the after snapshot (collect first, and declare before further edits)")
	}
	t2, err := trees.FromWorktree(ctx, l, root, trees.Limits{})
	if err != nil || t2 != t1 || snapshot.ReadGitState(root) != g0 {
		return "", TreeProvenance{}, errors.New("the work tree changed during the capture; nothing was declared")
	}
	return t1, TreeProvenance{Method: "trees.FromWorktree+derived-snapshot", CapturedAt: now.UTC().Format(time.RFC3339), Head: g0.Head, State: g0.State}, nil
}
