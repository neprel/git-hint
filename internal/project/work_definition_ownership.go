package project

// Received ownership only. These tuples never certify launch, usage, outcome,
// completion, capture policy, source origin or complete cost population.
import (
	"encoding/json"

	"github.com/neprel/git-hint/internal/activity"
)

type definitionRunOwner struct {
	binding        activity.LedgerBinding
	configuration  activity.Configuration
	agent          string
	epoch, ordinal int // disjoint project enrollment / controlled schedule domains
}

type definitionRunOwners map[string]definitionRunOwner

func (owners definitionRunOwners) add(owner definitionRunOwner) error {
	b := owner.binding
	if owner.agent == "" || owner.configuration.Agent.Name != owner.agent {
		return ErrWorkDefinition
	}
	switch b.Origin {
	case "":
		if b.ExecutionRef != "" || owner.epoch != 0 || owner.ordinal < 1 {
			return ErrWorkDefinition
		}
	case "project":
		if !batchID.MatchString(b.ExecutionRef) || owner.epoch < 1 || owner.ordinal != 0 {
			return ErrWorkDefinition
		}
	default:
		return ErrWorkDefinition
	}
	if !batchID.MatchString(b.RunRef) || !batchID.MatchString(b.UnitRef) || !batchID.MatchString(b.AttemptRef) || !batchID.MatchString(b.TaskID) || !batchID.MatchString(b.TaskFamilyID) || b.ConfigurationDigest != owner.configuration.Digest() {
		return ErrWorkDefinition
	}
	if _, exists := owners[b.RunRef]; exists {
		return ErrWorkDefinition
	}
	owners[b.RunRef] = owner
	return nil
}

func definitionOwnerConfiguration(value any) (activity.Configuration, error) {
	var cfg activity.Configuration
	raw, e := json.Marshal(value)
	if e != nil || definitionDecode(raw, &cfg) != nil || !definitionConfigurationValid(cfg) {
		return cfg, ErrWorkDefinition
	}
	return cfg, nil
}

// The existing shared root-CLI classifier checks the source and observation rules.
// Its evidence must resolve exactly one received execution; a task match is insufficient.
func (owners definitionRunOwners) behavior(o activity.Observation, sources map[string]activity.Source) bool {
	evidence := cliRootEvidence(o, sources)
	if evidence == nil {
		return false
	}
	owner, ok := owners[evidence.run]
	if !ok {
		return false
	}
	want := behaviorEvidence{configuration: owner.binding.ConfigurationDigest, task: owner.binding.TaskID, family: owner.binding.TaskFamilyID, run: owner.binding.RunRef}
	source := sources[o.SourceID]
	return *evidence == want && source.Agent == owner.agent && source.Configuration != nil && equalJSON(*source.Configuration, owner.configuration)
}

func (owners definitionRunOwners) ledger(o activity.Observation, source activity.Source, key []byte) bool {
	if o.ModelLedger == nil {
		return false
	}
	b := o.ModelLedger.Binding
	owner, ok := owners[b.RunRef]
	if !ok || b != owner.binding {
		return false
	}
	if o.Correlation["run_ref"] != b.RunRef || o.Correlation["task_id"] != b.TaskID || o.Correlation["task_family_id"] != b.TaskFamilyID {
		return false
	}
	// Use the existing producer declaration, not a second source recipe.
	wantSource := activity.LedgerSource(key, owner.agent, owner.configuration)
	return equalJSON(source, wantSource)
}

// A membership enrollment belongs to the actual received open revision that
// contains its attempt/task. A sealed epoch uses its exact frozen-open anchor.
func definitionProjectAnchor(u *WorkUnit, v *epochView) (map[string]any, error) {
	start := -1
	for i, rev := range u.Revisions {
		if rev["revision_digest"] == v.open["enrollment_start_revision_digest"] && rev["state"] == "open" {
			start = i
		}
	}
	if start < 0 {
		return nil, ErrWorkDefinition
	}
	if v.sealed {
		frozen := v.events[len(v.events)-1].ev["frozen_open_revision_digest"]
		for i, rev := range u.Revisions {
			if i >= start && rev["revision_digest"] == frozen && rev["state"] == "open" {
				return rev, nil
			}
		}
		return nil, ErrWorkDefinition
	}
	if u.State != "open" || u.Revisions[len(u.Revisions)-1]["state"] != "open" {
		return nil, ErrWorkDefinition
	}
	return u.Revisions[len(u.Revisions)-1], nil
}

func definitionProjectMember(anchor map[string]any, attempt, task string) bool {
	attempts, ok := anchor["attempts"].([]any)
	if !ok {
		return false
	}
	for _, entry := range attempts {
		a, ok := entry.(map[string]any)
		if !ok || a["attempt_ref"] != attempt || a["outcome"] != "open" {
			continue
		}
		ids, ok := a["task_ids"].([]any)
		if !ok {
			return false
		}
		for _, id := range ids {
			if id == task {
				return true
			}
		}
	}
	return false
}

// A published administrative claim must be the same received sealed epoch, not
// another holder that merely names a known project unit.
type definitionMembershipKey struct {
	unit  string
	epoch int
}
type definitionMembershipOwner struct {
	body   map[string]any
	source activity.Source
}
type definitionMembershipOwners map[definitionMembershipKey]definitionMembershipOwner

func (owners definitionMembershipOwners) matches(o activity.Observation, source activity.Source) bool {
	m := o.Membership
	if m == nil || len(m.Events) == 0 {
		return false
	}
	n, ok := evInt(plainJSON(m.Events[0]), "epoch")
	if !ok {
		return false
	}
	owner, ok := owners[definitionMembershipKey{m.UnitRef, n}]
	return ok && equalJSON(owner.body, membershipBody(m)) && equalJSON(source, owner.source)
}
