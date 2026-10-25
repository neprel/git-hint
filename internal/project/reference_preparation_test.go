package project

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReferenceProjectDefaultDisabledBeforeState(t *testing.T) {
	f := definitionFixtureNew(t)
	ref := writeDefinition(t, f)
	a, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{ref, f.request.Label, "attempt-one", runner.ReferencePreparationPolicyRef{PolicyID: "missing.policy", PolicyDigest: "sha256:" + strings.Repeat("a", 64)}, nil}, f.now)
	if e == nil || a != nil {
		t.Fatal(a, e)
	}
	if _, e = os.Lstat(f.s.Path("state", library.ReferencePreparationStateDir)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("disabled path allocated reservation", e)
	}
}
func TestReferenceProjectInvalidRequests(t *testing.T) {
	for _, parent := range []context.Context{nil, context.Background()} {
		if _, e := PrepareFinalReference(parent, nil, PrepareReferenceRequest{}, time.Now()); e == nil {
			t.Fatal("nil request")
		}
	}
	f := definitionFixtureNew(t)
	ref := writeDefinition(t, f)
	for _, op := range []string{"", strings.Repeat("a", 129), "line\nbreak"} {
		if a, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{Definition: ref, Label: f.request.Label, OperationID: op}, f.now); e == nil || a != nil {
			t.Fatal(op, a, e)
		}
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if _, e := PrepareFinalReference(ctx, f.s, PrepareReferenceRequest{Definition: ref, Label: f.request.Label, OperationID: "one"}, f.now); e == nil || !strings.Contains(e.Error(), "budget") {
		t.Fatal(e)
	}
}
func TestReferenceRecordStrictLocatorAndCanonical(t *testing.T) {
	f := definitionFixtureNew(t)
	u := hint.EvalRef(f.s.Key(), "unit", "synthetic")
	o := hint.EvalRef(f.s.Key(), "operation", "synthetic")
	base := []string{"state", library.ReferencePreparationStateDir, u, o}
	v := referenceReservation{Version: ReferencePreparationVersion, State: "reserved", Authority: "unknown"}
	raw := referenceEncoded(base, "reservation", v)
	var got referenceReservation
	if e := decodeReferenceRecord(raw, base, "reservation", &got); e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{[]byte(`null`), []byte(`{}`), append(append([]byte{}, raw...), byte(' ')), []byte(strings.Replace(string(raw), "reserved", "changed", 1))} {
		var r referenceReservation
		e := decodeReferenceRecord(bad, base, "request", &r)
		if e == nil {
			t.Fatal("bad locator/kind accepted")
		}
	}
	ref := referenceRecordRef(base, "reservation", raw)
	if _, e := referenceLocation(ref, "reservation"); e != nil {
		t.Fatal(e)
	}
	ref.RecordRef = "../" + ref.RecordRef
	if _, e := referenceLocation(ref, "reservation"); e == nil {
		t.Fatal("traversal")
	}
}
func TestReferenceImmutableCommitAndCanceledWrite(t *testing.T) {
	f := definitionFixtureNew(t)
	base := []string{"state", library.ReferencePreparationStateDir, hint.EvalRef(f.s.Key(), "unit", "u"), hint.EvalRef(f.s.Key(), "operation", "o")}
	v := referenceReservation{Version: ReferencePreparationVersion, State: "reserved", Authority: "unknown"}
	ref, e := writeReferenceRecord(context.Background(), f.s, base, "reservation", v)
	if e != nil {
		t.Fatal(e)
	}
	v.State = "changed"
	if _, e = writeReferenceRecord(context.Background(), f.s, base, "reservation", v); e == nil {
		t.Fatal("overwrote reservation")
	}
	raw, e := f.s.ReadStateBounded(context.Background(), referenceMetadataMax, append(base, "reservation.json")...)
	if e != nil || library.Digest(raw) != ref.RecordDigest {
		t.Fatal("immutable changed", e)
	}
	ctx, c := context.WithCancel(context.Background())
	c()
	if r, e := writeReferenceRecord(ctx, f.s, base, "outcome", v); e == nil || r.RecordRef != "" {
		t.Fatal("canceled commit", r, e)
	}
	if _, e = os.Lstat(f.s.Path(append(base, "outcome.json")...)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("provisional outcome", e)
	}
}

func TestReferenceAcceptanceCannotDeserialize(t *testing.T) {
	var witness ReferencePreparationAcceptance
	if _, e := json.Marshal(witness); e == nil {
		t.Fatal("serializable witness")
	}
	if e := json.Unmarshal([]byte(`{}`), &witness); e == nil {
		t.Fatal("deserializable witness")
	}
	if referenceAcceptanceMatches(&witness, ReferencePreparationRef{}, ReferencePreparationRef{}, ReferencePreparationRef{}, referenceRequest{}) {
		t.Fatal("zero witness accepted")
	}
	foreign := ReferencePreparationAcceptance{issuer: &referenceAcceptanceIssuer{1}}
	if referenceAcceptanceMatches(&foreign, ReferencePreparationRef{}, ReferencePreparationRef{}, ReferencePreparationRef{}, referenceRequest{}) {
		t.Fatal("foreign issuer accepted")
	}
}
