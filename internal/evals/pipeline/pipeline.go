// Package pipeline runs the automatic eval pipeline in one finite pass: import new
// sessions, keep exact states, propose episodes, package and qualify evals. It runs only
// when the local eval policy allows it, within the policy's batch limits, and it never
// loops on its own: callers (collect, the OpenCode adapter, `git hint eval`) start a pass.
package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
	"github.com/neprel/git-hint/internal/evals/sources"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/store"
)

// Env holds the inputs a pass needs besides the project store; tests replace them.
type Env struct {
	Home     string            // user home
	DataHome string            // XDG data home (OpenCode snapshots)
	Cmd      sources.Commander // local programs
	Now      func() time.Time
}

// DefaultEnv uses the real user environment.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return Env{Home: home, DataHome: data, Cmd: sources.Exec{}, Now: time.Now}
}

// Result of one pass. It holds counts and reason codes only.
type Result struct {
	Ran       bool             `json:"ran"`
	Blocked   string           `json:"blocked,omitempty"` // why the pass did not run
	Imports   []sources.Report `json:"imports,omitempty"`
	Snapshots SnapshotReport   `json:"snapshots"`
	Stopped   string           `json:"stopped,omitempty"` // time_limit when the batch time ran out
	Elapsed   float64          `json:"elapsed_seconds"`
	Extra     map[string]any   `json:"extra,omitempty"`
}

// SnapshotReport counts agent snapshot trees kept in the library.
type SnapshotReport struct {
	Kept        int            `json:"kept"`
	Known       int            `json:"known"`
	Unavailable map[string]int `json:"unavailable,omitempty"`
}

// Run executes one pass for the project.
func Run(ctx context.Context, s *store.Store, env Env) Result {
	p := s.Cfg.Evals
	if err := p.Check(evals.OpCapture, ""); err != nil {
		return Result{Blocked: err.Error()}
	}
	lim := p.Limits()
	start := env.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(lim.BatchSeconds)*time.Second)
	defer cancel()
	l := library.Open(s, lim.QuotaBytes)
	if _, err := l.Recover(); err != nil {
		return Result{Blocked: "the eval library cannot be opened"}
	}
	res := Result{Ran: true}
	var roots []string
	for _, r := range s.Cfg.Repos {
		roots = append(roots, s.RepoRoot(r))
	}
	res.Imports = sources.Run(ctx, l, sources.Options{Agents: p.Agents, Roots: roots, ImportRoots: p.ImportRoots,
		Home: env.Home, MaxSessions: lim.BatchEpisodes, Cmd: env.Cmd})
	res.Snapshots = keepSnapshots(ctx, l, env.DataHome)
	if ctx.Err() != nil {
		res.Stopped = "time_limit"
	}
	res.Elapsed = env.Now().Sub(start).Seconds()
	return res
}

// snapMap records, per agent snapshot tree ID, the HINT tree or the reason it is missing.
type snapMap struct {
	Trees       map[string]string `json:"trees"`
	Unavailable map[string]string `json:"unavailable"`
}

// keepSnapshots copies every OpenCode snapshot tree that imported sessions name into the
// library, so a later prune by OpenCode cannot remove a baseline. A tree that is already
// gone is recorded as unavailable; it is never replaced by a later state.
func keepSnapshots(ctx context.Context, l *library.Library, dataHome string) SnapshotReport {
	rep := SnapshotReport{Unavailable: map[string]int{}}
	names, _ := l.Indexes("opencode")
	for _, n := range names {
		var idx sessions.IndexEntry
		if l.Index(n, &idx) != nil {
			continue
		}
		s, err := sessions.Load(l, idx)
		if err != nil || s.Project == "" {
			continue
		}
		mapName := "snapmap/" + s.Project
		var m snapMap
		_ = l.Index(mapName, &m)
		if m.Trees == nil {
			m.Trees, m.Unavailable = map[string]string{}, map[string]string{}
		}
		dirs := trees.OpenCodeSnapshotDirs(dataHome, s.Project)
		changed := false
		for _, it := range s.Items {
			for _, id := range []string{it.SnapStart, it.SnapEnd} {
				if id == "" {
					continue
				}
				if _, ok := m.Trees[id]; ok {
					continue
				}
				if _, ok := m.Unavailable[id]; ok {
					continue
				}
				if ctx.Err() != nil {
					break
				}
				d, err := trees.FromGitTree(ctx, l, dirs, id, trees.Limits{})
				changed = true
				if err != nil {
					code := "baseline_missing"
					if u, ok := err.(*trees.Unsupported); ok {
						code = u.Code
					}
					m.Unavailable[id] = code
					continue
				}
				m.Trees[id] = d
			}
		}
		if changed {
			_ = l.PutIndex(mapName, m)
		}
		rep.Kept += len(m.Trees)
		rep.Known += len(m.Trees) + len(m.Unavailable)
		for _, c := range m.Unavailable {
			rep.Unavailable[c]++
		}
	}
	return rep
}
