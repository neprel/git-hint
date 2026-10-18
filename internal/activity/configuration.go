package activity

import (
	"encoding/json"

	"github.com/neprel/git-hint/internal/hint"
)

// VersionState is a version that is known, not applicable or unknown (unknown is never not
// applicable).
type VersionState struct {
	State string `json:"state"` // known, not_applicable, unknown
	Value string `json:"value,omitempty"`
}

// RefState is a project-scoped opaque reference that is known or unknown.
type RefState struct {
	State string `json:"state"` // known, unknown
	Ref   string `json:"ref,omitempty"`
}

// ModelState is the effective model: known with its provenance (launch_parameter or
// agent_reported) or unknown. A declaration alone never makes it known.
type ModelState struct {
	State      string `json:"state"` // known, unknown
	Ref        string `json:"ref,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// ConfigPolicy is the effective context policy of a run (enabled carries ref and version).
type ConfigPolicy struct {
	State   string `json:"state"` // enabled, disabled, not_applicable, unknown
	Ref     string `json:"ref,omitempty"`
	Version int    `json:"version,omitempty"`
}

// ConfigRefs are the digests of the observed effective configuration files; known with an
// empty list is a known empty set.
type ConfigRefs struct {
	State string   `json:"state"` // known, unknown
	Refs  []string `json:"refs,omitempty"`
}

// MarshalJSON keeps a known empty list (a known empty set) and omits the list when unknown.
func (c ConfigRefs) MarshalJSON() ([]byte, error) {
	if c.State != "known" {
		return json.Marshal(map[string]any{"state": c.State})
	}
	refs := c.Refs
	if refs == nil {
		refs = []string{}
	}
	return json.Marshal(map[string]any{"state": c.State, "refs": refs})
}

// Limit is a run limit: known with a value, unbounded (explicitly no limit) or unknown.
type Limit struct {
	State string `json:"state"`
	Value int    `json:"value,omitempty"`
}

// Budget holds the request and time limits of the runs (both essential for comparison).
type Budget struct {
	MaxRequests Limit `json:"max_requests"`
	MaxSeconds  Limit `json:"max_seconds"`
}

// Declared holds the user's explicit settings (provenance only; never the effective value).
type Declared struct {
	Model *RefState `json:"model,omitempty"`
}

// Configuration is the canonical effective configuration of a source's runs (hint-data v0.2
// sources[].configuration; the same form and derivations as eval report work units).
type Configuration struct {
	Agent struct {
		Name    string       `json:"name"`
		Version VersionState `json:"version"`
	} `json:"agent"`
	Model   ModelState `json:"model"`
	Harness struct {
		Kind    string       `json:"kind"` // harbor, direct, other
		Version VersionState `json:"version"`
	} `json:"harness"`
	ContextPolicy ConfigPolicy `json:"context_policy"`
	ConfigRefs    ConfigRefs   `json:"config_refs"`
	Budget        Budget       `json:"budget"`
	Declared      *Declared    `json:"declared,omitempty"`
}

// Digest is the identity of the effective configuration (without the declared settings).
func (c Configuration) Digest() string {
	e := c
	e.Declared = nil
	b, _ := json.Marshal(e)
	var v any
	_ = json.Unmarshal(b, &v)
	d, _ := hint.Digest(v)
	return d
}

// ModelRef derives the project reference of a raw model name (the eval report derivation).
func ModelRef(key []byte, raw string) string { return hint.EvalRef(key, "model", raw) }

// ConfigRef derives the project reference of an observed configuration file (kind, digest).
func ConfigRef(key []byte, kind, digest string) string {
	return hint.EvalRef(key, "observed-config", kind, digest)
}

// FamilyRef derives the project reference of a raw task family label (the eval report
// task_family_ref derivation).
func FamilyRef(key []byte, raw string) string { return hint.EvalRef(key, "family", raw) }
