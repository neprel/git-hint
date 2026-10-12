package activity

import (
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// WorkspaceSource returns the source entry of the workspace diff: the file changes that
// `git hint collect` finds between two snapshots. It sees actual changes only, never reads
// or process identity.
func WorkspaceSource(key []byte) Source {
	return Source{SourceID: hint.DeriveID(key, "source", "workspace-diff"), Agent: "none",
		Adapter:      Adapter{Name: "watcher", Version: Version},
		Capabilities: []string{"time.end", "targets", "mutation_attribution"},
		Unavailable: []string{"time.start", "correlation.task_id", "correlation.session_id", "correlation.turn_id",
			"correlation.tool_call_id", "outcome.exit_code", "size", "agent_version"}}
}

// FileChange is one file that differs between two snapshots.
type FileChange struct {
	EntityID string // file ID (the ID after the change; the old ID for a deletion)
}

// Mutations returns one FILE_MUTATION observation per changed file in (since, until].
//
// Attribution rule: a mutation is "inferred" and linked to a tool call only when exactly
// one edit or shell write observation of the window targets that file. Otherwise it stays
// "none": a time coincidence alone never assigns an owner.
func Mutations(key []byte, beforeSnap, afterSnap string, changes []FileChange, since, until time.Time, recent []Observation) []Observation {
	src := WorkspaceSource(key)
	end := Timestamp(until)
	var out []Observation
	for _, c := range changes {
		o := Observation{
			ObservationID: hint.DeriveID(key, "event", src.SourceID, beforeSnap, afterSnap, c.EntityID),
			Class:         "observation", Op: "FILE_MUTATION", Source: "watcher", SourceID: src.SourceID,
			Precision: "exact", Time: Time{Start: nil, End: &end, Clock: "collector"},
			Targets: []Target{{EntityID: c.EntityID}}, Outcome: Outcome{Status: "success"},
			MutationAttribution: "none",
			Missing: []string{"correlation.session_id", "correlation.task_id", "correlation.tool_call_id",
				"correlation.turn_id", "time.start"},
			Provenance: defaultProvenance(until),
		}
		var match []Observation
		for _, r := range recent {
			if !editLike(r) || !inWindow(r, since, until) {
				continue
			}
			for _, t := range r.Targets {
				if t.EntityID == c.EntityID {
					match = append(match, r)
					break
				}
			}
		}
		if len(match) == 1 {
			m := match[0]
			o.MutationAttribution = "inferred"
			o.Correlation = map[string]string{"parent_observation_id": m.ObservationID}
			for _, k := range []string{"session_id", "tool_call_id"} {
				if v := m.Correlation[k]; v != "" {
					o.Correlation[k] = v
				}
			}
			o.Missing = []string{"correlation.task_id", "correlation.turn_id", "time.start"}
		}
		out = append(out, o)
	}
	return out
}

// editLike: an edit tool call, or a shell command whose classification includes a write.
// A mixed shell command lists read and write targets together; it can only make the
// match ambiguous (then "none"), never more certain.
func editLike(o Observation) bool {
	if o.Op == "EDIT_INTENT" {
		return true
	}
	for _, s := range o.OpsSecondary {
		if s == "EDIT_INTENT" {
			return true
		}
	}
	return false
}

func inWindow(o Observation, since, until time.Time) bool {
	ts := o.Time.End
	if ts == nil {
		ts = o.Time.Start
	}
	if ts == nil {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, *ts)
	return err == nil && t.After(since) && !t.After(until)
}
