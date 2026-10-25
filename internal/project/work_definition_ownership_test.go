package project

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
)

// These helper controls use the actual synthetic SDK enrollment and CLI decoder.
// They test received ownership only; no tuple grants source or usage authority.
func TestWorkDefinitionOwnerMissingTupleAndDuplicateDomainsRefuse(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	u, err := LoadWork(f.s, memLabel)
	if err != nil {
		t.Fatal(err)
	}
	owner := definitionRunOwner{agent: "opencode", configuration: f.cfg, epoch: e.Number(), binding: activity.LedgerBinding{RunRef: x.RunRef, UnitRef: u.UnitRef, AttemptRef: x.AttemptRef, TaskID: x.TaskID, TaskFamilyID: activity.FamilyRef(f.s.Key(), "family"), ConfigurationDigest: f.cfg.Digest(), Origin: "project", ExecutionRef: x.ExecutionRef}}
	owners := definitionRunOwners{}
	if err = owners.add(owner); err != nil {
		t.Fatal(err)
	}
	decoded := memBehavior(t, f, f.cfg, x.TaskID, owner.binding.TaskFamilyID, x.RunRef)
	sources := map[string]activity.Source{decoded.Source.SourceID: decoded.Source}
	if !owners.behavior(decoded.Observations[0], sources) {
		t.Fatal("exact SDK tuple refused")
	}
	for _, field := range []string{"run_ref", "task_id", "task_family_id", "configuration", "adapter"} {
		t.Run(field, func(t *testing.T) {
			raw, _ := json.Marshal(decoded.Observations[0])
			var observation activity.Observation
			_ = json.Unmarshal(raw, &observation)
			raw, _ = json.Marshal(decoded.Source)
			var source activity.Source
			_ = json.Unmarshal(raw, &source)
			switch field {
			case "configuration":
				source.Configuration = nil
			case "adapter":
				source.Adapter.Version = "unsupported"
			default:
				delete(observation.Correlation, field)
			}
			if owners.behavior(observation, map[string]activity.Source{source.SourceID: source}) {
				t.Fatal("missing tuple or unsupported source admitted")
			}
		})
	}
	if err = owners.add(owner); !errors.Is(err, ErrWorkDefinition) {
		t.Fatal("duplicate holder admitted", err)
	}
	controlled := owner
	controlled.binding.Origin, controlled.binding.ExecutionRef = "", ""
	controlled.binding.UnitRef = hint.EvalRef(f.s.Key(), "work-unit", "controlled", "another-run", "another-eval")
	controlled.epoch, controlled.ordinal = 0, 1
	if err = owners.add(controlled); !errors.Is(err, ErrWorkDefinition) {
		t.Fatal("cross-origin execution collision admitted", err)
	}
}

func TestWorkDefinitionPublishedMembershipMustMatchActualEpoch(t *testing.T) {
	f := membershipFixture(t)
	e := mustOpen(t, f)
	x := mustEnroll(t, f, e, "task-1", "run-1")
	if err := e.LaunchHook(f.s, x)(f.facts); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(f.s, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Seal(f.s, memNow); err != nil {
		t.Fatal(err)
	}
	u, err := LoadWork(f.s, memLabel)
	if err != nil {
		t.Fatal(err)
	}
	v, err := readEpoch(f.s, memLabel, e.Number())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.ReadStateBounded(context.Background(), maxMembershipBytes, membershipPath(f.s, memLabel, sealedName(e.Number()))...)
	if err != nil {
		t.Fatal(err)
	}
	var batch sealedBatch
	if definitionDecode(raw, &batch) != nil {
		t.Fatal("actual producer batch malformed")
	}
	expected := definitionMembershipOwners{{unit: u.UnitRef, epoch: e.Number()}: {body: spoolBody(u.UnitRef, v), source: orchestratorSource(f.s.Key(), u.UnitRef, e.Number())}}
	if !expected.matches(batch.Observations[0], batch.Source) {
		t.Fatal("exact actual epoch refused")
	}
	for _, kind := range []string{"foreign-body", "foreign-source", "missing-holder"} {
		t.Run(kind, func(t *testing.T) {
			data, _ := json.Marshal(batch)
			var changed sealedBatch
			_ = json.Unmarshal(data, &changed)
			switch kind {
			case "foreign-body":
				changed.Observations[0].Membership.HeadDigest = "sha256:" + strings.Repeat("0", 64)
			case "foreign-source":
				changed.Source.SourceID = strings.Repeat("0", 32)
			case "missing-holder":
				changed.Observations[0].Membership.UnitRef = strings.Repeat("0", 32)
			}
			if expected.matches(changed.Observations[0], changed.Source) {
				t.Fatal("foreign administrative claim admitted")
			}
		})
	}
}
