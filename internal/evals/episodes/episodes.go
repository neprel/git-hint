// Package episodes proposes eval episodes from imported sessions with deterministic rules.
//
// An episode is a local proposal: its ID never becomes an observed task ID. Rules (version
// RulesVersion):
//
//   - Unit: a user request in a session that has no parent (a child session's prompt
//     comes from an agent). Child work joins the parent's episode.
//   - Boundary: a request after a passing check starts a new episode; a request before any
//     passing check is a correction of the current episode.
//   - Closed: an episode is proposed only when a later episode started or when the session
//     was idle for IdleClose. Open work is not proposed.
//   - States: base = the agent snapshot before the episode's first edit step; result = the
//     snapshot after its last step. A snapshot that the library does not hold is
//     unavailable (reason baseline_missing / result_state_missing); nothing is guessed.
//   - Outcome: the last check after the last edit (passed or failed); none otherwise
//     (reason no_checkable_outcome). An agent's or user's claim is not an outcome.
//   - Attribution: episodes of different sessions in the same directory whose activity
//     overlaps in time are ambiguous when they edit a common file and separated when their
//     files are disjoint; otherwise exclusive. Time overlap alone never assigns ownership.
//   - Links: depends_on (same session) and continues (other session) are inferred only
//     when the later episode starts from the earlier result state and edits a common file.
//   - Family: keyword rules on the first request; no match leaves it empty.
package episodes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
)

// RulesVersion is recorded in every episode.
const RulesVersion = "0.1.0"

// IdleClose closes an episode without a later request.
const IdleClose = 30 * time.Minute

var testCommand = regexp.MustCompile(`(^|[\s;&|(])(go test|pytest|python3? -m pytest|npm (run )?test|yarn test|pnpm test|cargo test|make (test|check)|mvn test|gradle test|\./gradlew test|bundle exec rspec|rspec|phpunit|dotnet test|ctest|bun test|vitest|jest)\b`)

var family = []struct {
	name string
	re   *regexp.Regexp
}{
	{"compatibility", regexp.MustCompile(`(?i)\b(api|signature|compatib\w*|deprecat\w*|interface|migrat\w*|backward)\b`)},
	{"refactor", regexp.MustCompile(`(?i)\b(refactor\w*|rename\w*|extract\w*|restructur\w*|clean ?up|reorganiz\w*)\b`)},
	{"bug_fix", regexp.MustCompile(`(?i)\b(fix\w*|bug\w*|broken|error|fail\w*|crash\w*|wrong|incorrect|regression)\b`)},
	{"feature", regexp.MustCompile(`(?i)\b(add\w*|implement\w*|support|introduc\w*|creat\w*|new)\b`)},
}

// Family classifies a request text; "" when no rule matches.
func Family(text string) string {
	for _, f := range family {
		if f.re.MatchString(text) {
			return f.name
		}
	}
	return ""
}

// step is one assistant response or tool call with what it did.
type turn struct {
	request  sessions.Item
	items    []sessions.Item
	edits    []string // relative paths
	checks   []check
	children []string
}

type check struct {
	passed bool
	after  int // seq
}

// Proposal is an episode with the facts that cross-session rules need.
type Proposal struct {
	Record     map[string]any // episode record (schema episode)
	Session    string
	Directory  string
	Requests   []string
	Paths      []string
	Start      int64
	End        int64
	BaseSnap   string
	ResultSnap string
	Outcome    string // test_passed, test_failed, none
	Closed     bool
}

// Snapshots resolves an agent snapshot ID to a HINT tree digest ("" when unavailable).
type Snapshots func(project, id string) string

// Repos maps a session directory to the name of the project repository that holds it ("" when
// the directory is outside the project; such sessions give no episode).
type Repos func(dir string) string

// Discover builds proposals for every imported session. now decides which episodes are
// closed. texts reads stored text objects.
func Discover(l *library.Library, now time.Time, snaps Snapshots, repos Repos) ([]*Proposal, error) {
	all := map[string]sessions.Session{}
	var order []string
	for _, agent := range []string{"opencode", "claude-code", "codex"} {
		names, err := l.Indexes(agent)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			var idx sessions.IndexEntry
			if l.Index(n, &idx) != nil {
				continue
			}
			s, err := sessions.Load(l, idx)
			if err != nil {
				continue
			}
			key := s.Agent + "/" + s.Session
			all[key] = s
			order = append(order, key)
		}
	}
	sort.Strings(order)
	var props []*Proposal
	for _, key := range order {
		s := all[key]
		repo := repos(s.Directory)
		if s.Parent != "" || repo == "" {
			continue
		}
		for _, p := range fromSession(l, s, all, now, snaps) {
			p.Record["repositories"].([]any)[0].(map[string]any)["repo"] = repo
			props = append(props, p)
		}
	}
	attribute(props)
	link(props)
	return props, nil
}

func text(l *library.Library, d string) string {
	if d == "" {
		return ""
	}
	b, err := l.GetObject(d)
	if err != nil {
		return ""
	}
	return string(b)
}

func rel(dir, p string) string {
	if dir != "" && strings.HasPrefix(p, dir+"/") {
		return strings.TrimPrefix(p, dir+"/")
	}
	return p
}

func fromSession(l *library.Library, s sessions.Session, all map[string]sessions.Session, now time.Time, snaps Snapshots) []*Proposal {
	// split into turns at user requests
	var turns []*turn
	for _, it := range s.Items {
		if it.Kind == sessions.KindRequest {
			turns = append(turns, &turn{request: it})
			continue
		}
		if len(turns) == 0 {
			continue
		}
		t := turns[len(turns)-1]
		t.items = append(t.items, it)
		if it.Kind == sessions.KindTool {
			switch it.Tool {
			case sessions.ToolEdit:
				for _, p := range it.Paths {
					t.edits = append(t.edits, rel(s.Directory, p))
				}
			case sessions.ToolShell:
				if testCommand.MatchString(text(l, it.Command)) && it.Status != "unknown" {
					t.checks = append(t.checks, check{passed: it.Status == "completed", after: it.Seq})
				}
			case sessions.ToolChild:
				if it.Child != "" {
					t.children = append(t.children, it.Child)
					if c, ok := all[s.Agent+"/"+it.Child]; ok {
						for _, ci := range c.Items {
							if ci.Kind == sessions.KindTool && ci.Tool == sessions.ToolEdit {
								for _, p := range ci.Paths {
									t.edits = append(t.edits, rel(s.Directory, p))
								}
							}
						}
					}
				}
			}
		}
	}
	// group turns into episodes
	type group struct{ turns []*turn }
	var groups []*group
	for _, t := range turns {
		if len(groups) == 0 || passed(groups[len(groups)-1].turns) {
			groups = append(groups, &group{})
		}
		g := groups[len(groups)-1]
		g.turns = append(g.turns, t)
	}
	var out []*Proposal
	for gi, g := range groups {
		p := build(l, s, all, g.turns, snaps)
		if p == nil {
			continue
		}
		p.Closed = gi < len(groups)-1 || now.Sub(time.UnixMilli(p.End)) >= IdleClose
		if !p.Closed {
			continue
		}
		if s.Truncated && gi == len(groups)-1 && p.Outcome != "test_passed" {
			addReason(p, "source_truncated")
		}
		out = append(out, p)
	}
	return out
}

// passed: the last check after the last edit passed.
func passed(ts []*turn) bool {
	lastEdit, last := -1, (*check)(nil)
	for _, t := range ts {
		for _, it := range t.items {
			if it.Kind == sessions.KindTool && it.Tool == sessions.ToolEdit {
				lastEdit = it.Seq
			}
		}
		for i := range t.checks {
			last = &t.checks[i]
		}
	}
	return last != nil && last.after > lastEdit && last.passed
}

func outcome(ts []*turn) string {
	lastEdit := -1
	var last *check
	for _, t := range ts {
		for _, it := range t.items {
			if it.Kind == sessions.KindTool && it.Tool == sessions.ToolEdit {
				lastEdit = it.Seq
			}
		}
		for i := range t.checks {
			last = &t.checks[i]
		}
	}
	switch {
	case last == nil || last.after < lastEdit:
		return "none"
	case last.passed:
		return "test_passed"
	}
	return "test_failed"
}

func build(l *library.Library, s sessions.Session, all map[string]sessions.Session, ts []*turn, snaps Snapshots) *Proposal {
	var paths []string
	for _, t := range ts {
		paths = append(paths, t.edits...)
	}
	paths = uniqSorted(paths)
	if len(paths) == 0 {
		return nil // no change: not an eval candidate
	}
	loc := func(it sessions.Item) map[string]any {
		m := map[string]any{"source": s.Source, "record": it.Record}
		if it.Line > 0 {
			m["line"] = it.Line
		}
		return m
	}
	ev := func(it sessions.Item) map[string]any {
		m := map[string]any{"at": loc(it)}
		if it.Text != "" {
			m["text"] = it.Text
		}
		return m
	}
	p := &Proposal{Session: s.Session, Directory: s.Directory, Paths: paths, Outcome: outcome(ts)}
	var requests, corrections []any
	var baseSnap, resultSnap string
	var in, out int64
	inKnown, outKnown := true, true
	responses, children := 0, 0
	first, last := ts[0].request, ts[0].request
	for i, t := range ts {
		p.Requests = append(p.Requests, t.request.Record)
		if i == 0 {
			requests = append(requests, ev(t.request))
		} else {
			corrections = append(corrections, ev(t.request))
		}
		children += len(t.children)
		for _, it := range t.items {
			last = it
			if it.Kind == sessions.KindAssistant {
				responses++
				if it.Usage != nil {
					if it.Usage.Input != nil {
						in += *it.Usage.Input
					} else {
						inKnown = false
					}
					if it.Usage.Output != nil {
						out += *it.Usage.Output
					} else {
						outKnown = false
					}
				} else {
					inKnown, outKnown = false, false
				}
			}
		}
	}
	// base: the start snapshot of the first assistant step that edits (OpenCode records
	// the snapshot on the step that holds the tool calls); result: end of the last step
	for _, t := range ts {
		for i, it := range t.items {
			if it.Kind != sessions.KindAssistant {
				continue
			}
			if baseSnap == "" && stepEdits(t.items, i) {
				baseSnap = it.SnapStart
			}
			if it.SnapEnd != "" {
				resultSnap = it.SnapEnd
			}
		}
	}
	p.BaseSnap, p.ResultSnap = baseSnap, resultSnap
	p.Start, p.End = first.Time, last.Time
	if p.End < p.Start {
		p.End = p.Start
	}
	repo := map[string]any{"repo": "", "base_evidence": "unavailable", "files": len(paths), "edited": paths}
	reasons := []string{}
	if d := snaps(s.Project, baseSnap); d != "" {
		repo["base_tree"], repo["base_evidence"] = d, "agent_snapshot"
	} else {
		reasons = append(reasons, "baseline_missing")
	}
	if d := snaps(s.Project, resultSnap); d != "" {
		repo["result_tree"] = d
	} else {
		reasons = append(reasons, "result_state_missing")
	}
	if p.Outcome == "none" {
		reasons = append(reasons, "no_checkable_outcome")
	}
	usage := map[string]any{"requests": responses, "child_sessions": children, "input_tokens": nil, "output_tokens": nil}
	if inKnown && responses > 0 {
		usage["input_tokens"] = in
	}
	if outKnown && responses > 0 {
		usage["output_tokens"] = out
	}
	rec := map[string]any{
		"record": "episode", "manifest_version": 1, "episode_id": id(s.Agent, s.Session, first.Record),
		"created_at": time.Now().UTC().Format(time.RFC3339), "rules_version": RulesVersion,
		"sources": []any{map[string]any{"agent": s.Agent, "agent_version": s.AgentVersion, "parser_version": s.ParserVersion,
			"source": s.Source, "session": s.Session, "role": "primary"}},
		"boundary": map[string]any{"basis": "inferred", "start": loc(first), "end": loc(last), "rule": "request-to-check", "support": "medium"},
		"requests": requests, "corrections": orEmpty(corrections), "outcomes": []any{},
		"repositories": []any{repo}, "links": []any{}, "attribution": "exclusive", "reasons": reasons, "usage": usage,
	}
	if f := Family(text(l, first.Text)); f != "" {
		rec["family"] = f
	}
	p.Record = rec
	return p
}

// stepEdits reports whether the tool calls that follow assistant item i (until the next
// assistant item) include an edit. OpenCode lists a step's tool calls before its text item.
func stepEdits(items []sessions.Item, i int) bool {
	for j := i - 1; j >= 0; j-- {
		it := items[j]
		if it.Kind == sessions.KindAssistant {
			break
		}
		if it.Kind == sessions.KindTool && it.Tool == sessions.ToolEdit {
			return true
		}
	}
	return false
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func id(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "ep-" + hex.EncodeToString(h[:8])
}

func uniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func addReason(p *Proposal, r string) {
	rs, _ := p.Record["reasons"].([]string)
	for _, x := range rs {
		if x == r {
			return
		}
	}
	p.Record["reasons"] = append(rs, r)
}

func overlap(a, b []string) bool {
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if m[x] {
			return true
		}
	}
	return false
}

// attribute applies the cross-session ownership rules.
func attribute(ps []*Proposal) {
	for i, a := range ps {
		for _, b := range ps[i+1:] {
			if a.Session == b.Session || a.Directory != b.Directory || a.End < b.Start || b.End < a.Start {
				continue
			}
			if overlap(a.Paths, b.Paths) {
				for _, p := range []*Proposal{a, b} {
					p.Record["attribution"] = "ambiguous"
					addReason(p, "attribution_ambiguous")
				}
				continue
			}
			for _, p := range []*Proposal{a, b} {
				if p.Record["attribution"] != "ambiguous" {
					p.Record["attribution"] = "separated"
				}
			}
		}
	}
	for _, p := range ps {
		sup := "medium"
		switch {
		case p.Record["attribution"] == "ambiguous":
			sup = "low"
		case p.Outcome == "test_passed":
			sup = "high"
		}
		p.Record["boundary"].(map[string]any)["support"] = sup
	}
}

// link adds inferred depends_on and continues links: the later episode starts from the
// earlier result state and edits a common file.
func link(ps []*Proposal) {
	for _, b := range ps {
		var best *Proposal
		for _, a := range ps {
			if a == b || a.Directory != b.Directory || a.End > b.Start || a.ResultSnap == "" || a.ResultSnap != b.BaseSnap || !overlap(a.Paths, b.Paths) {
				continue
			}
			if best == nil || a.End > best.End {
				best = a
			}
		}
		if best == nil {
			continue
		}
		kind := "continues"
		if best.Session == b.Session {
			kind = "depends_on"
		} else if best.Outcome == "test_passed" {
			kind = "depends_on"
		}
		b.Record["links"] = []any{map[string]any{"kind": kind, "episode_id": best.Record["episode_id"], "status": "inferred"}}
	}
}

// Store writes the proposals as immutable episode manifests and keeps an index of the
// current proposal per episode identity. A changed proposal (for example a later import
// made its attribution ambiguous) is stored under a revision ID; earlier ones stay.
func Store(l *library.Library, ps []*Proposal) (stored, unchanged int, err error) {
	for _, p := range ps {
		base := p.Record["episode_id"].(string)
		for rev := 0; rev < 100; rev++ {
			if rev > 0 {
				p.Record["episode_id"] = id(base, "rev", strings.Repeat("r", rev))
			}
			created := p.Record["created_at"]
			var old map[string]any
			if l.Manifest("episode", p.Record["episode_id"].(string), &old) == nil {
				p.Record["created_at"] = old["created_at"] // the creation time is not content
				if same(old, p.Record) {
					unchanged++
					break
				}
				p.Record["created_at"] = created
				continue
			}
			if _, err = l.PutManifest("episode", p.Record); err != nil {
				if errors.Is(err, library.ErrImmutable) {
					continue
				}
				return
			}
			stored++
			break
		}
		if err = l.PutIndex("episodes/"+base, map[string]any{"current": p.Record["episode_id"]}); err != nil {
			return
		}
	}
	return
}

func same(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
