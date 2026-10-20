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
	Name    string // harbor or hint-docker
	Version string
}

// SelectRunner is the one runner factory of eval run, the qualification worker and the MCP
// tool: the policy's runner, by default the built-in container runner (docker; it needs only
// a Docker engine). An explicit "harbor" selects the transitional developer harness (Harbor
// and Python), kept only for comparison. An unknown runner or an unavailable engine is an
// error; there is no fallback to another runner. close releases it.
func SelectRunner(ctx context.Context, p *evals.Policy) (qualify.Runner, RunnerIdentity, func(), error) {
	name := ""
	if p != nil && p.Run != nil {
		name = p.Run.Runner
	}
	switch name {
	case "docker", "": // the built-in runner is the default; a missing field never means Harbor
		d, err := runner.FindDocker(ctx)
		if err != nil {
			return nil, RunnerIdentity{}, nil, err
		}
		return d, RunnerIdentity{Name: "hint-docker", Version: runner.DockerRunnerSemver}, func() { d.Close() }, nil
	case "harbor": // explicit only: the transitional developer harness (Harbor and Python)
		h, err := runner.FindHarbor(ctx)
		if err != nil {
			return nil, RunnerIdentity{}, nil, err
		}
		return h, RunnerIdentity{Name: "harbor", Version: h.Version}, func() {}, nil
	}
	return nil, RunnerIdentity{}, nil, fmt.Errorf("unknown eval runner %q", name)
}

// jobResult is the container runner's result.json of a pass's single trial.
type jobResult struct {
	Container struct {
		Ledger      *runner.LedgerReport `json:"ledger"`
		LedgerFinal *bool                `json:"ledger_final"`
		SealedAt    string               `json:"ledger_sealed_at"`
		AgentBinary struct {
			Target string `json:"target"`
		} `json:"agent_binary"`
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

// handOverLedger writes the attempt's model ledger as revision 0 through the shared ledger
// writer. A ledger that is not final, or whose finality is not stated, is incomplete with
// teardown_incomplete whatever its account says; an attempt without a ledger writes nothing
// (its usage stays unknown; no totals are made up).
func handOverLedger(s *store.Store, work string, m workunits.Manifest, trial, run, eval string, attempt int, task, family string) error {
	r, err := readJobResult(work)
	if err != nil {
		return err
	}
	if r.Container.Ledger == nil {
		return nil
	}
	if r.Container.SealedAt == "" {
		return errors.New("the ledger has no seal time")
	}
	var cfg activity.Configuration
	b, _ := json.Marshal(workunits.ScopeConfiguration(s.Key(), m))
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	final := r.Container.LedgerFinal != nil && *r.Container.LedgerFinal
	_, err = WriteLedger(s, LedgerInput{Trial: trial, Run: run, Eval: eval, Attempt: attempt, Task: task, Family: family, Agent: "opencode",
		Configuration: cfg, Revision: 0, SealedAt: r.Container.SealedAt, Report: *r.Container.Ledger, NotFinal: !final})
	return err
}
