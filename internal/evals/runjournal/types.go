// Package runjournal holds the neutral stored controlled-run journal contract.
// Parsing establishes received input consistency only, never outcome or usage authority.
package runjournal

// Member is one frozen suite member of a run.
type Member struct {
	Eval           string `json:"eval"`
	EvalDigest     string `json:"eval_digest"`
	Baseline       string `json:"baseline"`
	BaselineDigest string `json:"baseline_digest"`
	Objective      string `json:"objective"`
	TaskFamily     string `json:"task_family"`
	Eligible       bool   `json:"eligible"` // a refactor eval, or a feature eval with a B1 (cli-file-v1) declaration, run by a live agent
	// Plan is the frozen plan binding of a refactor variant member (nil: unbound)
	Plan map[string]any `json:"plan,omitempty"`
}

// Manifest is the immutable plan of one run, written before the first attempt.
type Manifest struct {
	RunID         string   `json:"run_id"`
	Suite         string   `json:"suite"`
	Agent         string   `json:"agent"`
	AgentVersion  string   `json:"agent_version"`
	Arm           string   `json:"arm"`
	Attempts      int      `json:"attempts"`
	RunnerVersion string   `json:"runner_version"`
	ModelLabel    string   `json:"model_label"`
	LaunchDigest  string   `json:"launch_digest"`
	Harness       string   `json:"harness,omitempty"` // hint-docker; empty only in stored manifests of former Harbor runs
	MaxRequests   int      `json:"max_requests"`
	MaxSeconds    int      `json:"max_seconds"`
	Members       []Member `json:"members"`
	CreatedAt     string   `json:"created_at"`
}

// Unit is the journal of one controlled unit.
type Unit struct {
	Eval      string           `json:"eval"`
	UnitRef   string           `json:"unit_ref"`
	Scope     map[string]any   `json:"scope"`
	State     string           `json:"state"`
	Attempts  []map[string]any `json:"attempts"`
	Trials    []string         `json:"trials"` // recorded trial names (deduplication)
	Revisions []map[string]any `json:"revisions"`
	Reported  []string         `json:"reported,omitempty"`
}

// Run is the stored manifest with its units.
type Run struct {
	Manifest Manifest `json:"manifest"`
	Units    []*Unit  `json:"units"`
}
