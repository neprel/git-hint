package cli

import (
	"bytes"
	"encoding/json"
	"errors"

	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

func snapshotDiff(s *store.Store, a, b *snapshot.Local) (snapshot.ChangeSet, []snapshot.LocalChange) {
	return snapshot.Diff(s.Key(), a, b)
}

func cmdAdapter(e *Env, args []string) int {
	switch {
	case len(args) >= 2 && args[0] == "install" && args[1] == "opencode":
		return adapterInstall(e, args[2:])
	case len(args) >= 1 && args[0] == "opencode":
		return adapterOpenCode(e, args[1:])
	}
	fmt.Fprintln(e.Stderr, "usage: git hint adapter install opencode [--force]   [writes .opencode/plugins/hint-adapter.ts]")
	fmt.Fprintln(e.Stderr, "       git hint adapter opencode --project <dir>      [internal: started by the plugin]")
	return 2
}

func adapterInstall(e *Env, args []string) int {
	fs := flags(e, "adapter install opencode", "writes .opencode/plugins/hint-adapter.ts; no network")
	force := fs.Bool("force", false, "replace a different existing file")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	dir := filepath.Join(s.Root, ".opencode", "plugins")
	p := filepath.Join(dir, "hint-adapter.ts")
	if old, err := os.ReadFile(p); err == nil {
		if bytes.Equal(old, githint.OpenCodePlugin) {
			fmt.Fprintf(e.Stdout, "The OpenCode adapter is already installed: %s\n", p)
			return 0
		}
		if !*force {
			return fail(e, fmt.Errorf("%s exists and differs; use --force to replace it", p))
		}
	}
	others, _ := filepath.Glob(filepath.Join(dir, "*.ts"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(e, err)
	}
	if err := os.WriteFile(p, githint.OpenCodePlugin, 0o644); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Installed the OpenCode adapter: %s\n", p)
	fmt.Fprintln(e.Stdout, "OpenCode starts `git-hint adapter opencode` from PATH (or HINT_BIN). Tested with OpenCode 2.0.26.")
	for _, o := range others {
		if filepath.Base(o) != "hint-adapter.ts" {
			fmt.Fprintf(e.Stdout, "Note: OpenCode also loads %s from this directory.\n", o)
		}
	}
	return 0
}

// adapterOpenCode is the long-lived child of the OpenCode plugin. It reads reduced plugin
// events from stdin and appends one observation per finished tool call to the spool.
//
// Every line is written only after the Git exclusion of `.hint/` is checked again. When a
// check fails (a rule was removed, or a file below `.hint/` was added to the index), the
// child stops writing for the rest of the session: it tells the plugin on stdout
// ({"hint":"stopped"}), keeps reading stdin so that OpenCode is never blocked, and counts
// the discarded records in memory. It reports the counts on stderr and exits with 4.
//
// Exit codes: 0 done, 1 setup error, 3 spool write failure, 4 stopped (not excluded).
func adapterOpenCode(e *Env, args []string) int {
	fs := flags(e, "adapter opencode", "reads stdin; writes the activity spool; no network")
	dir := fs.String("project", e.Dir, "project directory of the OpenCode session")
	agentVersion := fs.String("agent-version", os.Getenv("HINT_AGENT_VERSION"), "OpenCode version, if known")
	modelCallsFlag := fs.Bool("model-calls", false, "the plugin forwards model-call events (draft v0.2; OpenCode "+activity.UsageAgentVersion+" only)")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := store.Open(*dir)
	if err != nil {
		return fail(e, err)
	}
	if err := s.CheckExcluded(); err != nil {
		return fail(e, err)
	}
	// HINT_TASK_ID: the orchestrator declares the one task of this agent run; checked before
	// any write; the raw value is never written or logged
	rawTask, declared := os.LookupEnv("HINT_TASK_ID")
	declared = declared && rawTask != ""
	if declared && !project.ValidTaskID(rawTask) {
		return fail(e, errors.New("HINT_TASK_ID is not a usable task ID (1-200 bytes of printable text); nothing was written"))
	}
	// HINT_TASK_FAMILY: the declared family of that task (a declaration, not a classification);
	// HINT_MODEL: the model the user declared (provenance; the effective model is not reported
	// by the plugin, so it stays unknown); HINT_CONFIG_REFS: kind:sha256-digest pairs of the
	// observed effective configuration files. All checked before any write; raw values stay local.
	rawFamily := os.Getenv("HINT_TASK_FAMILY")
	if rawFamily != "" && !declared {
		return fail(e, errors.New("HINT_TASK_FAMILY needs HINT_TASK_ID; nothing was written"))
	}
	if rawFamily != "" && !project.ValidTaskID(rawFamily) {
		return fail(e, errors.New("HINT_TASK_FAMILY is not a usable family label (1-200 bytes of printable text); nothing was written"))
	}
	root := os.Getenv("HINT_OPENCODE_SESSION")
	if root != "" && !project.ValidTaskID(root) {
		return fail(e, errors.New("HINT_OPENCODE_SESSION is not a usable session ID (1-200 bytes of printable text); nothing was written"))
	}
	// the configuration is attached only when the orchestrator declares something explicit;
	// otherwise the source identity and the v0.1 export stay as before
	var cfg *activity.Configuration
	if rawFamily != "" || os.Getenv("HINT_MODEL") != "" || os.Getenv("HINT_CONFIG_REFS") != "" {
		if cfg, err = runConfiguration(s, *agentVersion, os.Getenv("HINT_MODEL"), os.Getenv("HINT_CONFIG_REFS")); err != nil {
			return fail(e, fmt.Errorf("%w; nothing was written", err))
		}
	}
	// HINT_WORK_UNIT: the run is a member task of a declared work unit (git hint eval work
	// start); recorded after every other check and before any activity write; a closed unit records late activity
	if w := os.Getenv("HINT_WORK_UNIT"); w != "" {
		if !declared {
			return fail(e, errWorkWithoutTask)
		}
		lock, err := s.LockState(true)
		if err != nil {
			return fail(e, err)
		}
		err = project.WorkJoin(s, w, project.TaskID(s.Key(), rawTask), e.Now())
		lock.Unlock()
		if err != nil {
			return fail(e, fmt.Errorf("HINT_WORK_UNIT: %w; nothing was written", err))
		}
	}
	// model calls only for the OpenCode version whose events were checked (07B-E §30)
	modelCalls := *modelCallsFlag && *agentVersion == activity.UsageAgentVersion
	src := activity.OpenCodeSource(s.Key(), activity.SourceOptions{AgentVersion: *agentVersion, TaskDeclared: declared,
		FamilyDeclared: rawFamily != "", Configuration: cfg, ModelCalls: modelCalls})
	if err := project.RegisterSource(s, src); err != nil {
		return fail(e, err)
	}
	spool, err := project.OpenSpool(s)
	if err != nil {
		return fail(e, err)
	}
	defer spool.Close()
	var (
		written, discarded, statsLines int
		stopped                        error
	)
	stop := func(err error) {
		if stopped == nil {
			stopped = err
			fmt.Fprintln(e.Stdout, `{"hint":"stopped","reason":"not_excluded"}`)
			if f, ok := e.Stdout.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
	}
	n := &activity.Normalizer{Key: s.Key(), SourceID: src.SourceID, Cwd: cleanAbs(*dir), Resolve: resolver(s), Now: e.Now,
		ModelCalls: modelCalls}
	// the child's acknowledgment at the plugin's closure line: every preceding record was written
	n.Acknowledged = func() bool { return stopped == nil && discarded == 0 }
	// the declared root OpenCode session of this run (the orchestrator starts OpenCode with
	// --session <id>); without it no session is owned and model usage stays unknown (C7BE-18)
	if root != "" {
		n.RootSessions = []string{root}
	}
	if declared {
		n.TaskID = project.TaskID(s.Key(), rawTask)
		if rawFamily != "" {
			n.TaskFamilyID = activity.FamilyRef(s.Key(), rawFamily)
		}
	}
	n.OnStats = func(line []byte) {
		statsLines++
		if stopped != nil {
			return // nothing reaches .hint/ after a failed check; the plugin keeps its counters in memory
		}
		var in struct {
			Stats map[string]any `json:"stats"`
		}
		if json.Unmarshal(line, &in) != nil || in.Stats == nil {
			return
		}
		in.Stats["child"] = map[string]int{"written": written, "discarded": discarded}
		if err := s.WriteJSON(in.Stats, "state", "activity", "adapter-stats.json"); err != nil && errors.Is(err, store.ErrNotExcluded) {
			stop(err)
		}
	}
	// On SIGTERM or SIGINT (OpenCode exit), end the input so that open calls are written
	// with the end time missing instead of being lost.
	in := e.Stdin
	if f, ok := e.Stdin.(*os.File); ok {
		pr, pw := io.Pipe()
		go func() { _, err := io.Copy(pw, f); pw.CloseWithError(err) }()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		go func() { <-sig; pw.Close() }()
		in = pr
	}
	skipped, err := n.Stream(in, func(o activity.Observation) error {
		if stopped != nil {
			discarded++
			return nil
		}
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		if err := spool.Append(b); err != nil {
			if errors.Is(err, store.ErrNotExcluded) {
				stop(err)
				discarded++
				return nil
			}
			return err
		}
		written++
		return nil
	})
	fmt.Fprintf(e.Stderr, "adapter opencode: written=%d skipped=%d discarded=%d\n", written, skipped, discarded)
	if stopped != nil {
		fmt.Fprintf(e.Stderr, "adapter opencode: stopped writing: %v\n", stopped)
		return 4
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "adapter opencode: %v\n", err)
		if strings.HasPrefix(err.Error(), "emit:") {
			return 3
		}
		return 1
	}
	return 0
}

func cleanAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return filepath.ToSlash(filepath.Clean(p))
	}
	return filepath.ToSlash(a)
}

// resolver maps an absolute path to a file ID when the path lies in a configured
// repository. Both the configured and the symlink-resolved roots are checked.
func resolver(s *store.Store) activity.Resolver {
	type root struct{ dir, id string }
	var roots []root
	for _, r := range s.Cfg.Repos {
		id := s.RepoID(r)
		d := cleanAbs(s.RepoRoot(r))
		roots = append(roots, root{d, id})
		if ev, err := filepath.EvalSymlinks(d); err == nil && filepath.ToSlash(ev) != d {
			roots = append(roots, root{filepath.ToSlash(ev), id})
		}
	}
	return func(abs string) (string, bool) {
		for _, r := range roots {
			if rel, ok := strings.CutPrefix(abs, r.dir+"/"); ok && rel != "" && !strings.HasPrefix(rel, ".hint/") {
				return hint.FileID(s.Key(), r.id, rel), true
			}
		}
		return "", false
	}
}

// the kinds and digest form of eval trial observed_config (evals v1), so one file gives one ref
var configRefForm = regexp.MustCompile(`^(setting_override|dependency_lock):sha256:[0-9a-f]{64}$`)

// runConfiguration builds the canonical effective configuration of a direct OpenCode run.
func runConfiguration(s *store.Store, agentVersion, rawModel, configRefs string) (*activity.Configuration, error) {
	c := &activity.Configuration{}
	c.Agent.Name = "opencode"
	c.Agent.Version = activity.VersionState{State: "unknown"}
	if agentVersion != "" {
		c.Agent.Version = activity.VersionState{State: "known", Value: agentVersion}
	}
	c.Model = activity.ModelState{State: "unknown"} // no authoritative launch and no agent report: unknown
	if rawModel != "" {
		if !project.ValidTaskID(rawModel) {
			return nil, errors.New("HINT_MODEL is not a usable model name (1-200 bytes of printable text)")
		}
		c.Declared = &activity.Declared{Model: &activity.RefState{State: "known", Ref: activity.ModelRef(s.Key(), rawModel)}}
	}
	c.Harness.Kind = "direct"
	c.Harness.Version = activity.VersionState{State: "not_applicable"} // a direct session has no separate runner
	state, id, version, digest := s.Cfg.ContextPolicyIdentity()
	c.ContextPolicy = activity.ConfigPolicy{State: state}
	if state == "enabled" {
		c.ContextPolicy.Ref, c.ContextPolicy.Version = hint.EvalRef(s.Key(), "context-policy", id, strconv.Itoa(version), digest), version
	}
	// a direct run's limits are not known to HINT (the agent session has no HINT budget); they
	// stay unknown, never inferred as unbounded
	c.Budget = activity.Budget{MaxRequests: activity.Limit{State: "unknown"}, MaxSeconds: activity.Limit{State: "unknown"}}
	c.ConfigRefs = activity.ConfigRefs{State: "unknown"}
	if configRefs != "" {
		refs := []string{}
		if configRefs != "none" { // "none": a known empty set
			for _, p := range strings.Split(configRefs, ",") {
				if !configRefForm.MatchString(p) {
					return nil, errors.New("HINT_CONFIG_REFS must be setting_override or dependency_lock :sha256:<hex> pairs separated by commas, or none")
				}
				kind, digest, _ := strings.Cut(p, ":")
				refs = append(refs, activity.ConfigRef(s.Key(), kind, digest))
			}
			slices.Sort(refs)
			refs = slices.Compact(refs)
		}
		c.ConfigRefs = activity.ConfigRefs{State: "known", Refs: refs}
	}
	return c, nil
}
