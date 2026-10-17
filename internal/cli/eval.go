package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/bundle"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
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
  git hint eval run [ID...] [--agent opencode|oracle|nop] [--attempts N] [--arm ARM]
                                     freeze a suite of validated evals and run bounded trials
  git hint eval compare [SUITE]      raw results per arm and agent (every attempt counts)
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
	case "run":
		return evalRun(e, args[1:])
	case "compare":
		return evalCompare(e, args[1:])
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

// flagsFirst moves flag tokens before positional ones, so `eval show ID --text` works.
func flagsFirst(args []string) []string {
	var f, p []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			f = append(f, a)
		} else {
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
	fmt.Fprintf(w, "Requirements: %d; checks: %d; image: %s\n", len(rec["requirements"].([]any)),
		len(rec["verifier"].(map[string]any)["checks"].([]any)), rec["environment"].(map[string]any)["image"])
	if q, ok := rec["qualification"].(map[string]any); ok {
		for _, c := range q["checks"].([]any) {
			m := c.(map[string]any)
			fmt.Fprintf(w, "  %-18s %-14s runs %v\n", m["name"], m["status"], m["runs"])
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
	if fs.Parse(args) != nil {
		return 2
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
	h, err := runner.FindHarbor(ctx)
	if err != nil {
		return fail(e, err)
	}
	o := pipeline.SuiteOptions{Evals: fs.Args(), Agent: *agent, Attempts: *attempts, Arm: *arm}
	if *agent == "opencode" {
		if err := p.Check(evals.OpModel, ""); err != nil {
			return fail(e, err)
		}
		lv, err := runner.StartLive(runner.LiveConfig{BaseURL: p.Model.BaseURL, Model: p.Model.Model, KeyEnv: p.Model.KeyEnv, AgentVersion: "2.0.26"})
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
	fmt.Fprintf(e.Stdout, "%-20s %-13s %-22s %6s %6s %6s %8s %7s %9s %10s %10s %8s\n", "suite", "arm", "agent", "trials", "passed", "failed", "timeouts", "errors", "requests", "in tokens", "out tokens", "median s")
	for _, r := range rows {
		med := "unknown"
		if r.MedianWall != nil {
			med = fmt.Sprintf("%.0f", *r.MedianWall)
		}
		fmt.Fprintf(e.Stdout, "%-20s %-13s %-22s %6d %6d %6d %8d %7d %9s %10s %10s %8s\n", r.Suite, r.Arm, r.Agent, r.Trials, r.Passed, r.Failed,
			r.Timeouts, r.Errors+r.Budget, num(r.Requests), num(r.InputTokens), num(r.OutputTokens), med)
	}
	fmt.Fprintln(e.Stdout, "Raw counts only: failed and timed-out attempts and their resources count. Unknown usage stays unknown.")
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
