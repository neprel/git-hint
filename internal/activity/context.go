package activity

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
)

// Context is the HINT v0.2 context measurement of an observation (Observation.context).
type Context struct {
	Action      string         `json:"action"`
	Stage       string         `json:"stage"`
	ArtifactID  string         `json:"artifact_id,omitempty"`
	RefID       string         `json:"ref_id,omitempty"`
	SnapshotID  string         `json:"snapshot_id,omitempty"`
	PolicyState string         `json:"policy_state"`
	PolicyRef   string         `json:"policy_ref,omitempty"`
	Freshness   string         `json:"freshness,omitempty"`
	Result      string         `json:"result"`
	Bytes       *int           `json:"bytes"`
	Omitted     *int           `json:"omitted,omitempty"`
	Tokens      *ContextTokens `json:"tokens,omitempty"`
}

// ContextTokens is a labeled token estimate.
type ContextTokens struct {
	Value            int    `json:"value"`
	Estimator        string `json:"estimator"`
	EstimatorVersion string `json:"estimator_version"`
	Label            string `json:"label"`
}

// contextPart is the measurement that the plugin read from the marker part of a hint_context
// answer: the file IDs of the call and the measurement of the tool's answer as sent.
type contextPart struct {
	Targets []string `json:"targets"`
	Context Context  `json:"context"`
}

var (
	hex32     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	estVer    = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,4}){0,2}$`)
	ctxEnums  = map[string][]string{"action": {"deliver", "expand", "retrieve_original"}, "policy_state": {"enabled", "disabled", "not_applicable", "unknown"}, "freshness": {"", "current", "stale", "unknown"}, "result": {"delivered", "refused_stale", "missing", "error"}}
	maxMarker = 2048
)

// parseContextPart validates the plugin's measurement strictly (unknown fields, enums, IDs,
// counts, the schema conditions). It returns false for anything else: the call then has
// missing context coverage, never a zero or invented measurement.
func parseContextPart(raw json.RawMessage) (contextPart, bool) {
	var p contextPart
	if len(raw) == 0 || len(raw) > maxMarker {
		return p, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.More() {
		return p, false
	}
	c := p.Context
	if len(p.Targets) > 4 || c.Stage != "raw_tool_output" || c.Bytes == nil || *c.Bytes < 0 || (c.Omitted != nil && *c.Omitted < 0) {
		return p, false
	}
	for _, t := range p.Targets {
		if !hex32.MatchString(t) {
			return p, false
		}
	}
	for k, vals := range map[string]string{"action": c.Action, "policy_state": c.PolicyState, "freshness": c.Freshness, "result": c.Result} {
		if !slices.Contains(ctxEnums[k], vals) {
			return p, false
		}
	}
	for _, id := range []string{c.ArtifactID, c.RefID, c.SnapshotID, c.PolicyRef} {
		if id != "" && !hex32.MatchString(id) {
			return p, false
		}
	}
	if (c.PolicyState == "enabled") != (c.PolicyRef != "") || (c.Result == "refused_stale" && c.Freshness != "stale") ||
		(c.Result == "delivered" && (c.ArtifactID == "" || c.SnapshotID == "")) {
		return p, false
	}
	if t := c.Tokens; t != nil && (t.Value < 0 || t.Estimator != "bytes-div-4" || !estVer.MatchString(t.EstimatorVersion) || t.Label != "estimate") {
		return p, false
	}
	return p, true
}
