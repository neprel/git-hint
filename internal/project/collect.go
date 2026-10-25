package project

import (
	"context"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/hint"

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
	// Ctx cancels the scan; a repository is stored only after its complete scan.
	Ctx context.Context
	// TaskID is a task identifier that the agent or orchestrator supplied (raw); it is
	// stored only as an opaque ID on the file-change observations. Never invented.
	TaskID string
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
		res, err := scan.Scan(scan.Options{Ctx: opt.Ctx, Root: s.RepoRoot(r), RepoID: repoID, Key: s.Key(), Cache: cache})
		if err != nil {
			return nil, err
		}
		if opt.Ctx != nil && opt.Ctx.Err() != nil {
			return nil, opt.Ctx.Err()
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
		if changed {
			if cur, err = saveSnapshot(s, cur); err != nil {
				return nil, err
			}
		}
		rr := RepoResult{Name: r.Name, RepoID: repoID, Snapshot: cur, Changed: changed, Scan: res.Stats}
		if changed {
			if prev != nil {
				n, err := recordMutations(s, prev, cur, at, opt.TaskID)
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

func recordMutations(s *store.Store, prev, cur *snapshot.Local, at time.Time, rawTask string) (int, error) {
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
	if rawTask != "" {
		task := TaskID(s.Key(), rawTask)
		for i := range obs {
			if obs[i].Correlation == nil {
				obs[i].Correlation = map[string]string{}
			}
			obs[i].Correlation["task_id"] = task
			obs[i].Missing = withoutField(obs[i].Missing, "correlation.task_id")
		}
	}
	for _, o := range obs {
		if err := AppendObservation(s, o); err != nil {
			return 0, err
		}
	}
	return len(obs), nil
}

func withoutField(l []string, f string) []string {
	out := []string{}
	for _, x := range l {
		if x != f {
			out = append(out, x)
		}
	}
	return out
}

// TaskID is the opaque ID of a task that the agent or orchestrator supplied (hint_collect
// task_id, HINT_TASK_ID of an agent run). The raw value never leaves the machine.
func TaskID(key []byte, raw string) string { return hint.DeriveID(key, "task", raw) }

// ValidTaskID reports whether a supplied task ID can be used: 1 to 200 bytes of printable
// UTF-8 without control characters.
func ValidTaskID(raw string) bool {
	if raw == "" || len(raw) > 200 || !utf8.ValidString(raw) {
		return false
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
