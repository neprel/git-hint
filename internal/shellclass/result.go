// Package shellclass classifies a shell command string into effect labels from syntax
// only. It never executes anything.
//
// The output is an observation hint. It must never authorize a permission,
// a sandbox decision or a destructive action.
package shellclass

import (
	"slices"
	"sort"
)

// Labels. READ and WRITE are not exclusive.
const (
	READ    = "READ"
	WRITE   = "WRITE"
	SEARCH  = "SEARCH"
	LIST    = "LIST"
	EXECUTE = "EXECUTE"
	TEST    = "TEST"
	BUILD   = "BUILD"
	VCS     = "VCS"
	NETWORK = "NETWORK"
	DELETE  = "DELETE"
	UNKNOWN = "UNKNOWN"
	OTHER   = "OTHER"
	// MIXED is a summary operation, not a label. Op is MIXED when the
	// command has more than one label.
	MIXED = "MIXED"
)

// Result is the classification of one command string.
type Result struct {
	Op           string   `json:"op"`
	Labels       []string `json:"labels"`
	ReadTargets  []string `json:"read_targets"`
	WriteTargets []string `json:"write_targets"`
	// DynamicTargets is true when at least one target depends on a
	// variable, a command substitution, stdin (xargs) or a find/{} slot.
	DynamicTargets bool `json:"dynamic_targets"`
	// Reasons explains every UNKNOWN label.
	Reasons []string `json:"reasons,omitempty"`
}

type acc struct {
	labels  map[string]bool
	reads   []string
	writes  []string
	dynamic bool
	reasons []string
}

func newAcc() *acc { return &acc{labels: map[string]bool{}} }

func (a *acc) add(ls ...string) {
	for _, l := range ls {
		a.labels[l] = true
	}
}

func (a *acc) unknown(reason string) {
	a.labels[UNKNOWN] = true
	a.reasons = append(a.reasons, reason)
}

func (a *acc) read(ts ...string)  { a.reads = append(a.reads, ts...) }
func (a *acc) write(ts ...string) { a.writes = append(a.writes, ts...) }

func dedupe(in []string) []string {
	out := []string{}
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func (a *acc) result() Result {
	ls := []string{}
	for l := range a.labels {
		ls = append(ls, l)
	}
	if len(ls) > 1 {
		ls = slices.DeleteFunc(ls, func(s string) bool { return s == OTHER })
	}
	if len(ls) == 0 {
		ls = []string{OTHER}
	}
	sort.Strings(ls)
	op := ls[0]
	if len(ls) > 1 {
		op = MIXED
	}
	return Result{
		Op:             op,
		Labels:         ls,
		ReadTargets:    dedupe(a.reads),
		WriteTargets:   dedupe(a.writes),
		DynamicTargets: a.dynamic,
		Reasons:        dedupe(a.reasons),
	}
}
