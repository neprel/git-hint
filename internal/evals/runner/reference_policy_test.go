package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
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
		t.Fatal("missing declared source/module pins")
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

// A build-time source-package guard, never a runtime installed-code trust lookup.
// Enumerate all non-test Go files, including inactive platform variants. Only the
// catalogue is excluded here because the policy digest binds its source separately.
func referencePolicySourceSet(fsys fs.FS, sources []ReferencePolicySource) error {
	es, e := fs.ReadDir(fsys, ".")
	if e != nil {
		return e
	}
	actual := []string{}
	for _, v := range es {
		name := v.Name()
		if v.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "reference_policy_catalogue.go" {
			continue
		}
		if !v.Type().IsRegular() {
			return fmt.Errorf("nonregular runner Go source: %s", name)
		}
		actual = append(actual, name)
	}
	expected := make([]string, len(sources))
	for i, v := range sources {
		expected[i] = v.Path
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf("runner source set mismatch: actual=%v embedded=%v", actual, expected)
	}
	return nil
}
func TestReferencePolicyRunnerSourceSetMatchesEmbedded(t *testing.T) {
	_, sources, _, e := referenceBundleIdentity()
	if e != nil {
		t.Fatal(e)
	}
	if e = referencePolicySourceSet(os.DirFS("."), sources); e != nil {
		t.Fatal(e)
	}
}
func TestReferencePolicySourceSetRejectsUnlistedAndMissingPlatformHelper(t *testing.T) {
	_, sources, _, e := referenceBundleIdentity()
	if e != nil {
		t.Fatal(e)
	}
	fixture := fstest.MapFS{}
	for _, v := range sources {
		fixture[v.Path] = &fstest.MapFile{Data: []byte("package runner\n")}
	}
	fixture["reference_policy_catalogue.go"] = &fstest.MapFile{Data: []byte("package runner\n")}
	fixture["irrelevant_test.go"] = &fstest.MapFile{Data: []byte("package runner\n")}
	if e = referencePolicySourceSet(fixture, sources); e != nil {
		t.Fatal(e)
	}
	fixture["unlisted_future_helper.go"] = &fstest.MapFile{Data: []byte("package runner\nfunc futureProcedureHelper(){}\n")}
	if e = referencePolicySourceSet(fixture, sources); e == nil {
		t.Fatal("unlisted compiled helper passed")
	}
	delete(fixture, "unlisted_future_helper.go")
	delete(fixture, "reference_policy_other.go")
	if e = referencePolicySourceSet(fixture, sources); e == nil {
		t.Fatal("inactive platform source omitted")
	}
}
