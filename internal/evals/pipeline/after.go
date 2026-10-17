package pipeline

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/store"
)

// Summary of the eval work after a collect, for CLI and MCP output. Counts only.
type Summary struct {
	Ran      bool   `json:"ran"`
	Blocked  string `json:"blocked,omitempty"`
	Imported int    `json:"sessions_imported"`
	Episodes int    `json:"episodes"`
	Packaged int    `json:"packaged"`
	Refused  int    `json:"refused"`
	Pending  int    `json:"waiting_for_qualification"`
	Stopped  string `json:"stopped,omitempty"`
	Worker   string `json:"worker"` // started, runs_off, none, failed
}

// AfterCollect runs one pass when the eval policy is on and starts the qualification
// worker when evals wait and container runs are allowed. It returns nil when eval
// collection is off (nothing ran, nothing was created).
func AfterCollect(ctx context.Context, s *store.Store, env Env, spawn func() error) *Summary {
	if s.Cfg.Evals.Check(evals.OpCapture, "") != nil {
		return nil
	}
	r := Run(ctx, s, env)
	sum := &Summary{Ran: r.Ran, Blocked: r.Blocked, Episodes: r.Episodes.Proposed, Packaged: r.Packages.Packaged,
		Refused: r.Packages.Refused, Pending: r.Pending, Stopped: r.Stopped, Worker: "none"}
	for _, i := range r.Imports {
		sum.Imported += i.New + i.Updated
	}
	if r.Pending > 0 {
		switch {
		case s.Cfg.Evals.Check(evals.OpExecute, "") != nil:
			sum.Worker = "runs_off"
		case spawn() != nil:
			sum.Worker = "failed"
		default:
			sum.Worker = "started"
		}
	}
	return sum
}

// SpawnWorker starts `git-hint eval __qualify` detached in the project root, with its
// output in .hint/state/evals/worker.log.
func SpawnWorker(s *store.Store) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logp := s.Path("state", "evals", "worker.log")
	if err := s.CheckPath(logp); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Path("state", "evals"), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(logp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "eval", "__qualify")
	cmd.Dir, cmd.Stdout, cmd.Stderr = s.Root, lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
