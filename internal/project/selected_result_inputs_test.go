package project

import (
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/store"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectedResultMissingCustodyNoStoreUse(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, e := ResolveSelectedResultInputs(ctx, &store.Store{}, SelectedResultRequest{Result: &runner.TaskResultArtifact{}}); e == nil {
			t.Fatal("forged result")
		}
	}
}

// The actual exporter proves fixed-role derived identities; no engine/policy is
// involved. This is also the finite mutation oracle for environment equality.
func TestSelectedResultReferenceDerivedEnvironmentReplay(t *testing.T) {
	f := definitionFixtureNew(t)
	raw, e := f.l.ResolveBoundedRead(context.Background(), "eval", f.request.QualifiedIO.EvalName, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	var rec map[string]any
	if json.Unmarshal(raw, &rec) != nil {
		t.Fatal("manifest")
	}
	task, e := runner.ExportBounded(context.Background(), f.l, rec, filepath.Join(t.TempDir(), "task"), referenceExportSettings())
	if e != nil {
		t.Fatal(e)
	}
	frozen, e := task.FrozenIOInputs()
	if e != nil {
		t.Fatal(e)
	}
	v := referenceRequest{Inputs: frozen.PreparationIdentity(), ExportedTask: frozen.Identity(), ExportInputs: task.Inputs}
	if e := verifyReferenceExportedTask(context.Background(), f.s, rec, v); e != nil {
		t.Fatal("genuine role export", e)
	}
	v.Inputs.EnvironmentDigest = "sha256:" + strings.Repeat("a", 64)
	if e := verifyReferenceExportedTask(context.Background(), f.s, rec, v); e == nil {
		t.Fatal("changed derived environment admitted")
	}
}
