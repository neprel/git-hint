package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/store"
)

// RunnerIdentity names the runner of a pass and its version.
type RunnerIdentity struct {
	Name    string // hint-docker
	Version string
}

// Targeter resolves the OpenCode target of an exported task before a live run (the built-in
// runner; test runners implement it too).
type Targeter interface {
	PreflightTarget(ctx context.Context, taskDir string) (string, error)
}

// ErrHarborRemoved refuses an explicit legacy policy runner "harbor": git-hint no longer runs
// Harbor. The policy is not rewritten and nothing falls back to another runner.
var ErrHarborRemoved = errors.New("this project's eval policy selects the Harbor runner, which git-hint no longer runs; select the built-in runner with: git hint eval on --runner docker")

// CheckRunner checks the policy's runner before any work: docker or a missing field is the
// built-in runner; an explicit harbor is refused with ErrHarborRemoved; any other value is
// unknown. It starts no process.
func CheckRunner(p *evals.Policy) error {
	name := ""
	if p != nil && p.Run != nil {
		name = p.Run.Runner
	}
	switch name {
	case "docker", "": // a missing field never means Harbor
		return nil
	case "harbor":
		return ErrHarborRemoved
	}
	return fmt.Errorf("unknown eval runner %q", name)
}

// SelectRunner is the one runner factory of eval run, the qualification worker and the MCP
// tool: the built-in container runner (it needs only a Docker engine), after CheckRunner. An
// unavailable engine is an error; there is no fallback. close releases it.
func SelectRunner(ctx context.Context, p *evals.Policy) (qualify.Runner, RunnerIdentity, func(), error) {
	if err := CheckRunner(p); err != nil {
		return nil, RunnerIdentity{}, nil, err
	}
	d, err := runner.FindDocker(ctx)
	if err != nil {
		return nil, RunnerIdentity{}, nil, err
	}
	return d, RunnerIdentity{Name: "hint-docker", Version: runner.DockerRunnerSemver}, func() { d.Close() }, nil
}

// jobResult is the container runner's result.json of a pass's single trial.
type jobResult struct {
	Container struct {
		ImageID     string                     `json:"agent_image"`
		StudyLaunch *runner.StudyLaunchReceipt `json:"study_launch"`
		Ledger      *runner.LedgerReport       `json:"ledger"`
		LedgerFinal *bool                      `json:"ledger_final"`
		SealedAt    string                     `json:"ledger_sealed_at"`
		AgentBinary struct {
			Target          string `json:"target"`
			Version         string `json:"version"`
			ExecutionDigest string `json:"execution_digest"`
		} `json:"agent_binary"`
		PrelaunchTree *runner.PrelaunchTreeCapture `json:"prelaunch_tree"`
		AgentOutput   map[string]string            `json:"agent_output"`
	} `json:"hint_container"`
}

func readJobResult(work string) (jobResult, error) {
	var r jobResult
	b, err := os.ReadFile(filepath.Join(work, "jobs", "trial", "trial__1", "result.json"))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// jobAgentTarget is the OpenCode target the attempt actually installed ("" when it did not).
func jobAgentTarget(work string) string {
	r, err := readJobResult(work)
	if err != nil {
		return ""
	}
	return r.Container.AgentBinary.Target
}

// ledgerConfiguration is the configuration a run's ledgers are bound to: the work-unit scope
// configuration of its manifest (one definition for the writer and the backend record helper).
func ledgerConfiguration(key []byte, m workunits.Manifest) (activity.Configuration, error) {
	var cfg activity.Configuration
	b, _ := json.Marshal(workunits.ScopeConfiguration(key, m))
	err := json.Unmarshal(b, &cfg)
	return cfg, err
}

// handOverLedger writes the attempt's model ledger as revision 0 through the shared ledger
// writer. A ledger that is not final, or whose finality is not stated, is incomplete with
// teardown_incomplete whatever its account says; an attempt without a ledger writes nothing
// (its usage stays unknown; no totals are made up).
func handOverLedger(s *store.Store, work string, m workunits.Manifest, trial, run, eval string, attempt int, task, family string) error {
	_, err := handOverLedgerWithReceipt(s, work, m, trial, run, eval, attempt, task, family)
	return err
}

func handOverLedgerWithReceipt(s *store.Store, work string, m workunits.Manifest, trial, run, eval string, attempt int, task, family string) (*LedgerWriteReceipt, error) {
	r, err := readJobResult(work)
	if err != nil {
		return nil, err
	}
	if r.Container.Ledger == nil {
		return nil, nil
	}
	if r.Container.SealedAt == "" {
		return nil, errors.New("the ledger has no seal time")
	}
	cfg, err := ledgerConfiguration(s.Key(), m)
	if err != nil {
		return nil, err
	}
	final := r.Container.LedgerFinal != nil && *r.Container.LedgerFinal
	_, receipt, err := WriteLedgerWithReceipt(s, LedgerInput{Trial: trial, Run: run, Eval: eval, Attempt: attempt, Task: task, Family: family, Agent: "opencode",
		Configuration: cfg, Revision: 0, SealedAt: r.Container.SealedAt, Report: *r.Container.Ledger, NotFinal: !final})
	return receipt, err
}
