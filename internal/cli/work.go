package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// evalWork is `git hint eval work start|close`: an explicit, optional interface for
// orchestrators that attribute the cost of a piece of work (for example a refactoring).
// It writes local state only; `eval report` sends the units.
func evalWork(e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, "usage: git hint eval work start LABEL [--plan PLAN [--from SETFILE]] [--family LABEL] | close LABEL --outcome completed|failed|abandoned")
		return 2
	}
	switch args[0] {
	case "start":
		return evalWorkStart(e, args[1:])
	case "close":
		return evalWorkClose(e, args[1:])
	}
	fmt.Fprintln(e.Stderr, "git hint eval work: start or close")
	return 2
}

func evalWorkStart(e *Env, args []string) int {
	fs := flags(e, "eval work start", "reads local state; writes a local work unit; no network")
	plan := fs.String("plan", "", "the plan this work realizes (from the last fetched set or --from)")
	from := fs.String("from", "", "recommendation_set file for --plan")
	family := fs.String("family", os.Getenv("HINT_TASK_FAMILY"), "the declared task family (default HINT_TASK_FAMILY)")
	if fs.Parse(flagsFirst(args)) != nil || fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "git hint eval work start: one LABEL")
		return 2
	}
	if *from != "" && *plan == "" {
		fmt.Fprintln(e.Stderr, "git hint eval work start: --from needs --plan")
		return 2
	}
	if *family == "" {
		fmt.Fprintln(e.Stderr, "git hint eval work start: a declared family is required (--family or HINT_TASK_FAMILY)")
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	var raw []byte
	if *plan != "" {
		if !hexID.MatchString(*plan) {
			fmt.Fprintln(e.Stderr, "git hint eval work start: --plan needs a plan ID (32 hexadecimal characters)")
			return 2
		}
		if raw, err = readSet(e, s, *from); err != nil {
			return fail(e, err)
		}
	}
	cfg, err := runConfiguration(s, os.Getenv("HINT_AGENT_VERSION"), os.Getenv("HINT_MODEL"), os.Getenv("HINT_CONFIG_REFS"))
	if err != nil {
		return fail(e, err)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	if _, err := project.WorkStart(s, fs.Arg(0), raw, *plan, *family, cfg, e.Now()); err != nil {
		return fail(e, err)
	}
	fmt.Fprintln(e.Stdout, "Work unit started. Run its agent tasks with HINT_WORK_UNIT set to the label (and HINT_TASK_ID per task).")
	return 0
}

func evalWorkClose(e *Env, args []string) int {
	fs := flags(e, "eval work close", "reads local state; writes a local work unit; no network")
	outcome := fs.String("outcome", "", "completed, failed (another attempt may follow) or abandoned")
	if fs.Parse(flagsFirst(args)) != nil || fs.NArg() != 1 || *outcome == "" {
		fmt.Fprintln(e.Stderr, "git hint eval work close: one LABEL and --outcome")
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
	u, err := project.WorkClose(s, fs.Arg(0), *outcome, e.Now())
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintf(e.Stdout, "Work unit attempt %s: %s (unit %s, revision %d).\n", *outcome, u.Attempts[len(u.Attempts)-1].Closure, u.State, len(u.Revisions))
	return 0
}

// readSet reads a recommendation set file, or the last fetched one.
func readSet(e *Env, s *store.Store, from string) ([]byte, error) {
	src := from
	var err error
	if src == "" {
		if src, err = project.LatestRecommendation(s); err != nil {
			return nil, err
		}
	} else if !filepath.IsAbs(src) {
		src = filepath.Join(e.Dir, src)
	}
	return readLimited(src, project.MaxDocumentBytes)
}

var errWorkWithoutTask = errors.New("HINT_WORK_UNIT needs HINT_TASK_ID; nothing was written")
