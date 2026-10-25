package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReferencePolicyDefaultAndNilContexts(t *testing.T) {
	ref := ReferencePreparationPolicyRef{"not-installed", "sha256:" + strings.Repeat("a", 64)}
	if len(referencePreparationPolicies) != 0 {
		t.Fatal("production catalogue is not empty")
	}
	if _, e := ResolveReferencePreparationPolicy(ref); !errors.Is(e, ErrReferencePreparationPolicy) {
		t.Fatal(e)
	}
	if _, e := OpenReferencePreparationEngine(nil, ref); !errors.Is(e, ErrReferencePreparationPolicy) {
		t.Fatal(e)
	}
	if _, e := OpenReferencePreparationEngine(context.Background(), ref); e == nil || strings.Contains(e.Error(), "/") {
		t.Fatal("default dial or raw path", e)
	}
	if _, e := (*Docker)(nil).PrepareReferenceForPolicy(nil, "not-read", ReferencePolicyPrepareRequest{Policy: ref}); !errors.Is(e, ErrReferencePreparationPolicy) {
		t.Fatal(e)
	}
	if _, e := InspectReferencePreparationInputs(nil, "not-read"); !errors.Is(e, ErrReferencePreparationPolicy) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := InspectReferencePreparationInputs(ctx, "not-read"); e == nil {
		t.Fatal("cancelled inspector")
	}
}
func TestReferencePolicyDependencyPinsMatchReviewedModule(t *testing.T) {
	_, sources, deps, e := referenceBundleIdentity()
	if e != nil {
		t.Fatal(e)
	}
	if len(sources) < 10 || len(deps) != 4 {
		t.Fatal("missing source closure")
	}
	for _, v := range deps {
		b, e := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(v.Path)))
		if e != nil {
			t.Fatal(e)
		}
		if int64(len(b)) != v.Bytes || referenceDigest(b) != v.Digest {
			t.Fatalf("stale reviewed dependency pin: %s", v.Path)
		}
	}
	for _, v := range sources {
		if strings.HasSuffix(v.Path, "_test.go") {
			t.Fatal("test source embedded in bundle")
		}
	}
}
func TestReferencePolicyInspectorIsSameIdentityAlgorithm(t *testing.T) {
	task, o := referenceTaskOptions(t)
	got, e := InspectReferencePreparationInputs(context.Background(), task)
	if e != nil || got != o.Expected {
		t.Fatal(got, e)
	}
}
