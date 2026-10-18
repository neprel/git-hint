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
	Missing             []string          `json:"missing"`
	Provenance          Provenance        `json:"provenance"`
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
	CapturedAt      string      `json:"captured_at"`
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
