package project

import (
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// RepoResult is the collect result of one repository.
type RepoResult struct {
	Name      string
	RepoID    string
	Snapshot  *snapshot.Local
	Changed   bool
	Mutations int
	Scan      scan.Stats
	Duration  time.Duration
}

// CollectOptions control `git hint collect`.
type CollectOptions struct {
	Full bool // ignore the scan cache (full baseline)
	Now  func() time.Time
}

// Collect scans every configured repository. It stores a new snapshot when the state
// changed, and records the changed files as FILE_MUTATION observations. It needs no
// server access.
func Collect(s *store.Store, opt CollectOptions) ([]RepoResult, error) {
	if err := s.CheckExcluded(); err != nil {
		return nil, err
	}
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	var out []RepoResult
	for _, r := range s.Cfg.Repos {
		t0 := time.Now()
		repoID := s.RepoID(r)
		cachePath := s.Path("cache", "scan-"+repoID+".json")
		var cache *scan.Cache
		if opt.Full {
			cache = &scan.Cache{Version: scan.CacheVersion, Files: map[string]*scan.FileEntry{}}
		} else {
			cache = scan.LoadCache(cachePath)
		}
		res, err := scan.Scan(scan.Options{Root: s.RepoRoot(r), RepoID: repoID, Key: s.Key(), Cache: cache})
		if err != nil {
			return nil, err
		}
		if err := s.CheckPath(cachePath); err != nil {
			return nil, err
		}
		if err := cache.Save(cachePath); err != nil {
			return nil, err
		}
		prev, err := LoadLatest(s, repoID)
		if err != nil {
			return nil, err
		}
		at := now()
		cur, changed, err := snapshot.Build(prev, res, repoID, s.Key(), snapshot.ReadGitState(s.RepoRoot(r)), at)
		if err != nil {
			return nil, err
		}
		rr := RepoResult{Name: r.Name, RepoID: repoID, Snapshot: cur, Changed: changed, Scan: res.Stats}
		if changed {
			if err := saveSnapshot(s, cur); err != nil {
				return nil, err
			}
			if prev != nil {
				n, err := recordMutations(s, prev, cur, at)
				if err != nil {
					return nil, err
				}
				rr.Mutations = n
			}
		}
		rr.Duration = time.Since(t0)
		out = append(out, rr)
	}
	return out, nil
}

func recordMutations(s *store.Store, prev, cur *snapshot.Local, at time.Time) (int, error) {
	_, local := snapshot.Diff(s.Key(), prev, cur)
	var files []activity.FileChange
	for _, c := range local {
		if c.Level != "artifact" {
			continue
		}
		switch c.Operation {
		case "ADD", "MODIFY":
			files = append(files, activity.FileChange{EntityID: c.After[0]})
		case "DELETE":
			files = append(files, activity.FileChange{EntityID: c.Before[0]})
		case "RENAME", "AMBIGUOUS":
			files = append(files, activity.FileChange{EntityID: c.Before[0]})
			for _, a := range c.After {
				files = append(files, activity.FileChange{EntityID: a})
			}
		}
	}
	if len(files) == 0 {
		return 0, nil
	}
	since, err := time.Parse(time.RFC3339, prev.Record.CapturedAt)
	if err != nil {
		since = time.Time{}
	}
	recent, _, err := ReadSpool(s)
	if err != nil {
		return 0, err
	}
	if err := RegisterSource(s, activity.WorkspaceSource(s.Key())); err != nil {
		return 0, err
	}
	obs := activity.Mutations(s.Key(), prev.Record.SnapshotID, cur.Record.SnapshotID, files, since, at, recent)
	for _, o := range obs {
		if err := AppendObservation(s, o); err != nil {
			return 0, err
		}
	}
	return len(obs), nil
}
