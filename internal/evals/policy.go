package evals

import (
	"fmt"
	"slices"
)

// Policy is the `evals` object of .hint/state/project.json. A missing object or capture "off"
// collects nothing. Capture, model calls, code execution and result export are separate
// permissions: enabling one enables none of the others. Only the user writes the policy
// (CLI or file); MCP tools and agent text cannot change it.
type Policy struct {
	PolicyVersion int      `json:"policy_version"`
	Capture       string   `json:"capture"`
	Agents        []string `json:"agents,omitempty"`
	ImportRoots   []string `json:"import_roots,omitempty"`
	Model         *Model   `json:"model,omitempty"`
	Run           *Run     `json:"run,omitempty"`
	Disclosure    string   `json:"disclosure,omitempty"`
	Export        string   `json:"export,omitempty"`
	RetentionDays int      `json:"retention_days,omitempty"`
	QuotaMB       int      `json:"quota_mb,omitempty"`
	Batch         *Batch   `json:"batch,omitempty"`
}

// Model is the optional LAN generation provider. The key is read from the environment
// variable KeyEnv at use time; it is never stored in the policy or in any artifact.
type Model struct {
	Use               string `json:"use"`
	BaseURL           string `json:"base_url,omitempty"`
	Model             string `json:"model,omitempty"`
	KeyEnv            string `json:"key_env,omitempty"`
	MaxRequestsPerJob int    `json:"max_requests_per_job,omitempty"`
	MaxRetries        int    `json:"max_retries,omitempty"`
	MaxSecondsPerJob  int    `json:"max_seconds_per_job,omitempty"`
}

// Run is the optional code execution through the external runner.
type Run struct {
	Use                 string  `json:"use"`
	Runner              string  `json:"runner,omitempty"`
	MaxTrialsPerRun     int     `json:"max_trials_per_run,omitempty"`
	MaxSecondsPerTrial  int     `json:"max_seconds_per_trial,omitempty"`
	MaxRequestsPerTrial int     `json:"max_requests_per_trial,omitempty"`
	CPUs                float64 `json:"cpus,omitempty"`
	MemoryMB            int     `json:"memory_mb,omitempty"`
}

// Batch bounds one pipeline pass.
type Batch struct {
	Episodes int `json:"episodes"`
	Seconds  int `json:"seconds"`
}

// Defaults of an enabled policy (finite before capture can start).
const (
	DefaultRetentionDays = 90
	DefaultQuotaMB       = 2048
	DefaultBatchEpisodes = 20
	DefaultBatchSeconds  = 300
	DefaultModelRequests = 20
	DefaultModelSeconds  = 600
	DefaultTrials        = 20
	DefaultTrialSeconds  = 1800
	DefaultTrialRequests = 50
)

// SupportedAgents are the session sources of this version.
var SupportedAgents = []string{"opencode", "claude-code", "codex"}

// Enabled returns the policy that `git hint eval on` writes: capture on for the given
// agents; model calls, execution and export stay off.
func Enabled(agents []string) Policy {
	if len(agents) == 0 {
		agents = []string{"opencode"}
	}
	return Policy{PolicyVersion: 1, Capture: "on", Agents: slices.Clone(agents), Model: &Model{Use: "off"}, Run: &Run{Use: "off"},
		Disclosure: "local", Export: "off", RetentionDays: DefaultRetentionDays, QuotaMB: DefaultQuotaMB,
		Batch: &Batch{Episodes: DefaultBatchEpisodes, Seconds: DefaultBatchSeconds}}
}

// Op is an operation that the policy allows or denies.
type Op string

const (
	OpCapture Op = "capture"
	OpModel   Op = "model"
	OpExecute Op = "execute"
	OpExport  Op = "export"
)

// Denied explains why the policy refuses an operation.
type Denied struct {
	Op     Op
	Reason string
}

func (d *Denied) Error() string { return fmt.Sprintf("eval %s is not allowed: %s", d.Op, d.Reason) }

// Check reports whether p allows op for the given agent ("" when the operation has no
// agent). A nil policy denies everything.
func (p *Policy) Check(op Op, agent string) error {
	if p == nil || p.Capture != "on" {
		return &Denied{op, "eval collection is off (git hint eval on)"}
	}
	if err := ValidateValue("policy", p); err != nil {
		return &Denied{op, "the eval policy in .hint/state/project.json is invalid"}
	}
	switch op {
	case OpCapture:
		if agent != "" && !slices.Contains(p.Agents, agent) {
			return &Denied{op, agent + " is not in the eval policy agents"}
		}
	case OpModel:
		if p.Model == nil || p.Model.Use != "lan" || p.Model.BaseURL == "" || p.Model.Model == "" || p.Model.KeyEnv == "" {
			return &Denied{op, "local model use is off (policy evals.model)"}
		}
	case OpExecute:
		if p.Run == nil || p.Run.Use != "docker" {
			return &Denied{op, "code execution is off (policy evals.run)"}
		}
	case OpExport:
		if p.Export != "results" {
			return &Denied{op, "result export is off (policy evals.export)"}
		}
	default:
		return &Denied{op, "unknown operation"}
	}
	return nil
}

// Limits returns the effective limits with defaults filled in.
func (p *Policy) Limits() Limits {
	l := Limits{RetentionDays: DefaultRetentionDays, QuotaBytes: DefaultQuotaMB << 20, BatchEpisodes: DefaultBatchEpisodes,
		BatchSeconds: DefaultBatchSeconds, ModelRequests: DefaultModelRequests, ModelSeconds: DefaultModelSeconds,
		Trials: DefaultTrials, TrialSeconds: DefaultTrialSeconds, TrialRequests: DefaultTrialRequests}
	if p == nil {
		return l
	}
	set := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	set(&l.RetentionDays, p.RetentionDays)
	if p.QuotaMB > 0 {
		l.QuotaBytes = int64(p.QuotaMB) << 20
	}
	if p.Batch != nil {
		set(&l.BatchEpisodes, p.Batch.Episodes)
		set(&l.BatchSeconds, p.Batch.Seconds)
	}
	if p.Model != nil {
		set(&l.ModelRequests, p.Model.MaxRequestsPerJob)
		set(&l.ModelRetries, p.Model.MaxRetries)
		set(&l.ModelSeconds, p.Model.MaxSecondsPerJob)
	}
	if p.Run != nil {
		set(&l.Trials, p.Run.MaxTrialsPerRun)
		set(&l.TrialSeconds, p.Run.MaxSecondsPerTrial)
		set(&l.TrialRequests, p.Run.MaxRequestsPerTrial)
	}
	return l
}

// Limits are the finite budgets of the pipeline.
type Limits struct {
	RetentionDays                       int
	QuotaBytes                          int64
	BatchEpisodes, BatchSeconds         int
	ModelRequests, ModelRetries         int
	ModelSeconds                        int
	Trials, TrialSeconds, TrialRequests int
}
