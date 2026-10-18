// Package sources finds session records of the agents in the eval policy and imports the
// new ones into the local library. It reads session files and calls the OpenCode CLI for
// exports; it never starts an agent session, never sends a prompt, and never reads agent
// credential stores.
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
)

// Commander runs a local program and returns its standard output. Tests replace it.
type Commander interface {
	Output(ctx context.Context, dir string, name string, args ...string) ([]byte, error)
}

// Exec runs real programs with a timeout; standard error is discarded (it may hold text).
type Exec struct{ Timeout time.Duration }

func (e Exec) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	t := e.Timeout
	if t <= 0 {
		t = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s failed", name, args[0])
	}
	return out.Bytes(), nil
}

// Options of one discovery pass.
type Options struct {
	Agents      []string  // from the policy
	Roots       []string  // project repository roots
	ImportRoots []string  // policy override of the default session locations
	Home        string    // user home (tests use a temporary one)
	MaxSessions int       // per agent per pass
	Since       time.Time // sessions whose last activity is older are not imported (retention)
	Cmd         Commander
}

// Report counts what one pass did, per agent. It holds no text and no paths.
type Report struct {
	Agent     string         `json:"agent"`
	Found     int            `json:"found"`
	New       int            `json:"new"`
	Updated   int            `json:"updated"`
	Unchanged int            `json:"unchanged"`
	Skipped   int            `json:"skipped"`
	Failed    int            `json:"failed"`
	Reasons   map[string]int `json:"reasons,omitempty"`
}

func (r *Report) fail(code string) {
	r.Failed++
	if r.Reasons == nil {
		r.Reasons = map[string]int{}
	}
	r.Reasons[code]++
}

func code(err error) string {
	switch {
	case errors.Is(err, sessions.ErrUnsupported):
		return "parser_unsupported_version"
	case errors.Is(err, sessions.ErrTooLarge):
		return "source_too_large"
	case errors.Is(err, sessions.ErrFormat):
		return "source_malformed"
	case errors.Is(err, library.ErrQuota):
		return "quota_exceeded"
	}
	return "import_failed"
}

func (r *Report) count(o sessions.Outcome) {
	switch o.State {
	case "new":
		r.New++
	case "updated":
		r.Updated++
	default:
		r.Unchanged++
	}
}

// Run imports new sessions of every policy agent.
func Run(ctx context.Context, l *library.Library, opt Options) []Report {
	if opt.MaxSessions <= 0 {
		opt.MaxSessions = 20
	}
	if opt.Cmd == nil {
		opt.Cmd = Exec{}
	}
	var out []Report
	for _, a := range opt.Agents {
		var r Report
		switch a {
		case "opencode":
			r = openCode(ctx, l, opt)
		case "claude-code":
			r = files(l, opt, "claude-code", claudeFiles(opt), func(b []byte) (sessions.Result, error) { return sessions.ParseClaudeCode(b) })
		case "codex":
			r = files(l, opt, "codex", codexFiles(opt), func(b []byte) (sessions.Result, error) { return sessions.ParseCodex(b) })
		default:
			r = Report{Agent: a}
			r.fail("agent_unsupported")
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenCode: `opencode session list` per repository root, `opencode session export`
// per session; child sessions are found through the task tool of their parent.

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func openCode(ctx context.Context, l *library.Library, opt Options) Report {
	r := Report{Agent: "opencode"}
	vout, err := opt.Cmd.Output(ctx, "", "opencode", "--version")
	if err != nil {
		r.fail("agent_not_installed")
		return r
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(vout)), "opencode ")
	version = strings.TrimPrefix(version, "v")
	// OpenCode 2 talks to a background service by default; --standalone runs a private
	// server, so import works also when no OpenCode app runs (found in the live check)
	var standalone []string
	if strings.HasPrefix(version, "2.") {
		standalone = []string{"--standalone"}
	}
	type ref struct{ id, parent string }
	var queue []ref
	seen := map[string]bool{}
	for _, root := range opt.Roots {
		out, err := opt.Cmd.Output(ctx, root, "opencode", append([]string{"session", "list"}, append(standalone, "--format", "json", "-n", fmt.Sprint(opt.MaxSessions))...)...)
		if err != nil {
			r.fail("session_list_failed")
			continue
		}
		var list []struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
		}
		if json.Unmarshal(out, &list) != nil {
			r.fail("session_list_malformed")
			continue
		}
		for _, s := range list {
			if sessionID.MatchString(s.ID) && !seen[s.ID] && within(opt.Roots, s.Directory) {
				seen[s.ID] = true
				queue = append(queue, ref{id: s.ID})
			}
		}
	}
	limit := opt.MaxSessions * 4 // children included
	for len(queue) > 0 && r.Found < limit {
		cur := queue[0]
		queue = queue[1:]
		r.Found++
		src, err := opt.Cmd.Output(ctx, "", "opencode", append(append([]string{"session", "export"}, standalone...), cur.id)...)
		if err != nil {
			r.fail("session_export_failed")
			continue
		}
		res, err := sessions.ParseOpenCode(src, sessions.OpenCodeOptions{Version: version, Parent: cur.parent})
		if err != nil {
			r.fail(code(err))
			continue
		}
		if tooOld(res.Session, opt.Since) {
			r.Skipped++
			continue
		}
		o, err := sessions.Import(l, src, res)
		if err != nil {
			r.fail(code(err))
			continue
		}
		r.count(o)
		for _, it := range res.Session.Items {
			if it.Child != "" && sessionID.MatchString(it.Child) && !seen[it.Child] {
				seen[it.Child] = true
				queue = append(queue, ref{id: it.Child, parent: cur.id})
			}
		}
	}
	r.Skipped += len(queue)
	return r
}

// Within reports whether dir lies inside one of the roots (paths compared as given and
// with symbolic links resolved).
func Within(roots []string, dir string) bool { return within(roots, dir) }

// LastActivity is the time of the latest timed item of a session (zero when unknown).
func LastActivity(s sessions.Session) time.Time {
	var last int64
	for _, it := range s.Items {
		if it.Time > last {
			last = it.Time
		}
	}
	if last == 0 {
		return time.Time{}
	}
	return time.UnixMilli(last)
}

// tooOld: the session ended before the retention window (a session without times is kept).
func tooOld(s sessions.Session, since time.Time) bool {
	t := LastActivity(s)
	return !since.IsZero() && !t.IsZero() && t.Before(since)
}

// within reports whether dir lies inside one of the roots.
func within(roots []string, dir string) bool {
	if dir == "" {
		return false
	}
	forms := func(p string) []string {
		out := []string{filepath.Clean(p)}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			out = append(out, r)
		}
		return out
	}
	for _, root := range roots {
		for _, r := range forms(root) {
			for _, d := range forms(dir) {
				rel, err := filepath.Rel(r, d)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// file-based imports (Claude Code, Codex)

type file struct {
	path, parent string
	size         int64
	mod          int64
}

// fileState remembers size and time of imported files so that an unchanged file is not
// read again. It is local bookkeeping (paths stay on the machine).
type fileState struct {
	Files map[string][2]int64 `json:"files"`
}

func files(l *library.Library, opt Options, agent string, cands []file, parse func([]byte) (sessions.Result, error)) Report {
	r := Report{Agent: agent}
	var st fileState
	_ = l.Index("sources/"+agent, &st)
	if st.Files == nil {
		st.Files = map[string][2]int64{}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod > cands[j].mod })
	done := 0
	for _, f := range cands {
		r.Found++
		if prev, ok := st.Files[f.path]; ok && prev == [2]int64{f.size, f.mod} {
			r.Unchanged++
			continue
		}
		if done >= opt.MaxSessions {
			r.Skipped++
			continue
		}
		done++
		if f.size > sessions.MaxSourceBytes {
			r.fail("source_too_large")
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			r.fail("source_unreadable")
			continue
		}
		res, err := parse(b)
		if err != nil {
			r.fail(code(err))
			st.Files[f.path] = [2]int64{f.size, f.mod} // do not retry an unsupported file every pass
			continue
		}
		if tooOld(res.Session, opt.Since) {
			r.Skipped++
			st.Files[f.path] = [2]int64{f.size, f.mod}
			continue
		}
		if agent == "codex" && !within(opt.Roots, res.Session.Directory) {
			r.Skipped++
			st.Files[f.path] = [2]int64{f.size, f.mod}
			continue
		}
		if f.parent != "" && res.Session.Parent == "" {
			res.Session.Parent = f.parent
		}
		o, err := sessions.Import(l, b, res)
		if err != nil {
			r.fail(code(err))
			continue
		}
		r.count(o)
		st.Files[f.path] = [2]int64{f.size, f.mod}
	}
	if err := l.PutIndex("sources/"+agent, st); err != nil {
		r.fail(code(err))
	}
	return r
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeFiles lists `~/.claude/projects/<encoded root>/*.jsonl` and their subagent files.
func claudeFiles(opt Options) []file {
	dirs := opt.ImportRoots
	if len(dirs) == 0 {
		for _, root := range opt.Roots {
			dirs = append(dirs, filepath.Join(opt.Home, ".claude", "projects", nonAlnum.ReplaceAllString(filepath.Clean(root), "-")))
		}
	}
	var out []file
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			p := filepath.Join(d, e.Name())
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, stat(p, ""))
			}
			if e.IsDir() {
				subs, _ := filepath.Glob(filepath.Join(p, "subagents", "*.jsonl"))
				for _, s := range subs {
					out = append(out, stat(s, e.Name()))
				}
			}
		}
	}
	return clean(out)
}

// codexFiles lists rollout files below `~/.codex/sessions/` only (never other files of
// the Codex home, such as its authentication file).
func codexFiles(opt Options) []file {
	dirs := opt.ImportRoots
	if len(dirs) == 0 {
		dirs = []string{filepath.Join(opt.Home, ".codex", "sessions")}
	}
	var out []file
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, stat(p, ""))
			}
			return nil
		})
	}
	return clean(out)
}

func stat(p, parent string) file {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() { // symbolic links are not followed
		return file{}
	}
	return file{path: p, parent: parent, size: fi.Size(), mod: fi.ModTime().UnixNano()}
}

func clean(in []file) []file {
	var out []file
	for _, f := range in {
		if f.path != "" {
			out = append(out, f)
		}
	}
	return out
}
