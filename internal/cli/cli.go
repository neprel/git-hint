// Package cli implements the git-hint command line. Git runs it as `git hint` when the
// git-hint executable is on PATH.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

// Env is the process environment of one invocation (replaceable in tests).
type Env struct {
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
	// IsTerminal reports whether stdin is interactive (for the export approval prompt).
	IsTerminal func() bool
	// Spawn starts a background git-hint process (nil: the real executable). Tests replace it.
	Spawn func(args ...string) error
	// EvalEnv gives the eval pipeline its environment (nil: the real user environment).
	EvalEnv func() pipeline.Env
}

type command struct {
	name    string
	effects string // what the command reads, writes and sends
	summary string
	run     func(e *Env, args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"init", "writes local state; no network", "Create the local project store, the key and the repository list.", cmdInit},
		{"status", "reads local state; no network", "Show repositories, snapshots, adapters, queued packages and the privacy profile.", cmdStatus},
		{"collect", "reads the work trees; writes local state; no network", "Scan the repositories and store a new snapshot when they changed.", cmdCollect},
		{"changes", "reads local state; no network; --declare-plan writes a local declaration", "Show the changes between the last two snapshots; declare a completed change as the application of a plan.", cmdChanges},
		{"observe", "reads local state; no network", "Show how agent activity is collected.", cmdObserve},
		{"export", "reads local state; writes files; no network", "Build, validate and write an export package (file mode).", cmdExport},
		{"recommend", "reads local state; SENDS the package to the configured server", "Send the approved package and fetch the recommendations.", cmdRecommend},
		{"plan", "reads local state and work trees; writes a local Markdown plan; no network", "Validate a recommendation and render the local plan.", cmdPlan},
		{"cache", "removes .hint/cache/ only; no network", "`cache clean`: delete rebuildable data. Mapping, keys and state stay.", cmdCache},
		{"doctor", "reads local state and tools; no network", "Check Git, the store, the key, schemas, adapters and optional indexers.", cmdDoctor},
		{"adapter", "writes .opencode/plugins/ (install); no network", "`adapter install opencode`: install the OpenCode adapter plugin.", cmdAdapter},
		{"mcp", "stdio MCP server; tools state their own effects (hint_recommend sends only if the local rule allows it)", "Start the MCP server for a coding agent (stdio).", cmdMCP},
		{"eval", "reads agent session records; writes local state; container runs only if allowed; sends nothing", "Build local evals from your agent sessions and measure agent efficiency (`eval on` once).", cmdEval},
		{"version", "no effects", "Print the version; --capabilities lists what this build supports.", cmdVersion},
	}
}

// Run executes one command and returns the exit code.
func Run(args []string, e *Env) int {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.IsTerminal == nil {
		e.IsTerminal = func() bool { return false }
	}
	if len(args) > 0 && args[0] == "--version" {
		return cmdVersion(e, args[1:])
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(e.Stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(e, args[1:])
		}
	}
	fmt.Fprintf(e.Stderr, "git hint: unknown command %q\n\n", args[0])
	usage(e.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: git hint <command> [options]\n\nHINT collects repository structure and agent activity, removes content and\nnames, and exchanges canonical documents with a HINT server.\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-10s %s\n  %-10s [%s]\n", c.name, c.summary, "", c.effects)
	}
	fmt.Fprintf(w, "\nRun `git hint <command> -h` for the options of a command.\n")
}

func flags(e *Env, name, effects string) *flag.FlagSet {
	fs := flag.NewFlagSet("git hint "+name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.Stderr, "usage: git hint %s [options]   [%s]\n", name, effects)
		fs.PrintDefaults()
	}
	return fs
}

func fail(e *Env, err error) int {
	fmt.Fprintf(e.Stderr, "git hint: %v\n", err)
	return 1
}

func open(e *Env) (*store.Store, error) { return store.Open(e.Dir) }

// ---------------------------------------------------------------------------

type repoFlags []string

func (r *repoFlags) String() string     { return strings.Join(*r, ",") }
func (r *repoFlags) Set(v string) error { *r = append(*r, v); return nil }

func cmdInit(e *Env, args []string) int {
	fs := flags(e, "init", "writes local state; no network")
	var repos repoFlags
	fs.Var(&repos, "repo", "add another repository: PATH or PATH=NAME (repeatable)")
	server := fs.String("server", "", "HINT server base URL (https; http only for a loopback host)")
	mcpUpload := fs.String("mcp-upload", "", "allow or deny: may the MCP tool hint_recommend send data (default deny)")
	mcpEvalRun := fs.String("mcp-eval-run", "", "allow or deny: may the MCP tool hint_eval_run run eval trials (default deny)")
	ctxLimits := fs.String("context-limits", "", "local context policy of hint_context: depth=1,items=20,bytes=16384,ms=1000 (a new version), off, or restore=N")
	if fs.Parse(args) != nil {
		return 2
	}
	var opt store.InitOptions
	for _, r := range repos {
		p, n, _ := strings.Cut(r, "=")
		opt.Repos = append(opt.Repos, store.Repo{Path: p, Name: n})
	}
	if *server != "" {
		if _, err := api.New(*server); err != nil {
			return fail(e, err)
		}
		opt.ServerURL = *server
	}
	opt.MCPUpload, opt.MCPEvalRun, opt.ContextLimits, opt.Now = *mcpUpload, *mcpEvalRun, *ctxLimits, e.Now()
	s, created, err := store.Init(e.Dir, opt)
	if err != nil {
		return fail(e, err)
	}
	kf, _ := s.KeyFile()
	if created {
		fmt.Fprintf(e.Stdout, "Created the HINT store in %s\n", s.Path())
	} else {
		fmt.Fprintf(e.Stdout, "Updated the HINT store in %s\n", s.Path())
	}
	fmt.Fprintf(e.Stdout, "Project namespace: %s\nKey file (outside the work tree): %s\n.hint/ is ignored by Git.\n", s.Cfg.ProjectNamespace, kf)
	for _, r := range s.Cfg.Repos {
		fmt.Fprintf(e.Stdout, "Repository %s: %s\n", r.Name, s.RepoRoot(r))
	}
	if s.Cfg.ServerURL != "" {
		fmt.Fprintf(e.Stdout, "Server: %s\n", s.Cfg.ServerURL)
	}
	upload := "deny"
	if s.Cfg.MCPUploadAllowed() {
		upload = "allow"
	}
	fmt.Fprintf(e.Stdout, "MCP upload rule: %s\n", upload)
	if p := s.Cfg.Context; p != nil {
		if l := p.Active(); l != nil {
			fmt.Fprintf(e.Stdout, "Context policy: version %d (depth %d, items %d, bytes %d, ms %d); %d versions kept\n", p.Current, l.Depth, l.MaxEntities, l.MaxBytes, l.MaxMillis, len(p.Versions))
		} else {
			fmt.Fprintf(e.Stdout, "Context policy: off (version %d); %d versions kept\n", p.Current, len(p.Versions))
		}
	}
	fmt.Fprintf(e.Stdout, "Next: git hint collect --full\n")
	return 0
}

func cmdStatus(e *Env, args []string) int {
	fs := flags(e, "status", "reads local state; no network")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	w := e.Stdout
	fmt.Fprintf(w, "Project namespace: %s\n", s.Cfg.ProjectNamespace)
	fmt.Fprintf(w, "Privacy profile: %s (time resolution %d s), exported record types: %s\n",
		s.Cfg.Policy.Profile, s.Cfg.Policy.TimeResolutionSeconds, strings.Join(s.Cfg.Policy.RecordTypes, ", "))
	fmt.Fprintf(w, "Server: %s\n", orNone(serverURL(s, "")))
	if err := s.CheckExcluded(); err != nil {
		fmt.Fprintf(w, "WARNING: %v\n", err)
	}
	for _, r := range s.Cfg.Repos {
		l, err := project.LoadLatest(s, s.RepoID(r))
		switch {
		case err != nil:
			fmt.Fprintf(w, "Repository %s: error %v\n", r.Name, err)
		case l == nil:
			fmt.Fprintf(w, "Repository %s: no snapshot (run git hint collect --full)\n", r.Name)
		default:
			fmt.Fprintf(w, "Repository %s: snapshot %s at %s, %d entities, %d relations, languages %s, work tree %s\n",
				r.Name, l.Record.SnapshotID[:12], l.Record.CapturedAt, len(l.Entities), len(l.Relations),
				orNone(strings.Join(l.Record.Languages, " ")), l.Record.WorktreeState)
		}
	}
	obs, bad, err := project.ReadSpool(s)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(w, "Activity spool: %d observations", len(obs))
	if bad > 0 {
		fmt.Fprintf(w, ", %d unreadable lines", bad)
	}
	fmt.Fprintln(w)
	adapterStatus(e, s)
	q, _ := project.Queued(s)
	fmt.Fprintf(w, "Queued packages: %d\n", len(q))
	fmt.Fprintf(w, "Evals: %s\n", state(s.Cfg.Evals))
	return 0
}

func adapterStatus(e *Env, s *store.Store) {
	plugin := filepath.Join(s.Root, ".opencode", "plugins", "hint-adapter.ts")
	if _, err := os.Stat(plugin); err == nil {
		fmt.Fprintf(e.Stdout, "OpenCode adapter: installed (%s)\n", plugin)
	} else {
		fmt.Fprintf(e.Stdout, "OpenCode adapter: not installed (git hint adapter install opencode)\n")
	}
	var st struct{ Sent, Dropped, HookErrors int }
	if b, err := os.ReadFile(s.Path("state", "activity", "adapter-stats.json")); err == nil && json.Unmarshal(b, &st) == nil {
		fmt.Fprintf(e.Stdout, "OpenCode adapter counters (last session): sent %d, dropped %d, hook errors %d\n", st.Sent, st.Dropped, st.HookErrors)
	}
}

func cmdCollect(e *Env, args []string) int {
	fs := flags(e, "collect", "reads the work trees; writes local state; no network")
	full := fs.Bool("full", false, "ignore the scan cache and build a full baseline")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	res, err := project.Collect(s, project.CollectOptions{Full: *full, Now: e.Now})
	if err != nil {
		return fail(e, err)
	}
	lock.Unlock()
	if !*asJSON {
		defer evalPass(e, s)
	}
	if *asJSON {
		type row struct {
			Repo       string `json:"repo"`
			SnapshotID string `json:"snapshot_id"`
			Changed    bool   `json:"changed"`
			Entities   int    `json:"entities"`
			Relations  int    `json:"relations"`
			Mutations  int    `json:"mutations"`
			Files      int    `json:"files"`
			Parsed     int    `json:"parsed"`
			Reused     int    `json:"reused"`
			Millis     int64  `json:"duration_ms"`
		}
		var rows []row
		for _, r := range res {
			rows = append(rows, row{r.Name, r.Snapshot.Record.SnapshotID, r.Changed, len(r.Snapshot.Entities),
				len(r.Snapshot.Relations), r.Mutations, r.Scan.Files, r.Scan.Parsed, r.Scan.Reused, r.Duration.Milliseconds()})
		}
		return writeJSON(e, rows)
	}
	for _, r := range res {
		state := "unchanged"
		if r.Changed {
			state = "new snapshot"
		}
		fmt.Fprintf(e.Stdout, "%s: %s %s; %d files (%d parsed, %d from cache), %d entities, %d relations, %d file changes; %s\n",
			r.Name, state, r.Snapshot.Record.SnapshotID[:12], r.Scan.Files, r.Scan.Parsed, r.Scan.Reused,
			len(r.Snapshot.Entities), len(r.Snapshot.Relations), r.Mutations, r.Duration.Round(time.Millisecond))
		if r.Scan.Unparsed > 0 {
			fmt.Fprintf(e.Stdout, "  %d Go files could not be parsed; they are kept at file level\n", r.Scan.Unparsed)
		}
	}
	return 0
}

func cmdChanges(e *Env, args []string) int {
	fs := flags(e, "changes", "reads local state; no network; --declare-plan writes a local declaration")
	format := fs.String("format", "text", "text (local names) or canonical (HINT change_set payload, no names)")
	declare := fs.String("declare-plan", "", "declare the latest change of the plan's repository as the application of PLAN (an association only; sent with the next export)")
	from := fs.String("from", "", "recommendation_set file for --declare-plan (default: the last one fetched by `git hint recommend`)")
	if fs.Parse(args) != nil {
		return 2
	}
	if *from != "" && *declare == "" {
		fmt.Fprintln(e.Stderr, "git hint changes: --from needs --declare-plan")
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	if *declare != "" {
		return declarePlan(e, s, *declare, *from)
	}
	for _, r := range s.Cfg.Repos {
		cur, err := project.LoadLatest(s, s.RepoID(r))
		if err != nil {
			return fail(e, err)
		}
		if cur == nil || len(cur.Record.ParentSnapshotIDs) == 0 {
			fmt.Fprintf(e.Stdout, "%s: no earlier snapshot\n", r.Name)
			continue
		}
		prev, err := project.LoadSnapshot(s, cur.Record.RepoID, cur.Record.ParentSnapshotIDs[0])
		if err != nil {
			return fail(e, err)
		}
		cs, local := snapshotDiff(s, prev, cur)
		if *format == "canonical" {
			if rc := writeJSON(e, cs); rc != 0 {
				return rc
			}
			continue
		}
		fmt.Fprintf(e.Stdout, "%s: %d changes from %s to %s\n", r.Name, len(local), prev.Record.SnapshotID[:12], cur.Record.SnapshotID[:12])
		for _, c := range local {
			conf := ""
			if c.MatchConfidence != nil {
				conf = fmt.Sprintf(" confidence %.2f", *c.MatchConfidence)
			}
			fmt.Fprintf(e.Stdout, "  %-9s %-8s %s -> %s (%s%s)\n", c.Operation, c.Level, orDash(c.BeforeNames), orDash(c.AfterNames), c.MatchMethod, conf)
		}
	}
	return 0
}

func cmdObserve(e *Env, args []string) int {
	fs := flags(e, "observe", "reads local state; no network")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintln(e.Stdout, "OpenCode: the adapter plugin starts `git-hint adapter opencode` itself; no receiver is needed.")
	fmt.Fprintln(e.Stdout, "File changes: `git hint collect` records the files that changed since the last snapshot.")
	fmt.Fprintln(e.Stdout, "Claude Code and Codex adapters are not available in this version.")
	adapterStatus(e, s)
	return 0
}

func cmdExport(e *Env, args []string) int {
	fs := flags(e, "export", "reads local state; writes files; no network")
	dry := fs.Bool("dry-run", false, "build and validate the package; write nothing")
	report := fs.Bool("privacy-report", false, "print the privacy report")
	out := fs.String("out", "", "output directory (default .hint/state/exports/<time>/)")
	all := fs.Bool("all", false, "include observations that were already exported")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	now := e.Now()
	p, err := project.BuildPackage(s, project.ExportOptions{All: *all, Now: now})
	if err != nil {
		return fail(e, err)
	}
	summary(e, p)
	if *report {
		privacyReport(e, p)
	}
	if *dry {
		fmt.Fprintln(e.Stdout, "Dry run: nothing was written. The package is valid.")
		return 0
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	dir := *out
	if dir == "" {
		dir = s.Path("state", "exports", now.UTC().Format("20060102T150405Z"))
		for _, d := range p.Docs {
			if err := s.CheckPath(filepath.Join(dir, d.File)); err != nil {
				return fail(e, err)
			}
		}
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.Dir, dir)
	}
	files, err := project.WritePackage(p, dir)
	if err != nil {
		return fail(e, err)
	}
	if err := project.MarkExported(s, p, "file", "", now); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Wrote %d documents to %s\n", len(files), dir)
	return 0
}

func summary(e *Env, p *project.Package) {
	w := e.Stdout
	fmt.Fprintf(w, "Package: %d documents, profile %s, time resolution %d s, policy %s\n", len(p.Docs), p.Report.Profile,
		p.Report.TimeResolutionSeconds, p.Report.PolicyID)
	for _, d := range p.Docs {
		fmt.Fprintf(w, "  %-22s %s (%d bytes)\n", d.Type, d.File, len(d.Raw))
	}
	var rts []string
	for k := range p.Report.Records {
		rts = append(rts, k)
	}
	sort.Strings(rts)
	for _, k := range rts {
		fmt.Fprintf(w, "  records %-12s %d\n", k, p.Report.Records[k])
	}
}

func privacyReport(e *Env, p *project.Package) {
	w := e.Stdout
	r := p.Report
	fmt.Fprintln(w, "Privacy report")
	fmt.Fprintln(w, "  Content: no field for source code, names, paths, prompts or outputs; every string is an ID, digest, time, version or enumeration (checked).")
	fmt.Fprintf(w, "  Identifiers: HMAC pseudonyms under the project key; linkable across this project's exports, not across projects.\n")
	fmt.Fprintf(w, "  Time span: %s to %s; resolution %s\n", orNone(r.TimeMin), orNone(r.TimeMax), resolution(r.TimeResolutionSeconds))
	fmt.Fprintf(w, "  Exact entity features: %s\n", orNone(strings.Join(r.Features, ", ")))
	var kinds []string
	for k, n := range r.EntityKinds {
		kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(kinds)
	fmt.Fprintf(w, "  Graph granularity: %s\n", orNone(strings.Join(kinds, ", ")))
	var ops []string
	for k, n := range r.Ops {
		ops = append(ops, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(ops)
	fmt.Fprintf(w, "  Observations: %s\n", orNone(strings.Join(ops, ", ")))
	var rts []string
	for k := range r.Fields {
		rts = append(rts, k)
	}
	sort.Strings(rts)
	for _, k := range rts {
		fmt.Fprintf(w, "  Allowed fields %s: %s\n", k, strings.Join(r.Fields[k], ", "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  Note: %s\n", n)
	}
}

func resolution(s int) string {
	if s == 0 {
		return "as recorded"
	}
	return fmt.Sprintf("%d s", s)
}

func serverURL(s *store.Store, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("HINT_SERVER_URL"); v != "" {
		return v
	}
	return s.Cfg.ServerURL
}

func cmdRecommend(e *Env, args []string) int {
	fs := flags(e, "recommend", "reads local state; SENDS the package to the configured server")
	yes := fs.Bool("yes", false, "approve sending without a prompt")
	retry := fs.Bool("retry", false, "send queued packages again")
	server := fs.String("server", "", "server base URL (default: configuration or HINT_SERVER_URL)")
	all := fs.Bool("all", false, "include observations that were already exported")
	horizon := fs.String("forecast-horizon", "", "request a conditional forecast over N future tasks (1-100000)")
	threshold := fs.String("forecast-threshold", "", "configured decision threshold in (0, 1] (needs --forecast-horizon; default 0.9)")
	cost := fs.String("assumed-cost", "", "assumed one-time cost LO:HI in tokens; shown as a scenario, never a recommendation (needs --forecast-horizon)")
	if fs.Parse(args) != nil {
		return 2
	}
	policy, err := forecastPolicy(fs, *horizon, *threshold, *cost)
	if err != nil {
		fmt.Fprintln(e.Stderr, "git hint recommend:", err)
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	c, err := api.New(serverURL(s, *server))
	if err != nil {
		return fail(e, err)
	}
	now := e.Now()
	ctx := context.Background()
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	var packages []*project.Package
	var queueIDs []string
	if *retry {
		ids, err := project.Queued(s)
		if err != nil {
			return fail(e, err)
		}
		if len(ids) == 0 {
			fmt.Fprintln(e.Stdout, "No queued packages.")
			return 0
		}
		for _, id := range ids {
			p, err := project.LoadQueued(s, id)
			if err != nil {
				return fail(e, err)
			}
			packages, queueIDs = append(packages, p), append(queueIDs, id)
		}
	} else {
		p, err := project.BuildPackage(s, project.ExportOptions{All: *all, Now: now})
		if err != nil {
			return fail(e, err)
		}
		packages, queueIDs = []*project.Package{p}, []string{""}
		summary(e, p)
	}
	n := 0
	for _, p := range packages {
		n += len(p.Docs)
	}
	if !*yes {
		if !e.IsTerminal() {
			return fail(e, fmt.Errorf("sending %d documents to %s needs approval; inspect them with `git hint export --dry-run --privacy-report`, then run again with --yes", n, c.Origin()))
		}
		fmt.Fprintf(e.Stdout, "Send %d documents to %s? [y/N] ", n, c.Origin())
		var ans string
		fmt.Fscanln(e.Stdin, &ans)
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(e.Stdout, "Not sent.")
			return 1
		}
	}
	for i, p := range packages {
		if err := project.Send(ctx, s, c, p, now); err != nil {
			var q *project.ErrQueued
			if errors.As(err, &q) && queueIDs[i] != "" {
				_ = project.DropQueued(s, filepath.Base(q.Dir)) // the old entry is still queued
			}
			return fail(e, err)
		}
		if queueIDs[i] != "" {
			if err := project.DropQueued(s, queueIDs[i]); err != nil {
				return fail(e, err)
			}
		}
	}
	fmt.Fprintf(e.Stdout, "Sent %d documents to %s.\n", n, c.Origin())
	raw, err := c.RecommendationsWith(ctx, s.Cfg.ProjectNamespace, policy)
	if err != nil {
		return fail(e, fmt.Errorf("fetch recommendations: %w", err))
	}
	path, err := project.SaveRecommendation(s, raw)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Saved the recommendation document: %s\nNext: git hint plan\n", path)
	return 0
}

func cmdPlan(e *Env, args []string) int {
	fs := flags(e, "plan", "reads local state and work trees; writes a local Markdown plan; no network")
	from := fs.String("from", "", "recommendation_set file (default: the last one fetched by `git hint recommend`)")
	out := fs.String("out", "", "Markdown output file (default .hint/state/recommendations/plan-<time>.md)")
	asJSON := fs.Bool("json", false, "print the validation result as JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	src := *from
	if src == "" {
		if src, err = project.LatestRecommendation(s); err != nil {
			return fail(e, err)
		}
	} else if !filepath.IsAbs(src) {
		src = filepath.Join(e.Dir, src)
	}
	raw, err := readLimited(src, project.MaxDocumentBytes)
	if err != nil {
		return fail(e, err)
	}
	now := e.Now()
	cur, err := project.CurrentState(s, now)
	if err != nil {
		return fail(e, err)
	}
	im, err := project.Import(raw, cur)
	if err != nil {
		return fail(e, err)
	}
	md := project.Render(im, cur, filepath.Base(src))
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	dst := *out
	if dst == "" {
		dst = s.Path("state", "recommendations", "plan-"+now.UTC().Format("20060102T150405Z")+".md")
		if err := s.WriteState([]byte(md), "state", "recommendations", filepath.Base(dst)); err != nil {
			return fail(e, err)
		}
	} else {
		if !filepath.IsAbs(dst) {
			dst = filepath.Join(e.Dir, dst)
		}
		if err := os.WriteFile(dst, []byte(md), 0o600); err != nil {
			return fail(e, err)
		}
	}
	if *asJSON {
		writeJSON(e, map[string]any{"result": im.Result, "markdown": dst})
	} else {
		fmt.Fprintf(e.Stdout, "Document: %s\n", im.Result.Document)
		for _, p := range im.Result.Plans {
			fmt.Fprintf(e.Stdout, "  plan %s: %s %s\n", p.PlanID[:12], p.Status, strings.Join(p.Codes, ","))
		}
		fmt.Fprintf(e.Stdout, "Markdown plan: %s\n", dst)
	}
	if im.Result.Document != "accepted" {
		return 1
	}
	return 0
}

func readLimited(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	return b, nil // Import reports too_large for len > max
}

func cmdCache(e *Env, args []string) int {
	if len(args) != 1 || args[0] != "clean" {
		fmt.Fprintln(e.Stderr, "usage: git hint cache clean   [removes .hint/cache/ only; no network]")
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	if err := s.CleanCache(); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Removed %s. Mapping, keys and state are unchanged.\n", s.Path("cache"))
	return 0
}

func cmdMCP(e *Env, args []string) int {
	fs := flags(e, "mcp", "stdio MCP server; stdout carries MCP messages only")
	dir := fs.String("project", e.Dir, "project directory")
	tools := fs.String("tools", "", "comma-separated tool names to serve (default: all)")
	if fs.Parse(args) != nil {
		return 2
	}
	var only []string
	for _, t := range strings.Split(*tools, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !slices.Contains(mcpserver.ToolNames, t) {
			return fail(e, fmt.Errorf("unknown tool %q", t))
		}
		only = append(only, t)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := mcpserver.Run(ctx, mcpserver.Options{Dir: *dir, Now: e.Now, Stderr: e.Stderr, Tools: only},
		io.NopCloser(e.Stdin), nopWriteCloser{e.Stdout})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(e.Stderr, "git hint mcp: session ended:", classifyForStderr(err))
		return 1
	}
	return 0
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// classifyForStderr keeps stderr free of names: only the error kind is printed.
func classifyForStderr(err error) string {
	if strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
		return "client disconnected"
	}
	return "transport error"
}

func cmdVersion(e *Env, args []string) int {
	fs := flags(e, "version", "no effects")
	asJSON := fs.Bool("json", false, "print JSON")
	caps := fs.Bool("capabilities", false, "also list languages, MCP, adapters and optional tools of this build")
	if fs.Parse(args) != nil {
		return 2
	}
	b := version.Info()
	if !*caps {
		if *asJSON {
			return writeJSON(e, b)
		}
		fmt.Fprintf(e.Stdout, "git-hint %s (HINT data standard %s)\ncommit %s%s, %s, %s %s/%s (metadata: %s%s)\n", b.Version, b.Standard,
			b.Commit, modifiedMark(b), b.Date, b.GoVersion, b.OS, b.Arch, b.Source, moduleNote(b))
		return 0
	}
	c := capabilities(b)
	if *asJSON {
		return writeJSON(e, c)
	}
	w := e.Stdout
	fmt.Fprintf(w, "git-hint %s (HINT data standard %s; reads %s)\n", b.Version, b.Standard, strings.Join(b.ReadsMinors, ", "))
	fmt.Fprintf(w, "build: commit %s%s, %s, %s, %s/%s, cgo %v (metadata: %s%s)\n", b.Commit, modifiedMark(b), b.Date, b.GoVersion, b.OS, b.Arch, b.CGO,
		b.Source, moduleNote(b))
	fmt.Fprintf(w, "platform %s/%s: %s\n", b.OS, b.Arch, c.Platform)
	fmt.Fprintf(w, "structure: %s\n", c.Structure.Summary)
	fmt.Fprintf(w, "file-level languages: %s\n", strings.Join(c.Structure.FileLevelLanguages, ", "))
	fmt.Fprintf(w, "embedded grammars: none\n")
	fmt.Fprintf(w, "MCP: stdio; protocol versions %s; %d tools\n", strings.Join(c.MCP.ProtocolVersions, ", "), len(c.MCP.Tools))
	for _, a := range c.Adapters {
		fmt.Fprintf(w, "adapter %s: %s\n", a.Agent, a.Status)
	}
	for _, o := range c.OptionalTools {
		fmt.Fprintf(w, "optional %s: %s\n", o.Name, o.State)
	}
	fmt.Fprintf(w, "server: not needed for init, collect, export, plan or mcp; recommend needs a configured HINT server\n")
	return 0
}

func modifiedMark(b version.Build) string {
	if b.Modified {
		return "+modified"
	}
	return ""
}

func moduleNote(b version.Build) string {
	if b.ModuleVersion != "" {
		return "; module " + b.ModuleVersion
	}
	return ""
}

// Capabilities is the `version --capabilities` report.
type Capabilities struct {
	Build     version.Build `json:"build"`
	Platform  string        `json:"platform"`
	Structure struct {
		Summary            string   `json:"summary"`
		Declarations       []string `json:"declaration_languages"`
		Parser             string   `json:"parser"`
		FileLevelLanguages []string `json:"file_level_languages"`
		EmbeddedGrammars   []string `json:"embedded_grammars"`
	} `json:"structure"`
	MCP struct {
		Transport        string   `json:"transport"`
		ProtocolVersions []string `json:"protocol_versions"`
		Tools            []string `json:"tools"`
	} `json:"mcp"`
	Adapters      []AdapterCap  `json:"adapters"`
	OptionalTools []OptionalCap `json:"optional_tools"`
}

type AdapterCap struct {
	Agent  string `json:"agent"`
	Status string `json:"status"`
}

type OptionalCap struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func capabilities(b version.Build) Capabilities {
	var c Capabilities
	c.Build = b
	c.Platform = platformSupport(b.OS, b.Arch)
	c.Structure.Summary = "Go: declarations and imports (go/parser, syntax level); other languages: file level"
	c.Structure.Declarations = []string{"go"}
	c.Structure.Parser = "go/parser (Go standard library)"
	c.Structure.FileLevelLanguages = scan.FileLevelLanguages()
	c.Structure.EmbeddedGrammars = []string{}
	c.MCP.Transport = "stdio"
	c.MCP.ProtocolVersions = mcpserver.ProtocolVersions()
	c.MCP.Tools = append([]string{}, mcpserver.ToolNames...)
	for _, a := range adapterInfo {
		c.Adapters = append(c.Adapters, AdapterCap{a.agent, a.status})
	}
	for _, tool := range optionalTools {
		st := "not found; not needed"
		if _, err := exec.LookPath(tool); err == nil {
			st = "found; not used by this version"
		}
		c.OptionalTools = append(c.OptionalTools, OptionalCap{tool, st})
	}
	return c
}

// platformSupport states the support level of a platform. Only tested platforms are
// "supported"; Windows is deferred.
func platformSupport(goos, arch string) string {
	switch {
	case (goos == "darwin" || goos == "linux") && (arch == "arm64" || arch == "amd64"):
		return "supported"
	case goos == "windows":
		return "deferred (not supported, not tested)"
	default:
		return "not tested"
	}
}

func writeJSON(e *Env, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintln(e.Stdout, string(b))
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orDash(l []string) string {
	if len(l) == 0 {
		return "—"
	}
	return strings.Join(l, ", ")
}
