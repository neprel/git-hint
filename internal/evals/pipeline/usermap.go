package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// MapFile is the user's requirement mapping file of one exact eval version
// (git hint eval map). Sources, Tests and Draft are information for the person; apply
// ignores them.
type MapFile struct {
	MapVersion   int               `json:"map_version"`
	Eval         string            `json:"eval"`   // eval ID @ version
	Digest       string            `json:"digest"` // digest of that version's record
	Draft        string            `json:"draft,omitempty"`
	Sources      map[string]string `json:"sources,omitempty"`
	Tests        []string          `json:"tests,omitempty"`
	Requirements []build.Link      `json:"requirements"`
	Context      []string          `json:"context"`
	Alternatives []MapAlternative  `json:"alternatives"`
}

// MapAlternative names an existing alternative solution: a Git work tree whose current
// state solves the task. ClaimedBy "user" states that it was made independently of the
// reference; HINT records the claim and does not check it.
type MapAlternative struct {
	Worktree  string `json:"worktree"`
	ClaimedBy string `json:"claimed_by,omitempty"`
}

// ErrStale marks a mapping file of an older or changed eval version.
var ErrStale = errors.New("the mapping file does not belong to the latest version of this eval")

// MapDraft returns the mapping file of an eval version. Earlier links (by the model or a
// user) are a draft: each atomic requirement appears with its tests; a model's context
// sentence appears as a requirement without tests.
func MapDraft(l *library.Library, ref string) (MapFile, error) {
	var rec map[string]any
	if err := l.Manifest("eval", ref, &rec); err != nil {
		return MapFile{}, err
	}
	d, err := l.ManifestDigest("eval", ref)
	if err != nil {
		return MapFile{}, err
	}
	orig := build.Originals(rec)
	f := MapFile{MapVersion: 1, Eval: ref, Digest: d, Sources: map[string]string{}, Context: []string{}, Alternatives: []MapAlternative{}}
	ids := map[string]string{} // original text digest -> requirement ID
	for _, r := range orig["requirements"].([]any) {
		m := r.(map[string]any)
		if m["mapping"] != "none" {
			continue
		}
		b, err := l.GetObject(m["text"].(string))
		if err != nil {
			return MapFile{}, err
		}
		ids[m["text"].(string)] = m["req_id"].(string)
		f.Sources[m["req_id"].(string)] = string(b)
	}
	f.Tests = build.C1Tests(rec)
	if build.PrimaryCheck(rec) == "C1" { // a cli-file-v1 eval offers only its declared cases
		f.Tests = append(f.Tests, "C2")
		if len(build.C1Tests(rec)) == 0 {
			f.Tests = []string{"C1", "C2"}
		}
	}
	drafted := map[string]bool{}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		if m["origin"] != "request" && m["origin"] != "correction" {
			continue
		}
		src, _ := m["source"].(string)
		if src == "" {
			continue // an original that no mapping split
		}
		q, err := l.GetObject(m["text"].(string))
		if err != nil {
			return MapFile{}, err
		}
		ln := build.Link{Source: ids[src], Quote: string(q), Tests: linkTests(m)}
		f.Requirements = append(f.Requirements, ln)
		drafted[ids[src]] = true
		if by, _ := m["mapping"].(string); by == "model" && f.Draft == "" {
			f.Draft = "model estimate: check every link and move background sentences to context"
		}
	}
	for _, r := range orig["requirements"].([]any) {
		m := r.(map[string]any)
		id := m["req_id"].(string)
		if m["mapping"] == "none" && !drafted[id] {
			f.Requirements = append(f.Requirements, build.Link{Source: id, Quote: f.Sources[id], Tests: []string{}})
		}
	}
	cs, _ := rec["context"].([]any)
	for _, c := range cs {
		if b, err := l.GetObject(c.(map[string]any)["text"].(string)); err == nil {
			f.Context = append(f.Context, string(b))
		}
	}
	if as, _ := rec["alternatives"].([]any); len(as) > 0 {
		f.Draft = strings.TrimSpace(f.Draft + " (stored alternatives stay unless the file names new ones)")
	}
	return f, nil
}

// linkTests returns the tests of a stored requirement, or its whole check.
func linkTests(m map[string]any) []string {
	if m["mapping"] == "none" {
		return []string{}
	}
	var out []string
	if ts, ok := m["tests"].([]any); ok {
		for _, t := range ts {
			out = append(out, t.(string))
		}
		return out
	}
	for _, c := range m["checks"].([]any) {
		out = append(out, c.(string))
	}
	return out
}

// ErrTarget marks a mapping file that names another eval than the one the user named.
var ErrTarget = errors.New("the mapping file belongs to another eval")

// ApplyMapFile checks a mapping file against the eval the user named (want: an eval ID,
// or ID@version, which must match exactly) and the latest version of that eval, and
// stores a new packaged version with the user's links. It never sets validated:
// qualification does that. base is the directory of the file (relative work tree paths
// start there). It returns the new version and its unmapped requirements. Nothing is
// written when a check fails.
func ApplyMapFile(ctx context.Context, l *library.Library, want string, data []byte, base string, now time.Time) (string, []string, error) {
	var f MapFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return "", nil, fmt.Errorf("the mapping file is not valid: %v", err)
	}
	if f.MapVersion != 1 {
		return "", nil, fmt.Errorf("unsupported map_version %d", f.MapVersion)
	}
	id, _, _ := strings.Cut(f.Eval, "@")
	if wantID, _, exact := strings.Cut(want, "@"); want == "" || wantID != id || exact && want != f.Eval {
		return "", nil, fmt.Errorf("%w: you named %s, the file names %s", ErrTarget, want, f.Eval)
	}
	latest := ""
	names, _ := l.Manifests("eval")
	for _, n := range names {
		if strings.HasPrefix(n, id+"@") && (latest == "" || versionOf(strings.SplitN(n, "@", 2)[1]) > versionOf(strings.SplitN(latest, "@", 2)[1])) {
			latest = n
		}
	}
	if latest == "" {
		return "", nil, fmt.Errorf("no eval %s", f.Eval)
	}
	d, err := l.ManifestDigest("eval", latest)
	if err != nil {
		return "", nil, err
	}
	if latest != f.Eval || d != f.Digest {
		return "", nil, fmt.Errorf("%w (latest: %s)", ErrStale, latest)
	}
	var rec map[string]any
	if err := l.Manifest("eval", latest, &rec); err != nil {
		return "", nil, err
	}
	// a link that repeats a model link unchanged keeps the model's provenance
	gen := map[string]any{}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		if g, ok := m["generator"]; ok {
			if q, err := l.GetObject(m["text"].(string)); err == nil {
				gen[string(q)+"\x00"+fmt.Sprint(linkTests(m))] = g
			}
		}
	}
	orig := build.Originals(rec)
	reqs, err := build.ApplyMapping(l, orig, build.Mapping{By: "user", Links: f.Requirements, Context: f.Context})
	if err != nil {
		return "", nil, err
	}
	for _, r := range reqs {
		m := r.(map[string]any)
		if m["mapping"] != "user" {
			continue
		}
		q, _ := l.GetObject(m["text"].(string))
		if g, ok := gen[strings.TrimSpace(string(q))+"\x00"+fmt.Sprint(linkTests(m))]; ok {
			m["generator"] = g // the user confirmed a model estimate
		}
	}
	next := map[string]any{}
	for k, v := range rec {
		next[k] = v
	}
	// the context the user confirmed, with the original it belongs to
	delete(next, "context")
	var ctxs []any
	for _, c := range f.Context {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		src := ""
		for _, r := range orig["requirements"].([]any) {
			m := r.(map[string]any)
			if b, err := l.GetObject(m["text"].(string)); err == nil && m["mapping"] == "none" && strings.Contains(norm(string(b)), norm(c)) {
				src = m["text"].(string)
				break
			}
		}
		d, err := l.PutObject([]byte(c))
		if err != nil {
			return "", nil, err
		}
		ctxs = append(ctxs, map[string]any{"text": d, "source": src, "by": "user"})
	}
	if len(ctxs) > 0 {
		next["context"] = ctxs
	}
	next["qualification"] = map[string]any{"policy": qualify.Policy, "checks": []any{}} // not qualified yet
	v := int(rec["version"].(float64))
	next["version"] = v + 1
	next["previous"] = latest
	next["created_at"] = now.UTC().Format(time.RFC3339)
	next["state"] = "packaged"
	next["requirements"] = reqs
	if len(f.Alternatives) > 0 {
		var alts []any
		for _, a := range f.Alternatives {
			if a.ClaimedBy != "" && a.ClaimedBy != "user" {
				return "", nil, fmt.Errorf("claimed_by must be \"user\" or empty")
			}
			dir := a.Worktree
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(base, dir)
			}
			tree, err := trees.FromWorktree(ctx, l, dir, trees.Limits{})
			if err != nil {
				return "", nil, fmt.Errorf("alternative %s: %v", a.Worktree, err)
			}
			m := map[string]any{"tree": tree, "source": "user"}
			if a.ClaimedBy != "" {
				m["claimed_by"] = a.ClaimedBy
			}
			alts = append(alts, m)
		}
		next["alternatives"] = alts
	}
	unmapped := build.Unmapped(next)
	reasons := []any{}
	if len(unmapped) > 0 {
		reasons = append(reasons, "requirement_coverage_missing")
	}
	next["reasons"] = reasons
	name, err := l.PutManifest("eval", next)
	if err != nil {
		return "", nil, err
	}
	return name, unmapped, nil
}

func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
