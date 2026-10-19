// Package activity turns agent events into HINT Observation records. Content (tool
// output, commands, prompts, paths) is read in memory only; the records hold opaque IDs,
// enumerations, numbers and timestamps.
package activity

import (
	"time"

	"github.com/neprel/git-hint/internal/scan"
)

// Version is the adapter and normalizer version written into records.
const Version = "0.2.0"

// Observation is the HINT Observation record (hint-data §7).
type Observation struct {
	ObservationID       string            `json:"observation_id"`
	Class               string            `json:"class"`
	Op                  string            `json:"op"`
	OpsSecondary        []string          `json:"ops_secondary,omitempty"`
	Source              string            `json:"source"`
	SourceID            string            `json:"source_id"`
	Precision           string            `json:"precision"`
	Time                Time              `json:"time"`
	Correlation         map[string]string `json:"correlation,omitempty"`
	Targets             []Target          `json:"targets,omitempty"`
	Outcome             Outcome           `json:"outcome"`
	Size                *scan.Quantity    `json:"size,omitempty"`
	CommandClass        string            `json:"command_class,omitempty"`
	MutationAttribution string            `json:"mutation_attribution,omitempty"`
	ContentDelivery     string            `json:"content_delivery,omitempty"`
	Coordination        *Coordination     `json:"coordination,omitempty"`
	Context             *Context          `json:"context,omitempty"`
	Usage               *Usage            `json:"usage,omitempty"`
	// RequestKind is the kind of a model call on MODEL_CALL and its terminal USAGE (draft v0.2).
	RequestKind string `json:"request_kind,omitempty"`
	// Collection is the collector closure of a session on its closing SESSION record (draft v0.2).
	Collection *Collection `json:"collection,omitempty"`
	// ModelLedger is the sealed account of one run's model traffic on its USAGE record (draft v0.2).
	ModelLedger *ModelLedger `json:"model_ledger,omitempty"`
	Missing     []string     `json:"missing"`
	Provenance  Provenance   `json:"provenance"`
}

// Usage is the token use of one model call (hint-data usage; cache and reasoning are separate
// quantities, never added to or subtracted from input or output).
type Usage struct {
	InputTokens      scan.Quantity  `json:"input_tokens"`
	OutputTokens     scan.Quantity  `json:"output_tokens"`
	CacheReadTokens  *scan.Quantity `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *scan.Quantity `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *scan.Quantity `json:"reasoning_tokens,omitempty"`
}

// Collection is the closure of one session by this collector (draft v0.2): it proves only the
// collector's observed scope under its source capabilities.
type Collection struct {
	// CollectionRef separates closures of one session (draft v0.2 collection_ref)
	CollectionRef string        `json:"collection_ref,omitempty"`
	Lifecycle     string        `json:"lifecycle"` // attached_at_start, attached_mid_session, unknown
	Closure       string        `json:"closure"`   // flushed, interrupted, unknown
	EventsDropped scan.Quantity `json:"events_dropped"`
	Errors        scan.Quantity `json:"errors"`
	OpenCalls     scan.Quantity `json:"open_calls"`
	ClosedAt      *string       `json:"closed_at"`
}

// Time is the time of an observation.
type Time struct {
	Start *string `json:"start"`
	End   *string `json:"end"`
	Clock string  `json:"clock"`
}

// Target is an entity that an operation acts on.
type Target struct {
	EntityID string `json:"entity_id"`
}

// Outcome is the result of an operation.
type Outcome struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Coordination holds multi-agent fields; empty when the source gives none.
type Coordination struct{}

// Provenance records capture time and transformations.
type Provenance struct {
	CapturedAt string `json:"captured_at"`
	// SourceEventID is the opaque identity of the source event (domain-separated, scoped to
	// the source and session); empty when the source gives none.
	SourceEventID   string      `json:"source_event_id,omitempty"`
	Transformations []Transform `json:"transformations"`
}

// Transform is one processing step.
type Transform struct {
	Step    string `json:"step"`
	Tool    string `json:"tool"`
	Version string `json:"version"`
}

// Source is an entry of the envelope `sources` list.
type Source struct {
	SourceID     string   `json:"source_id"`
	Agent        string   `json:"agent"`
	AgentVersion *string  `json:"agent_version"`
	Adapter      Adapter  `json:"adapter"`
	Capabilities []string `json:"capabilities"`
	Unavailable  []string `json:"unavailable"`
	// Configuration is the canonical effective configuration (hint-data v0.2 only).
	Configuration *Configuration `json:"configuration,omitempty"`
	// UsageNormalization is the allowlisted normalization of the source's token counters
	// (draft v0.2); empty when the source reports no usage.
	UsageNormalization string `json:"usage_normalization,omitempty"`
}

// Adapter names the adapter of a source.
type Adapter struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Timestamp formats a time as RFC 3339 UTC with milliseconds.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func ptr(s string) *string { return &s }

func defaultProvenance(now time.Time) Provenance {
	return Provenance{CapturedAt: Timestamp(now), Transformations: []Transform{
		{Step: "normalize", Tool: "hint-cli", Version: Version}, {Step: "redact", Tool: "hint-cli", Version: Version}}}
}
