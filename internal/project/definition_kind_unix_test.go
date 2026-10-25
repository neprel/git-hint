//go:build unix

package project

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/hint"
)

func TestDefinitionKindsMutuallyExclusiveActualProducer(t *testing.T) {
	for _, first := range []string{WorkDefinitionInputsVersion, WorkDefinitionInputsV2} {
		t.Run(first, func(t *testing.T) {
			f, b := baselineFixture(t)
			f = baselinePlan(t, f, b)
			v2req := f.request
			v1req := f.request
			v1req.Baseline = nil
			firstReq, nextReq := v1req, v2req
			if first == WorkDefinitionInputsV2 {
				firstReq, nextReq = v2req, v1req
			}
			original, e := WriteWorkDefinitionInputs(context.Background(), f.s, firstReq, time.Now())
			if e != nil {
				t.Fatal("first genuine producer", e)
			}
			before := projectionStateFiles(t, f.s.Path("state"))
			for i := 0; i < 2; i++ {
				if ref, e := WriteWorkDefinitionInputs(context.Background(), f.s, nextReq, time.Now()); e == nil || ref != (WorkDefinitionRef{}) {
					t.Fatal("same-unit alternate kind admitted", first, ref, e)
				}
			}
			if after := projectionStateFiles(t, f.s.Path("state")); !reflect.DeepEqual(before, after) {
				t.Fatal("alternate-kind refusal wrote state")
			}
			replay, e := WriteWorkDefinitionInputs(context.Background(), f.s, firstReq, time.Now())
			if e != nil || replay != original {
				t.Fatal("same-kind unchanged replay", e)
			}
			if after := projectionStateFiles(t, f.s.Path("state")); !reflect.DeepEqual(before, after) {
				t.Fatal("unchanged replay wrote state")
			}
			if first == WorkDefinitionInputsV2 {
				if e = os.Chmod(filepath.Join(f.s.Root, "main.go"), 0755); e != nil {
					t.Fatal(e)
				}
				if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, CaptureSourceRequest{Label: f.request.Label, Definition: original}, time.Now()); e == nil || a != nil {
					t.Fatal("mode drift admitted through v2", e)
				}
				// Downgrade remains refused after drift. There is no v1 reference to capture.
				before = projectionStateFiles(t, f.s.Path("state"))
				if ref, e := WriteWorkDefinitionInputs(context.Background(), f.s, v1req, time.Now()); e == nil || ref != (WorkDefinitionRef{}) {
					t.Fatal("mode drift regained acceptance via v1")
				}
				if !reflect.DeepEqual(before, projectionStateFiles(t, f.s.Path("state"))) {
					t.Fatal("downgrade allocated state")
				}
			}
		})
	}
}

func TestDefinitionCounterpartMalformedFailsClosedWithoutWrites(t *testing.T) {
	for _, bad := range []string{"malformed", "directory", "oversized"} {
		t.Run(bad, func(t *testing.T) {
			f, b := baselineFixture(t)
			f = baselinePlan(t, f, b)
			original := writeDefinition(t, f)
			unit := hint.EvalRef(f.s.Key(), "work-unit", f.request.Label)
			other := hint.EvalRef(f.s.Key(), WorkDefinitionInputsVersion, unit)
			path := f.s.Path("state", library.WorkDefinitionStateDir, other+".json")
			switch bad {
			case "malformed":
				if e := f.s.WriteState([]byte("{"), "state", library.WorkDefinitionStateDir, other+".json"); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Mkdir(path, 0700); e != nil {
					t.Fatal(e)
				}
			case "oversized":
				if e := f.s.WriteState([]byte(strings.Repeat("x", int(WorkDefinitionRecordMax)+1)), "state", library.WorkDefinitionStateDir, other+".json"); e != nil {
					t.Fatal(e)
				}
			}
			before := projectionStateFiles(t, f.s.Path("state"))
			if _, e := WriteWorkDefinitionInputs(context.Background(), f.s, f.request, time.Now()); e == nil {
				t.Fatal("malformed counterpart replay")
			}
			if _, e := LoadWorkDefinitionInputs(context.Background(), f.s, original); e == nil {
				t.Fatal("malformed counterpart reload")
			}
			if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, CaptureSourceRequest{Label: f.request.Label, Definition: original}, time.Now()); e == nil || a != nil {
				t.Fatal("malformed counterpart capture")
			}
			if !reflect.DeepEqual(before, projectionStateFiles(t, f.s.Path("state"))) {
				t.Fatal("malformed counterpart refusal wrote state")
			}
		})
	}
}

func TestDefinitionRejectedC308CoexistenceCannotRegainSourceAcceptance(t *testing.T) {
	f, b := baselineFixture(t)
	f = baselinePlan(t, f, b)
	v2 := writeDefinition(t, f)
	h, e := LoadWorkDefinitionInputs(context.Background(), f.s, v2)
	if e != nil {
		t.Fatal(e)
	}
	// Reconstruct only the already-persisted rejected c308 shape. The successor
	// producer cannot create this second kind. Every loader/capture must refuse it.
	legacy := h.Identity()
	legacy.Kind = WorkDefinitionInputsVersion
	legacy.Baseline = nil
	legacy.RecordRef = hint.EvalRef(f.s.Key(), legacy.Kind, legacy.UnitRef)
	legacy.DefinitionBindingDigest, e = definitionBinding(legacy)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := hint.Canonical(legacy)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.WriteStateImmutableContext(context.Background(), raw, WorkDefinitionRecordMax, "state", library.WorkDefinitionStateDir, legacy.RecordRef+".json"); e != nil {
		t.Fatal(e)
	}
	v1 := WorkDefinitionRef{legacy.RecordRef, library.Digest(raw)}
	if e = os.Chmod(filepath.Join(f.s.Root, "main.go"), 0755); e != nil {
		t.Fatal(e)
	}
	before := projectionStateFiles(t, f.s.Path("state"))
	for _, ref := range []WorkDefinitionRef{v1, v2} {
		if _, e := LoadWorkDefinitionInputs(context.Background(), f.s, ref); e == nil {
			t.Fatal("coexisting kind reloaded")
		}
		if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, CaptureSourceRequest{Label: f.request.Label, Definition: ref}, time.Now()); e == nil || a != nil {
			t.Fatal("coexisting kind issued source acceptance")
		}
	}
	if !reflect.DeepEqual(before, projectionStateFiles(t, f.s.Path("state"))) {
		t.Fatal("coexistence refusal wrote state")
	}
}
