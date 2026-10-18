package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/sessions"
	"github.com/neprel/git-hint/internal/store"
)

// PackageReport counts the packaging stage of a pass.
type PackageReport struct {
	Packaged int            `json:"packaged"`
	Refused  int            `json:"refused"`
	Reasons  map[string]int `json:"reasons,omitempty"`
}

// evalMap records what happened to an episode: its eval version, or the refusal reasons.
type evalMap struct {
	Eval    string   `json:"eval,omitempty"`
	Refused []string `json:"refused,omitempty"`
}

var testCmd = regexp.MustCompile(`(^|[\s;&|(])(go test|pytest|python3? -m pytest)\b`)

// checkOf finds the last test command between the episode's first and last records and
// whether it passed. The command text only selects a check template later.
func checkOf(l *library.Library, s sessions.Session, start, end string) (cmd string, passed, found bool) {
	in := false
	for _, it := range s.Items {
		if it.Record == start {
			in = true
		}
		if in && it.Kind == sessions.KindTool && it.Tool == sessions.ToolShell && it.Command != "" {
			b, err := l.GetObject(it.Command)
			if err == nil && testCmd.Match(b) && it.Status != "unknown" {
				cmd, passed, found = string(b), it.Status == "completed", true
			}
		}
		if in && it.Kind == sessions.KindTool && it.Tool == sessions.ToolEdit {
			found = false // a later edit voids an earlier check
		}
		if it.Record == end {
			break
		}
	}
	return
}

func textOf(l *library.Library, v any) string {
	m, _ := v.(map[string]any)
	d, _ := m["text"].(string)
	if d == "" {
		return ""
	}
	b, err := l.GetObject(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// packageEpisodes builds an eval version for every current episode that has none yet.
func packageEpisodes(l *library.Library, p *evals.Policy, now time.Time) PackageReport {
	rep := PackageReport{Reasons: map[string]int{}}
	names, _ := l.Indexes("episodes")
	for _, n := range names {
		var cur struct {
			Current string `json:"current"`
		}
		if l.Index(n, &cur) != nil || cur.Current == "" {
			continue
		}
		var done evalMap
		if l.Index("evalmap/"+cur.Current, &done) == nil {
			continue
		}
		var ep map[string]any
		if l.Manifest("episode", cur.Current, &ep) != nil {
			continue
		}
		res := packageOne(l, ep, p, now)
		if res.Eval != "" {
			rep.Packaged++
		} else {
			rep.Refused++
			for _, r := range res.Refused {
				rep.Reasons[r]++
			}
		}
		_ = l.PutIndex("evalmap/"+cur.Current, res)
	}
	return rep
}

func packageOne(l *library.Library, ep map[string]any, p *evals.Policy, now time.Time) evalMap {
	in, refused := episodeInput(l, ep, now)
	if refused != nil {
		return evalMap{Refused: refused}
	}
	built, err := build.Build(l, in)
	var ref *build.Refusal
	if errors.As(err, &ref) && len(ref.Reasons) == 1 && ref.Reasons[0] == "verifier_missing" {
		// no test of its own: a local model draft may supply a hidden check (policy model)
		if drafted, ok := draftVerifier(context.Background(), l, p, in); ok {
			built, err = build.Build(l, drafted)
			in.Result = drafted.Result
		}
	}
	if errors.As(err, &ref) {
		return evalMap{Refused: ref.Reasons}
	}
	if err != nil {
		return evalMap{Refused: []string{"verifier_missing"}}
	}
	rec, why := mapRequirements(context.Background(), l, p, built, in.Result)
	if why != "" {
		rec["reasons"] = append(rec["reasons"].([]any), why)
	}
	if p.Check(evals.OpExecute, "") != nil {
		rec["reasons"] = append(rec["reasons"].([]any), "execution_disabled")
	}
	name, err := l.PutManifest("eval", rec)
	if err != nil {
		if errors.Is(err, library.ErrQuota) {
			return evalMap{Refused: []string{"quota_exceeded"}}
		}
		return evalMap{Refused: []string{"verifier_missing"}}
	}
	return evalMap{Eval: name}
}

// episodeInput reads the packaging input of an episode: its repository states, request,
// corrections and the session's check. It returns refusal reasons when it cannot.
func episodeInput(l *library.Library, ep map[string]any, now time.Time) (build.Input, []string) {
	repos, _ := ep["repositories"].([]any)
	srcs, _ := ep["sources"].([]any)
	reqs, _ := ep["requests"].([]any)
	if len(repos) != 1 || len(srcs) == 0 || len(reqs) == 0 {
		return build.Input{}, []string{"requirements_missing"}
	}
	repo := repos[0].(map[string]any)
	src := srcs[0].(map[string]any)
	var idx sessions.IndexEntry
	agent, _ := src["agent"].(string)
	session, _ := src["session"].(string)
	if l.Index(agent+"/"+safe(session), &idx) != nil {
		return build.Input{}, []string{"requirements_missing"}
	}
	s, err := sessions.Load(l, idx)
	if err != nil {
		return build.Input{}, []string{"requirements_missing"}
	}
	b := ep["boundary"].(map[string]any)
	start := b["start"].(map[string]any)["record"].(string)
	end := start
	if e, ok := b["end"].(map[string]any); ok {
		end = e["record"].(string)
	}
	cmd, passed, found := checkOf(l, s, start, end)
	outcome := "none"
	if found {
		outcome = "test_failed"
		if passed {
			outcome = "test_passed"
		}
	}
	in := build.Input{EpisodeID: ep["episode_id"].(string), Repo: str(repo["repo"]), Base: str(repo["base_tree"]), Result: str(repo["result_tree"]),
		Attribution: str(ep["attribution"]), Outcome: outcome, Request: textOf(l, reqs[0]), CheckHint: cmd, Now: now}
	in.RequestAt, _ = reqs[0].(map[string]any)["at"].(map[string]any)
	if f, ok := ep["family"].(string); ok {
		in.Family = f
	}
	if e, ok := repo["edited"].([]any); ok {
		for _, x := range e {
			in.Paths = append(in.Paths, str(x))
		}
	}
	if cs, ok := ep["corrections"].([]any); ok {
		for _, c := range cs {
			at, _ := c.(map[string]any)["at"].(map[string]any)
			in.Corrections = append(in.Corrections, build.Text{Body: textOf(l, c), At: at})
		}
	}
	return in, nil
}

func str(v any) string { s, _ := v.(string); return s }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safe(s string) string { return unsafeName.ReplaceAllString(s, "_") }

// QualifyReport counts the qualification stage.
type QualifyReport struct {
	Blocked   string         `json:"blocked,omitempty"`
	Qualified int            `json:"qualified"`
	Kept      int            `json:"run_dirs_kept,omitempty"` // results that could not be archived stay on disk
	States    map[string]int `json:"states,omitempty"`
	Trials    int            `json:"trials"`
	Stopped   string         `json:"stopped,omitempty"`
}

// Pending lists the latest versions of evals that wait for qualification.
func Pending(l *library.Library) []string {
	names, _ := l.Manifests("eval")
	latest := map[string]string{}
	for _, n := range names {
		id, v, _ := strings.Cut(n, "@")
		if cur, ok := latest[id]; !ok || versionOf(v) > versionOf(strings.SplitN(cur, "@", 2)[1]) {
			latest[id] = n
		}
	}
	var out []string
	for _, n := range latest {
		var rec map[string]any
		if l.Manifest("eval", n, &rec) == nil && rec["state"] == "packaged" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func versionOf(v string) int {
	var n int
	fmt.Sscan(v, &n)
	return n
}

// Qualify runs the qualification of pending evals within the policy's trial budget. The
// budget is journaled: a restart resumes it and cannot reset it.
func Qualify(ctx context.Context, s *store.Store, r qualify.Runner, now time.Time) QualifyReport {
	rep := QualifyReport{States: map[string]int{}}
	p := s.Cfg.Evals
	if err := p.Check(evals.OpExecute, ""); err != nil {
		rep.Blocked = err.Error()
		return rep
	}
	lim := p.Limits()
	l := library.Open(s, lim.QuotaBytes)
	h := sha256.Sum256([]byte("qualify\x00" + now.UTC().Format(time.RFC3339Nano)))
	job := "jb-" + hex.EncodeToString(h[:8])
	if err := l.StartJob(job, "qualify", library.Budget{Trials: lim.Trials, Seconds: lim.Trials * lim.TrialSeconds}); err != nil {
		rep.Blocked = "the trial budget cannot be recorded"
		return rep
	}
	defer l.EndJob(job, "done")
	settings := runner.DefaultSettings
	settings.AgentSeconds, settings.VerifierSeconds = lim.TrialSeconds, lim.TrialSeconds
	if p.Run != nil {
		if p.Run.CPUs > 0 {
			settings.CPUs = p.Run.CPUs
		}
		if p.Run.MemoryMB > 0 {
			settings.MemoryMB = p.Run.MemoryMB
		}
	}
	for _, name := range Pending(l) {
		st, _ := l.Job(job)
		var rec map[string]any
		if l.Manifest("eval", name, &rec) != nil {
			continue
		}
		alts := Alternatives(rec)
		if ok, why := st.Remaining(); !ok || st.Trials+qualifyTrials+len(alts) > lim.Trials {
			if why == "" {
				why = "trial_limit"
			}
			rep.Stopped = why
			break
		}
		work := s.Path("cache", "evals", "work", strings.ReplaceAll(name, "@", "-")+"-"+job)
		_ = os.RemoveAll(work)
		if err := os.MkdirAll(work, 0o700); err != nil {
			continue
		}
		_ = l.RecordExternal(library.External{Kind: "job_dir", Name: work})
		t0 := time.Now()
		res, err := qualify.Run(ctx, l, r, rec, qualify.Options{Work: work, Settings: settings, Alternatives: alts})
		_ = l.Consume(job, 0, 0, res.Trials, time.Since(t0).Seconds())
		rep.Trials += res.Trials
		if err != nil {
			rep.States["error"]++
			continue
		}
		evidence, kerr := keepRun(l, work)
		next := nextVersion(rec, res, evidence, now)
		if _, err := l.PutManifest("eval", next); err != nil {
			rep.States["store_failed"]++
			continue // the work directory stays: it holds the only copy of the results
		}
		rep.Qualified++
		rep.States[res.State]++
		if kerr != nil {
			rep.Kept++ // results not archived: the directory stays, listed in the external inventory
			continue
		}
		_ = os.RemoveAll(work)
		_ = l.RecordExternal(library.External{Kind: "job_dir", Name: work, Removed: true})
	}
	return rep
}

// Alternatives returns the existing alternative solutions stored with an eval version,
// with their provenance and the independence claim as recorded.
func Alternatives(rec map[string]any) []qualify.Alternative {
	var out []qualify.Alternative
	as, _ := rec["alternatives"].([]any)
	for _, a := range as {
		m, _ := a.(map[string]any)
		tree, _ := m["tree"].(string)
		src, _ := m["source"].(string)
		by, _ := m["claimed_by"].(string)
		if tree != "" {
			out = append(out, qualify.Alternative{Tree: tree, Source: src, ClaimedBy: by})
		}
	}
	return out
}

// qualifyTrials is the number of trials one qualification needs at most (plus one per
// alternative).
const qualifyTrials = 1 + qualify.ReferenceRuns + qualify.MaxNearMisses + 1

func nextVersion(rec map[string]any, res qualify.Result, evidence string, now time.Time) map[string]any {
	next := map[string]any{}
	for k, v := range rec {
		next[k] = v
	}
	v := int(rec["version"].(float64))
	next["version"] = v + 1
	next["previous"] = fmt.Sprintf("%s@%d", rec["eval_id"], v)
	next["created_at"] = now.UTC().Format(time.RFC3339)
	next["state"] = res.State
	var reasons []any
	for _, r := range res.Reasons {
		reasons = append(reasons, r)
	}
	if reasons == nil {
		reasons = []any{}
	}
	next["reasons"] = reasons
	var checks []any
	for _, c := range res.Checks {
		m := map[string]any{"name": c.Name, "status": c.Status, "runs": c.Runs}
		if c.Independent != nil {
			m["independent"] = *c.Independent
		}
		if c.ClaimedBy != "" {
			m["claimed_by"] = c.ClaimedBy
		}
		var ev []any
		for _, d := range c.Evidence {
			ev = append(ev, d)
		}
		if evidence != "" {
			ev = append(ev, evidence)
		}
		if len(ev) > 0 {
			m["evidence"] = ev
		}
		checks = append(checks, m)
	}
	q := map[string]any{"policy": qualify.Policy, "checks": checks}
	if len(res.Requirements) > 0 {
		var reqs []any
		for _, r := range res.Requirements {
			m := map[string]any{"req_id": r.ReqID, "status": r.Status}
			if r.Reason != "" {
				m["reason"] = r.Reason
			}
			reqs = append(reqs, m)
		}
		q["requirements"] = reqs
	}
	next["qualification"] = q
	return next
}

// Limits of a stored run archive.
const (
	runFileMax  = 64 << 20
	runTotalMax = 256 << 20
)

// errRunTooLarge is returned when a run directory exceeds the archive limits; the
// directory is then kept on disk.
var errRunTooLarge = errors.New("the run results exceed the archive limits")

// keepRun stores everything that the runner wrote under work/jobs (trial results,
// verifier outputs, agent logs and the collected artifacts) as one archive object in the
// library. The caller removes the directory only when this succeeds: the results are
// evidence, and a later regrade or review needs them.
func keepRun(l *library.Library, work string) (string, error) {
	var files []string
	var total int64
	var tooLarge bool
	err := filepath.WalkDir(filepath.Join(work, "jobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // links and special files are not results
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > runFileMax {
			tooLarge = true
		}
		total += fi.Size()
		files = append(files, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	if tooLarge || total > runTotalMax {
		return "", errRunTooLarge
	}
	sort.Strings(files)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(work, f)
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return "", err
		}
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return l.PutObject(buf.Bytes())
}
