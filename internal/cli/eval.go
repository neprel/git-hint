package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/bundle"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/store"
)

const evalEffects = "reads agent session records; writes local state only; runs code in containers only when the eval policy allows it; sends nothing"

func evalUsage(e *Env) {
	fmt.Fprint(e.Stderr, `usage: git hint eval [command]   [`+evalEffects+`]

Turns your own agent sessions into local evals: repeatable tasks with hidden checks that
measure how well agents work on this project, before and after a refactoring or a change
of agent setup. After 'eval on', collect builds and qualifies evals automatically.

  git hint eval                      list the evals and what the pipeline did
  git hint eval on [--agent LIST] [--run docker]
                                     enable collection (once); --run allows container runs
  git hint eval off                  stop collecting (stored evals stay)
  git hint eval show ID [--text]     one eval: state, checks, reasons (--text: local texts)
  git hint eval forget ID...|--all   delete evals (pinned ones stay) and list leftovers
  git hint eval pin ID [--off]       keep an eval version beyond the retention period
  git hint eval map ID [--apply FILE]
                                     print the requirement mapping file of an eval; --apply
                                     checks your edited file and stores a new version
  git hint eval run [ID...] [--agent opencode|oracle|nop] [--attempts N] [--arm ARM]
                                     freeze a suite of validated evals and run bounded trials
  git hint eval run ID... --arm context_only --with FILE...
  git hint eval run ID... --arm refactor --with WORKTREE
                                     the same tasks with added context files, or on your
                                     refactored code; each variant is qualified first
  git hint eval compare [SUITE]      raw results per arm and agent (every attempt counts)
  git hint eval report [-o FILE] [--send] [--yes]
                                     minimized results of new trials (no task content);
                                     --send uploads them when the policy allows result export
  git hint eval pack ID... -o FILE   write evals to a private bundle for your other machines
  git hint eval unpack FILE          read a bundle (every object is checked; never sent anywhere)
`)
}

func cmdEval(e *Env, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return evalList(e, args)
	}
	switch args[0] {
	case "on":
		return evalOn(e, args[1:])
	case "off":
		return evalOff(e, args[1:])
	case "show":
		return evalShow(e, args[1:])
	case "forget":
		return evalForget(e, args[1:])
	case "pin":
		return evalPin(e, args[1:])
	case "map":
		return evalMap(e, args[1:])
	case "run":
		return evalRun(e, args[1:])
	case "compare":
		return evalCompare(e, args[1:])
	case "report":
		return evalReport(e, args[1:])
	case "pack":
		return evalPack(e, args[1:])
	case "unpack":
		return evalUnpack(e, args[1:])
	case "__qualify":
		return evalWorker(e, args[1:])
	case "help", "-h", "--help":
		evalUsage(e)
		return 0
	}
	evalUsage(e)
	return 2
}

func evalOn(e *Env, args []string) int {
	fs := flags(e, "eval on", evalEffects)
	agents := fs.String("agent", "opencode", "agents whose sessions are collected: opencode, claude-code, codex (comma separated)")
	run := fs.String("run", "", "allow container runs: docker (needs Harbor 0.23 and Docker)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	var list []string
	for _, a := range strings.Split(*agents, ",") {
		if a = strings.TrimSpace(a); a != "" {
			list = append(list, a)
		}
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
	p := evals.Enabled(list)
	if old := s.Cfg.Evals; old != nil { // keep the user's own settings
		if old.Model != nil {
			p.Model = old.Model
		}
		if old.Run != nil {
			p.Run = old.Run
		}
		if old.RetentionDays > 0 {
			p.RetentionDays = old.RetentionDays
		}
		if old.QuotaMB > 0 {
			p.QuotaMB = old.QuotaMB
		}
		p.ImportRoots, p.Export = old.ImportRoots, orDefault(old.Export, "off")
	}
	switch *run {
	case "":
	case "docker":
		p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	default:
		fmt.Fprintf(e.Stderr, "git hint: --run accepts docker only\n")
		return 2
	}
	if err := evals.ValidateValue("policy", p); err != nil {
		return fail(e, fmt.Errorf("the eval policy is not valid: %w", err))
	}
	s.Cfg.Evals = &p
	if err := s.SaveConfig(); err != nil {
		return fail(e, err)
	}
	l := p.Limits()
	fmt.Fprintf(e.Stdout, "Eval collection is on for %s.\n", strings.Join(p.Agents, ", "))
	fmt.Fprintf(e.Stdout, "Session texts, file states and checks stay in .hint/state/evals/ (owner only, ignored by Git); nothing is sent.\n")
	fmt.Fprintf(e.Stdout, "Limits: %d days retention, %d MiB, %d episodes per pass.\n", l.RetentionDays, l.QuotaBytes>>20, l.BatchEpisodes)
	fmt.Fprintf(e.Stdout, "Container runs: %s. Local model: %s. Result reports: %s.\n", allowed(p.Check(evals.OpExecute, "")), allowed(p.Check(evals.OpModel, "")), allowed(p.Check(evals.OpExport, "")))
	fmt.Fprintf(e.Stdout, "git hint collect now imports new sessions and builds evals.\n")
	return 0
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func allowed(err error) string {
	if err == nil {
		return "allowed"
	}
	return "off"
}

func evalOff(e *Env, args []string) int {
	if len(args) != 0 {
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
	if s.Cfg.Evals == nil {
		fmt.Fprintln(e.Stdout, "Eval collection is already off.")
		return 0
	}
	s.Cfg.Evals.Capture = "off"
	if err := s.SaveConfig(); err != nil {
		return fail(e, err)
	}
	fmt.Fprintln(e.Stdout, "Eval collection is off. Stored evals stay; git hint eval forget --all deletes them.")
	return 0
}

type evalRow struct {
	Ref     string   `json:"eval"`
	Family  string   `json:"family"`
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`
	Created string   `json:"created_at"`
}

func latestEvals(l *library.Library) []evalRow {
	names, _ := l.Manifests("eval")
	latest := map[string]map[string]any{}
	for _, n := range names {
		var rec map[string]any
		if l.Manifest("eval", n, &rec) != nil {
			continue
		}
		id := rec["eval_id"].(string)
		if cur, ok := latest[id]; !ok || rec["version"].(float64) > cur["version"].(float64) {
			latest[id] = rec
		}
	}
	var rows []evalRow
	for id, rec := range latest {
		r := evalRow{Ref: fmt.Sprintf("%s@%d", id, int(rec["version"].(float64))), Family: fmt.Sprint(rec["family"]), State: fmt.Sprint(rec["state"]), Created: fmt.Sprint(rec["created_at"])}
		for _, x := range rec["reasons"].([]any) {
			r.Reasons = append(r.Reasons, fmt.Sprint(x))
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Created > rows[j].Created })
	return rows
}

func evalList(e *Env, args []string) int {
	fs := flags(e, "eval", evalEffects)
	asJSON := fs.Bool("json", false, "print as JSON")
	if fs.Parse(args) != nil {
		evalUsage(e)
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	p := s.Cfg.Evals
	l := library.Open(s, p.Limits().QuotaBytes)
	rows := latestEvals(l)
	refused := map[string]int{}
	names, _ := l.Indexes("evalmap")
	for _, n := range names {
		var m struct{ Refused []string }
		if l.Index(n, &m) == nil {
			for _, r := range m.Refused {
				refused[r]++
			}
		}
	}
	if *asJSON {
		return writeJSON(e, map[string]any{"collection": state(p), "evals": rows, "refused_episodes": refused})
	}
	fmt.Fprintf(e.Stdout, "Eval collection: %s\n", state(p))
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "No evals yet.")
	}
	for _, r := range rows {
		fmt.Fprintf(e.Stdout, "  %-24s %-14s %-13s %s\n", r.Ref, r.Family, r.State, strings.Join(r.Reasons, ", "))
	}
	if len(refused) > 0 {
		var parts []string
		for k, v := range refused {
			parts = append(parts, fmt.Sprintf("%s %d", k, v))
		}
		sort.Strings(parts)
		fmt.Fprintf(e.Stdout, "Episodes not packaged: %s\n", strings.Join(parts, ", "))
	}
	return 0
}

func state(p *evals.Policy) string {
	if p == nil || p.Capture != "on" {
		return "off (git hint eval on)"
	}
	return fmt.Sprintf("on for %s; container runs %s", strings.Join(p.Agents, ", "), allowed(p.Check(evals.OpExecute, "")))
}

// valueFlags take a separate value token (`-o FILE`).
var valueFlags = map[string]bool{"-with": true, "--with": true, "-server": true, "--server": true, "-o": true, "--o": true, "-agent": true, "--agent": true, "-attempts": true, "--attempts": true, "-arm": true, "--arm": true, "-apply": true, "--apply": true}

// flagsFirst moves flag tokens (with their values) before positional ones, so
// `eval show ID --text` and `eval pack ID -o FILE` work.
func flagsFirst(args []string) []string {
	var f, p []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			p = append(p, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			f = append(f, a)
			if valueFlags[a] && i+1 < len(args) {
				f = append(f, args[i+1])
				i++
			}
		default:
			p = append(p, a)
		}
	}
	return append(f, p...)
}

func evalShow(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval show", evalEffects)
	withText := fs.Bool("text", false, "print the local instruction text (it stays on this machine)")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, 0)
	ref := fs.Arg(0)
	if !strings.Contains(ref, "@") {
		for _, r := range latestEvals(l) {
			if strings.HasPrefix(r.Ref, ref+"@") {
				ref = r.Ref
			}
		}
	}
	var rec map[string]any
	if err := l.Manifest("eval", ref, &rec); err != nil {
		return fail(e, fmt.Errorf("no eval %s", fs.Arg(0)))
	}
	if *asJSON {
		return writeJSON(e, rec)
	}
	w := e.Stdout
	fmt.Fprintf(w, "Eval %s: %s, %s\n", ref, rec["family"], rec["state"])
	if rs := rec["reasons"].([]any); len(rs) > 0 {
		fmt.Fprintf(w, "Reasons: %v\n", rs)
	}
	fmt.Fprintf(w, "Requirements: %d (%d without a check); checks: %d; image: %s\n", len(rec["requirements"].([]any)), len(build.Unmapped(rec)),
		len(rec["verifier"].(map[string]any)["checks"].([]any)), rec["environment"].(map[string]any)["image"])
	cov := map[string]map[string]any{}
	if q, ok := rec["qualification"].(map[string]any); ok {
		rs, _ := q["requirements"].([]any)
		for _, r := range rs {
			m := r.(map[string]any)
			cov[m["req_id"].(string)] = m
		}
	}
	for _, r := range rec["requirements"].([]any) {
		m := r.(map[string]any)
		mapping, _ := m["mapping"].(string)
		if mapping == "" {
			mapping = "construction"
		}
		line := fmt.Sprintf("  %-4s %-13s %-12s checks %v", m["req_id"], m["origin"], mapping, m["checks"])
		if ts, ok := m["tests"].([]any); ok {
			line += fmt.Sprintf(" tests %d", len(ts))
		}
		if c := cov[m["req_id"].(string)]; c != nil {
			line += fmt.Sprintf(" coverage %v", c["status"])
			if c["reason"] != nil {
				line += fmt.Sprintf(" (%v)", c["reason"])
			}
		}
		fmt.Fprintln(w, line)
	}
	if cs, _ := rec["context"].([]any); len(cs) > 0 {
		fmt.Fprintf(w, "Context confirmed by the user: %d part(s)\n", len(cs))
	}
	if as, _ := rec["alternatives"].([]any); len(as) > 0 {
		claimed := 0
		for _, a := range as {
			if a.(map[string]any)["claimed_by"] != nil {
				claimed++
			}
		}
		fmt.Fprintf(w, "Alternative solutions: %d (%d claimed independent by the user; a claim, not checked)\n", len(as), claimed)
	}
	if q, ok := rec["qualification"].(map[string]any); ok {
		fmt.Fprintf(w, "Qualification (policy %v):\n", q["policy"])
		for _, c := range q["checks"].([]any) {
			m := c.(map[string]any)
			line := fmt.Sprintf("  %-20s %-14s runs %v", m["name"], m["status"], m["runs"])
			if by, ok := m["claimed_by"]; ok {
				line += fmt.Sprintf(" (independence claimed by %v)", by)
			}
			fmt.Fprintln(w, line)
		}
	}
	if *withText {
		if b, err := l.GetObject(rec["instruction"].(string)); err == nil {
			fmt.Fprintf(w, "\nInstruction:\n%s\n", b)
		}
	}
	return 0
}

func evalForget(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval forget", "deletes local eval data; no network")
	all := fs.Bool("all", false, "delete the whole eval library of this project")
	if fs.Parse(args) != nil || (*all == (fs.NArg() > 0)) {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, 0)
	var rep library.Report
	if *all {
		rep, err = l.Purge()
	} else {
		rep, err = l.Forget(fs.Args())
	}
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Deleted %d records and %d objects.\n", rep.Manifests, rep.Objects)
	if len(rep.Kept) > 0 {
		fmt.Fprintf(e.Stdout, "Kept (pinned): %s\n", strings.Join(rep.Kept, ", "))
	}
	for _, x := range rep.External {
		fmt.Fprintf(e.Stdout, "Still on disk outside the library (%s): %s\n", x.Kind, x.Name)
	}
	return 0
}

// evalPass runs the eval work after collect (when the policy is on) and prints one line.
func evalPass(e *Env, s *store.Store) {
	env := pipeline.DefaultEnv
	if e.EvalEnv != nil {
		env = e.EvalEnv
	}
	spawn := func() error { return pipeline.SpawnWorker(s) }
	if e.Spawn != nil {
		spawn = func() error { return e.Spawn("eval", "__qualify") }
	}
	r := pipeline.AfterCollect(context.Background(), s, env(), spawn)
	if r == nil {
		return
	}
	if !r.Ran {
		fmt.Fprintf(e.Stdout, "Evals: %s\n", r.Blocked)
		return
	}
	fmt.Fprintf(e.Stdout, "Evals: %d sessions imported, %d episodes, %d packaged, %d refused, %d waiting for qualification",
		r.Imported, r.Episodes, r.Packaged, r.Refused, r.Pending)
	if r.Stopped != "" {
		fmt.Fprintf(e.Stdout, " (pass stopped: %s)", r.Stopped)
	}
	fmt.Fprintln(e.Stdout)
	switch r.Worker {
	case "started":
		fmt.Fprintln(e.Stdout, "Evals: qualification runs in the background (git hint eval shows the result).")
	case "failed":
		fmt.Fprintln(e.Stdout, "Evals: qualification could not start.")
	}
}

// evalWorker qualifies pending evals within the policy budget; one worker per project.
func evalWorker(e *Env, _ []string) int {
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	lockp := s.Path("state", "evals", "worker.lock")
	if err := s.CheckPath(lockp); err != nil {
		return fail(e, err)
	}
	os.MkdirAll(filepath.Dir(lockp), 0o700)
	f, err := os.OpenFile(lockp, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail(e, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(e.Stdout, "another qualification worker runs; nothing to do")
		return 0
	}
	h, err := runner.FindHarbor(context.Background())
	if err != nil {
		var u = runner.ErrUnavailable
		if errors.Is(err, u) {
			fmt.Fprintf(e.Stdout, "%s %v\n", e.Now().UTC().Format("2006-01-02T15:04:05Z"), err)
			return 1
		}
		return fail(e, err)
	}
	rep := pipeline.Qualify(context.Background(), s, h, e.Now())
	fmt.Fprintf(e.Stdout, "%s qualified %d (trials %d) states %v %s %s\n", e.Now().UTC().Format("2006-01-02T15:04:05Z"), rep.Qualified, rep.Trials, rep.States, rep.Blocked, rep.Stopped)
	return 0
}

func evalRun(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval run", "runs agents in containers (policy evals.run); a live agent uses only the local model of the policy; sends nothing to a HINT server")
	agent := fs.String("agent", "opencode", "agent: opencode (local model of the policy), oracle or nop (checks without a model)")
	attempts := fs.Int("attempts", 1, "attempts per eval")
	arm := fs.String("arm", "baseline", "label for comparisons: "+strings.Join(pipeline.Arms, ", "))
	asJSON := fs.Bool("json", false, "print the result as JSON")
	var with repoFlags
	fs.Var(&with, "with", "context_only: a file to add to each task's repository (repeatable); refactor: the work tree of the refactored repository")
	ctxPolicy := fs.String("context-policy", "", "not supported yet: a HINT context policy as the treatment of a context_only arm")
	if fs.Parse(args) != nil {
		return 2
	}
	if *ctxPolicy != "" {
		// a policy that is only written into the trial would be a label-only arm; the eval
		// container cannot run hint_context yet, so the treatment cannot be applied
		return fail(e, fmt.Errorf("--context-policy is not supported: the eval harness cannot apply a HINT context policy inside the task container yet, so no comparison is made; use --arm context_only --with FILE for a context variant"))
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	p := s.Cfg.Evals
	if err := p.Check(evals.OpExecute, ""); err != nil {
		return fail(e, err)
	}
	ctx := context.Background()
	evalsToRun := fs.Args()
	if len(with) > 0 {
		refs, ready, err := prepareVariants(e, s, *arm, fs.Args(), with)
		if err != nil {
			return fail(e, err)
		}
		if !ready {
			return 1
		}
		evalsToRun = refs
	}
	h, err := runner.FindHarbor(ctx)
	if err != nil {
		return fail(e, err)
	}
	o := pipeline.SuiteOptions{Evals: evalsToRun, Agent: *agent, Attempts: *attempts, Arm: *arm}
	if *agent == "opencode" {
		if err := p.Check(evals.OpModel, ""); err != nil {
			return fail(e, err)
		}
		lv, err := runner.StartLive(runner.LiveConfig{BaseURL: p.Model.BaseURL, Model: p.Model.Model, KeyEnv: p.Model.KeyEnv, AgentVersion: runner.OpenCodeVersion})
		if err != nil {
			return fail(e, err)
		}
		defer lv.Close()
		o.Live = lv
	}
	rep, err := pipeline.RunSuite(ctx, s, h, h.Version, o, e.Now())
	if err != nil {
		return fail(e, err)
	}
	if *asJSON {
		return writeJSON(e, rep)
	}
	fmt.Fprintf(e.Stdout, "Suite %s: %d trials, outcomes %v", rep.Suite, rep.Trials, rep.Outcomes)
	if rep.Requests != nil {
		fmt.Fprintf(e.Stdout, ", model requests %d", *rep.Requests)
	}
	if rep.Stopped != "" {
		fmt.Fprintf(e.Stdout, " (stopped: %s)", rep.Stopped)
	}
	fmt.Fprintln(e.Stdout)
	for _, sk := range rep.Skipped {
		fmt.Fprintf(e.Stdout, "  not in the suite: %s\n", sk)
	}
	return 0
}

func evalCompare(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval compare", "reads local trial records; no network")
	asJSON := fs.Bool("json", false, "print as JSON")
	if fs.Parse(args) != nil || fs.NArg() > 1 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	rows, err := pipeline.Compare(library.Open(s, 0), fs.Arg(0))
	if err != nil {
		return fail(e, err)
	}
	if *asJSON {
		return writeJSON(e, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "No trials yet (git hint eval run).")
		return 0
	}
	num := func(p *int64) string {
		if p == nil {
			return "unknown"
		}
		return fmt.Sprint(*p)
	}
	fmt.Fprintf(e.Stdout, "%-19s %-20s %-13s %-22s %6s %6s %6s %8s %7s %9s %10s %10s %8s\n", "objective", "suite", "arm", "agent", "trials", "passed", "failed", "timeouts", "errors", "requests", "in tokens", "out tokens", "median s")
	for _, r := range rows {
		med := "unknown"
		if r.MedianWall != nil {
			med = fmt.Sprintf("%.0f", *r.MedianWall)
		}
		obj := r.Objective
		if obj == "" {
			obj = "-"
		}
		fmt.Fprintf(e.Stdout, "%-19s %-20s %-13s %-22s %6d %6d %6d %8d %7d %9s %10s %10s %8s\n", obj, r.Suite, r.Arm, r.Agent, r.Trials, r.Passed, r.Failed,
			r.Timeouts, r.Errors+r.Budget, num(r.Requests), num(r.InputTokens), num(r.OutputTokens), med)
	}
	fmt.Fprintln(e.Stdout, "Raw counts only: failed and timed-out attempts and their resources count. Unknown usage stays unknown.")
	fmt.Fprintln(e.Stdout, "Arms pair only within one objective (a baseline eval and its variants).")
	return 0
}

func evalPack(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval pack", "reads local evals; writes a private bundle file; sends nothing")
	out := fs.String("o", "", "bundle file to write (private: holds instructions, file states and solutions)")
	if fs.Parse(args) != nil || *out == "" || fs.NArg() == 0 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, 0)
	latest := map[string]string{}
	for _, r := range latestEvals(l) {
		id, _, _ := strings.Cut(r.Ref, "@")
		latest[id] = r.Ref
	}
	var refs []string
	for _, a := range fs.Args() {
		if !strings.Contains(a, "@") {
			if r, ok := latest[a]; ok {
				a = r
			}
		}
		refs = append(refs, a)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(e, err)
	}
	idx, perr := bundle.Pack(l, refs, f)
	cerr := f.Close()
	if perr != nil || cerr != nil {
		os.Remove(*out)
		if perr == nil {
			perr = cerr
		}
		return fail(e, perr)
	}
	fmt.Fprintf(e.Stdout, "Wrote %s: %d evals, %d objects. It holds private task content; keep it on your own machines.\n", *out, len(idx.Evals), len(idx.Objects))
	return 0
}

func evalUnpack(e *Env, args []string) int {
	fs := flags(e, "eval unpack", "reads a bundle file; writes local state; sends nothing")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return fail(e, err)
	}
	defer f.Close()
	res, err := bundle.Unpack(library.Open(s, s.Cfg.Evals.Limits().QuotaBytes), f)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Read %d evals (%s) with %d objects.\n", len(res.Evals), strings.Join(res.Evals, ", "), res.Objects)
	return 0
}

func evalReport(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval report", "reads local trial records; writes a report file; --send SENDS the minimized report to the configured server")
	out := fs.String("o", "", "report file (default .hint/state/evals/reports/<report>.json)")
	send := fs.Bool("send", false, "send the report to the configured server (needs evals.export = results)")
	yes := fs.Bool("yes", false, "approve sending without a prompt")
	server := fs.String("server", "", "server base URL (default: the project setting)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, s.Cfg.Evals.Limits().QuotaBytes)
	names, err := report.Unreported(l)
	if err != nil {
		return fail(e, err)
	}
	if len(names) == 0 {
		fmt.Fprintln(e.Stdout, "No new trial results to report.")
		return 0
	}
	data, used, err := report.Builder{Key: s.Key(), Namespace: s.Cfg.ProjectNamespace}.Build(l, names, e.Now())
	if err != nil {
		return fail(e, err)
	}
	var meta struct {
		ReportID      string `json:"report_id"`
		ContentDigest string `json:"content_digest"`
	}
	json.Unmarshal(data, &meta)
	path := *out
	if path == "" {
		path = s.Path("state", "evals", "reports", meta.ReportID+".json")
		if err := s.CheckPath(path); err != nil {
			return fail(e, err)
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Report %s: %d trial results in %s.\n", meta.ReportID, len(used), path)
	fmt.Fprintln(e.Stdout, "It holds outcomes, counters and opaque references only: no task text, code, path, command, log or local ID.")
	if !*send {
		return 0
	}
	if err := s.Cfg.Evals.Check(evals.OpExport, ""); err != nil {
		return fail(e, err)
	}
	c, err := api.New(serverURL(s, *server))
	if err != nil {
		return fail(e, err)
	}
	if !*yes {
		if !e.IsTerminal() {
			return fail(e, fmt.Errorf("sending the report to %s needs approval; read %s, then run again with --yes", c.Origin(), path))
		}
		fmt.Fprintf(e.Stdout, "Send report %s (%d trial results) to %s? [y/N] ", meta.ReportID, len(used), c.Origin())
		var ans string
		fmt.Fscanln(e.Stdin, &ans)
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(e.Stdout, "Not sent.")
			return 1
		}
	}
	rc, err := c.PostEvalReport(context.Background(), data, meta.ReportID, meta.ContentDigest)
	if err != nil {
		return fail(e, err)
	}
	if err := report.MarkReported(l, used); err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Sent: the server answered %s.\n", rc.Status)
	return 0
}

func evalPin(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval pin", "writes local state; no network")
	off := fs.Bool("off", false, "remove the pin")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, 0)
	ref := fs.Arg(0)
	if !strings.Contains(ref, "@") && !strings.HasPrefix(ref, "su-") {
		for _, r := range latestEvals(l) {
			if strings.HasPrefix(r.Ref, ref+"@") {
				ref = r.Ref
			}
		}
	}
	var probe map[string]any
	kind := "eval"
	if strings.HasPrefix(ref, "su-") {
		kind = "suite"
	}
	if l.Manifest(kind, ref, &probe) != nil {
		return fail(e, fmt.Errorf("no %s %s", kind, fs.Arg(0)))
	}
	if err := l.Pin(ref, !*off); err != nil {
		return fail(e, err)
	}
	if *off {
		fmt.Fprintf(e.Stdout, "%s is no longer pinned; retention applies again.\n", ref)
	} else {
		fmt.Fprintf(e.Stdout, "%s is pinned: retention and forget keep it.\n", ref)
	}
	return 0
}

// evalMap prints the requirement mapping file of an eval version, or applies the user's
// edited file: links of requests and corrections to tests, confirmed context and existing
// alternative solutions. Apply stores a new packaged version; qualification then runs as
// after collect, within the policy.
func evalMap(e *Env, args []string) int {
	args = flagsFirst(args)
	fs := flags(e, "eval map", "writes local state only; runs code in containers only when the eval policy allows it; sends nothing")
	apply := fs.String("apply", "", "the edited mapping file to check and store")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	l := library.Open(s, 0)
	ref := fs.Arg(0)
	if !strings.Contains(ref, "@") {
		for _, r := range latestEvals(l) {
			if strings.HasPrefix(r.Ref, ref+"@") {
				ref = r.Ref
			}
		}
	}
	if *apply == "" {
		f, err := pipeline.MapDraft(l, ref)
		if err != nil {
			return fail(e, fmt.Errorf("no eval %s", fs.Arg(0)))
		}
		return writeJSON(e, f)
	}
	data, err := os.ReadFile(*apply)
	if err != nil {
		return fail(e, err)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	name, unmapped, err := pipeline.ApplyMapFile(context.Background(), l, fs.Arg(0), data, filepath.Dir(*apply), e.Now())
	lock.Unlock()
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Stored %s (packaged).", name)
	if len(unmapped) > 0 {
		fmt.Fprintf(e.Stdout, " Requirements without a test: %s; qualification will end in needs_review.", strings.Join(unmapped, ", "))
	}
	fmt.Fprintln(e.Stdout)
	spawn := func() error { return pipeline.SpawnWorker(s) }
	if e.Spawn != nil {
		spawn = func() error { return e.Spawn("eval", "__qualify") }
	}
	switch {
	case s.Cfg.Evals.Check(evals.OpExecute, "") != nil:
		fmt.Fprintln(e.Stdout, "Container runs are off: qualification waits (git hint eval on --run docker).")
	case spawn() != nil:
		fmt.Fprintln(e.Stdout, "Qualification could not start; the next collect starts it again.")
	default:
		fmt.Fprintln(e.Stdout, "Qualification runs in the background (git hint eval show shows the result).")
	}
	return 0
}

// prepareVariants makes (or finds) the variant of each named baseline eval for a variant
// arm. It reports ready only when every variant is validated; otherwise it lists their
// states, starts the qualification worker when variants wait, and nothing runs.
func prepareVariants(e *Env, s *store.Store, arm string, ids, with []string) ([]string, bool, error) {
	if arm != "context_only" && arm != "refactor" {
		return nil, false, fmt.Errorf("--with is only for --arm context_only or --arm refactor")
	}
	if len(ids) == 0 {
		return nil, false, fmt.Errorf("name the baseline evals of the variant (eval run ID... --arm %s --with ...)", arm)
	}
	if arm == "refactor" && len(with) != 1 {
		return nil, false, fmt.Errorf("--arm refactor takes one --with: the work tree of the refactored repository")
	}
	l := library.Open(s, s.Cfg.Evals.Limits().QuotaBytes)
	latest := map[string]string{}
	for _, r := range latestEvals(l) {
		id, _, _ := strings.Cut(r.Ref, "@")
		latest[id] = r.Ref
	}
	for i, w := range with {
		if !filepath.IsAbs(w) {
			with[i] = filepath.Join(e.Dir, w)
		}
	}
	var refs []string
	ready, pending := true, false
	for _, id := range ids {
		ref := id
		if !strings.Contains(ref, "@") {
			if ref = latest[id]; ref == "" {
				return nil, false, fmt.Errorf("no eval %s", id)
			}
		}
		var rec map[string]any
		if err := l.Manifest("eval", ref, &rec); err != nil {
			return nil, false, fmt.Errorf("no eval %s", id)
		}
		in := pipeline.VariantInput{Arm: arm}
		repoName := fmt.Sprint(rec["repositories"].([]any)[0].(map[string]any)["repo"])
		if arm == "refactor" {
			tree, err := trees.FromWorktree(context.Background(), l, with[0], trees.Limits{})
			if err != nil {
				return nil, false, fmt.Errorf("the refactored work tree: %v", err)
			}
			in.Base = tree
		} else {
			root := ""
			for _, r := range s.Cfg.Repos {
				if r.Name == repoName {
					root = s.RepoRoot(r)
				}
			}
			if root == "" {
				return nil, false, fmt.Errorf("the repository %s of %s is not in this project", repoName, ref)
			}
			for _, w := range with {
				abs, _ := filepath.Abs(w)
				rel, err := filepath.Rel(root, abs)
				fi, serr := os.Lstat(abs)
				if err != nil || strings.HasPrefix(rel, "..") || serr != nil || !fi.Mode().IsRegular() {
					return nil, false, fmt.Errorf("%s is not a regular file inside the repository %s", w, repoName)
				}
				data, err := os.ReadFile(abs)
				if err != nil {
					return nil, false, err
				}
				mode := "100644"
				if fi.Mode()&0o111 != 0 {
					mode = "100755"
				}
				in.Context = append(in.Context, pipeline.ContextFile{Path: filepath.ToSlash(rel), Data: data, Mode: mode})
			}
		}
		v, err := pipeline.MakeVariant(l, ref, in, e.Now())
		if err != nil {
			return nil, false, err
		}
		refs = append(refs, v.Ref)
		if v.State != "validated" {
			ready = false
			pending = pending || v.State == "packaged"
			fmt.Fprintf(e.Stdout, "Variant %s of %s: %s (reference: %s); it runs when it is validated.\n", v.Ref, ref, v.State, v.Reference)
		}
	}
	if pending {
		spawn := func() error { return pipeline.SpawnWorker(s) }
		if e.Spawn != nil {
			spawn = func() error { return e.Spawn("eval", "__qualify") }
		}
		if spawn() == nil {
			fmt.Fprintln(e.Stdout, "Qualification of the variants runs in the background; run the same command again later.")
		}
	}
	return refs, ready, nil
}
