package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
)

// EvalsIn selects one eval (empty: all latest versions).
type EvalsIn struct {
	Eval string `json:"eval,omitempty" jsonschema:"eval ID or ID@version; empty lists every eval"`
}

// EvalItem is one eval without any session text, path or reference content.
type EvalItem struct {
	Eval    string            `json:"eval"`
	Family  string            `json:"family"`
	State   string            `json:"state"`
	Reasons []string          `json:"reasons"`
	Checks  map[string]string `json:"qualification"`
}

// EvalsOut is the read-only eval summary.
type EvalsOut struct {
	SideEffect string         `json:"side_effect"`
	Collection string         `json:"collection"`
	Evals      []EvalItem     `json:"evals"`
	Trials     []pipeline.Row `json:"trials"`
}

func (h *handlers) evals(ctx context.Context, _ *mcp.CallToolRequest, in EvalsIn) (*mcp.CallToolResult, EvalsOut, error) {
	s, err := h.open()
	if err != nil {
		return nil, EvalsOut{}, h.fail("hint_evals", err)
	}
	out := EvalsOut{SideEffect: "none", Collection: "off"}
	if p := s.Cfg.Evals; p != nil && p.Capture == "on" {
		out.Collection = "on"
	}
	l := library.Open(s, 0)
	latest, err := suites.Latest(l)
	if err != nil {
		return nil, EvalsOut{}, h.fail("hint_evals", err)
	}
	for ref, rec := range latest {
		id := fmt.Sprint(rec["eval_id"])
		if in.Eval != "" && in.Eval != ref && in.Eval != id {
			continue
		}
		it := EvalItem{Eval: ref, Family: fmt.Sprint(rec["family"]), State: fmt.Sprint(rec["state"]), Reasons: []string{}, Checks: map[string]string{}}
		for _, r := range rec["reasons"].([]any) {
			it.Reasons = append(it.Reasons, fmt.Sprint(r))
		}
		if q, ok := rec["qualification"].(map[string]any); ok {
			for _, c := range q["checks"].([]any) {
				m := c.(map[string]any)
				it.Checks[fmt.Sprint(m["name"])] = fmt.Sprint(m["status"])
			}
		}
		out.Evals = append(out.Evals, it)
	}
	if out.Trials, err = pipeline.Compare(l, ""); err != nil {
		return nil, EvalsOut{}, h.fail("hint_evals", err)
	}
	return text("%d evals; collection %s. No session text is returned.", len(out.Evals), out.Collection), out, nil
}

// EvalRunIn selects the evals and the agent of a bounded run.
type EvalRunIn struct {
	Evals    []string `json:"evals,omitempty" jsonschema:"eval IDs or versions; empty runs every validated eval"`
	Agent    string   `json:"agent,omitempty" jsonschema:"oracle or nop (checks without a model) or opencode (the local model of the user's eval policy)"`
	Attempts int      `json:"attempts,omitempty" jsonschema:"attempts per eval; the user's policy limits the total"`
	Arm      string   `json:"arm,omitempty" jsonschema:"comparison label: baseline, configuration, no_change, context_only, refactor"`
}

// EvalRunOut reports a run.
type EvalRunOut struct {
	SideEffect string               `json:"side_effect"`
	Report     pipeline.SuiteReport `json:"report"`
}

func (h *handlers) evalRun(ctx context.Context, _ *mcp.CallToolRequest, in EvalRunIn) (*mcp.CallToolResult, EvalRunOut, error) {
	s, err := h.open()
	if err != nil {
		return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
	}
	if !s.Cfg.MCPEvalRunAllowed() {
		return nil, EvalRunOut{}, h.fail("hint_eval_run", toolErr("eval_run_denied"))
	}
	p := s.Cfg.Evals
	if err := p.Check(evals.OpExecute, ""); err != nil {
		return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
	}
	agent := in.Agent
	if agent == "" {
		agent = "oracle"
	}
	h2, err := runner.FindHarbor(ctx)
	if err != nil {
		return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
	}
	o := pipeline.SuiteOptions{Evals: in.Evals, Agent: agent, Attempts: in.Attempts, Arm: in.Arm}
	if agent == "opencode" {
		if err := p.Check(evals.OpModel, ""); err != nil {
			return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
		}
		lv, err := runner.StartLive(runner.LiveConfig{BaseURL: p.Model.BaseURL, Model: p.Model.Model, KeyEnv: p.Model.KeyEnv, AgentVersion: "2.0.26"})
		if err != nil {
			return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
		}
		defer lv.Close()
		o.Live = lv
	}
	rep, err := pipeline.RunSuite(ctx, s, h2, h2.Version, o, h.o.Now())
	if err != nil {
		return nil, EvalRunOut{}, h.fail("hint_eval_run", err)
	}
	return text("Suite %s: %d trials %v.", rep.Suite, rep.Trials, rep.Outcomes), EvalRunOut{SideEffect: "local containers", Report: rep}, nil
}
