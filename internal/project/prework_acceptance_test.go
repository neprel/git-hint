package project

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"unsafe"

	"github.com/neprel/git-hint/internal/evals/runner"
)

func TestPreworkIssuerKindsAndImmutableCopies(t *testing.T) {
	if preworkReferencePreparation == 0 || preworkSelectedSource == 0 || preworkOriginalStart == 0 || preworkReferencePreparation == preworkSelectedSource || preworkReferencePreparation == preworkOriginalStart || preworkSelectedSource == preworkOriginalStart {
		t.Fatal("closed checkpoint kind collision")
	}
	if unsafe.Sizeof(preworkIssuer{}) == 0 {
		t.Fatal("zero-sized issuer")
	}
	for _, kind := range []preworkKind{preworkReferencePreparation, preworkSelectedSource, preworkBaseline} {
		original := mintPreworkAcceptance(kind)
		copy := original
		if !matchPreworkAcceptance(original, kind) {
			t.Fatal("ready checkpoint refused")
		}
		for _, other := range []preworkKind{preworkReferencePreparation, preworkSelectedSource, preworkBaseline} {
			if matchPreworkAcceptance(original, other) != (kind == other) {
				t.Fatal("cross-kind checkpoint")
			}
		}
		copy.kind = 0
		if !matchPreworkAcceptance(original, kind) || matchPreworkAcceptance(copy, kind) {
			t.Fatal("mutable copy")
		}
		foreign := original
		foreign.issuer = &preworkIssuer{1}
		if matchPreworkAcceptance(foreign, kind) {
			t.Fatal("foreign issuer")
		}
	}
	for _, kind := range []preworkKind{0, preworkOriginalStart, 255} {
		a := mintPreworkAcceptance(kind)
		if a != (preworkAcceptance{}) || matchPreworkAcceptance(a, kind) || matchPreworkAcceptance(a, preworkReferencePreparation) {
			t.Fatal("unknown kind minted", kind)
		}
	}
	if matchPreworkAcceptance(preworkAcceptance{}, preworkReferencePreparation) {
		t.Fatal("zero accepted")
	}
}
func TestPreworkReferenceWrapperRetainsEveryTypedBinding(t *testing.T) {
	out := ReferencePreparationRef{"outcome", "outcome-digest"}
	request := ReferencePreparationRef{"request", "request-digest"}
	reservation := ReferencePreparationRef{"reservation", "reservation-digest"}
	v := referenceRequest{}
	v.Definition.StoreBinding = "actual-store"
	v.Policy.Policy = runner.ReferencePreparationPolicyRef{PolicyID: "actual-policy", PolicyDigest: "actual-policy-digest"}
	original := ReferencePreparationAcceptance{mintPreworkAcceptance(preworkReferencePreparation), out, request, reservation, v.Definition.StoreBinding, referenceBridgeIdentity(), v.Policy.Policy}
	if !referenceAcceptanceMatches(&original, out, request, reservation, v) {
		t.Fatal("ready typed binding")
	}
	changes := []func(*ReferencePreparationAcceptance){func(a *ReferencePreparationAcceptance) { a.outcome.RecordRef += "changed" }, func(a *ReferencePreparationAcceptance) { a.outcome.RecordDigest += "changed" }, func(a *ReferencePreparationAcceptance) { a.request.RecordRef += "changed" }, func(a *ReferencePreparationAcceptance) { a.request.RecordDigest += "changed" }, func(a *ReferencePreparationAcceptance) { a.reservation.RecordRef += "changed" }, func(a *ReferencePreparationAcceptance) { a.reservation.RecordDigest += "changed" }, func(a *ReferencePreparationAcceptance) { a.storeBinding += "changed" }, func(a *ReferencePreparationAcceptance) { a.writer += "changed" }, func(a *ReferencePreparationAcceptance) { a.policy.PolicyID += "changed" }, func(a *ReferencePreparationAcceptance) { a.policy.PolicyDigest += "changed" }, func(a *ReferencePreparationAcceptance) { a.checkpoint = mintPreworkAcceptance(preworkSelectedSource) }, func(a *ReferencePreparationAcceptance) { a.checkpoint = mintPreworkAcceptance(preworkOriginalStart) }}
	for i, change := range changes {
		copy := original
		change(&copy)
		if referenceAcceptanceMatches(&copy, out, request, reservation, v) {
			t.Fatal("lost exact typed binding", i)
		}
	}
}

// A build-time guard of the declared owned source families, not a runtime trust
// lookup and not a claim about the whole transitive compiled project closure.
func TestPreworkDeclaredSourceSetAndVersionIdentity(t *testing.T) {
	if preworkAcceptanceVersion != "project-prework-acceptance-v1" {
		t.Fatal("unreviewed checkpoint version")
	}
	actual, e := referenceBridgeSourceNames(os.DirFS("."))
	if e != nil {
		t.Fatal(e)
	}
	embedded, e := referenceBridgeSourceNames(referencePreparationSources)
	if e != nil || !reflect.DeepEqual(actual, embedded) {
		t.Fatal("declared source omission", actual, embedded, e)
	}
	fixture := fstest.MapFS{}
	for _, name := range embedded {
		raw, e := fs.ReadFile(referencePreparationSources, name)
		if e != nil {
			t.Fatal(e)
		}
		fixture[name] = &fstest.MapFile{Data: raw}
	}
	original := referenceBridgeIdentityFrom(fixture)
	if original == "" || original != referenceBridgeIdentity() {
		t.Fatal("identity source disagreement")
	}
	helper := fixture["prework_acceptance.go"].Data
	fixture["prework_acceptance.go"] = &fstest.MapFile{Data: append(append([]byte{}, helper...), []byte("\n// changed helper bytes\n")...)}
	if referenceBridgeIdentityFrom(fixture) == original {
		t.Fatal("helper bytes missing from identity")
	}
	delete(fixture, "prework_acceptance.go")
	if referenceBridgeIdentityFrom(fixture) == original {
		t.Fatal("missing source membership did not change identity")
	}
	missing, _ := referenceBridgeSourceNames(fixture)
	if reflect.DeepEqual(missing, actual) {
		t.Fatal("omitted helper escaped source-set guard")
	}
	fixture["prework_acceptance.go"] = &fstest.MapFile{Data: helper}
	fixture["prework_acceptance_unlisted.go"] = &fstest.MapFile{Data: []byte("package project\n")}
	added, _ := referenceBridgeSourceNames(fixture)
	if reflect.DeepEqual(added, embedded) {
		t.Fatal("unlisted compiled helper escaped source-set guard")
	}
}
func TestPreworkOnlyReviewedProductionMint(t *testing.T) {
	entries, e := os.ReadDir(".")
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	seen := map[string]int{}
	reviewed := map[string]string{"reference_preparation.go": "preworkReferencePreparation", "capture_source.go": "preworkSelectedSource", "baseline_collect.go": "preworkBaseline"}
	for _, entry := range entries {
		n := entry.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, e := parser.ParseFile(token.NewFileSet(), n, nil, 0)
		if e != nil {
			t.Fatal(e)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "mintPreworkAcceptance" {
				return true
			}
			count++
			expected, reviewedFile := reviewed[n]
			seen[n]++
			if !reviewedFile || len(call.Args) != 1 || seen[n] != 1 {
				t.Error("unreviewed production checkpoint callsite", n)
				return true
			}
			kind, ok := call.Args[0].(*ast.Ident)
			if !ok || kind.Name != expected {
				t.Error("unreviewed checkpoint kind activated", n)
			}
			return true
		})
	}
	if count != 3 || seen["baseline_collect.go"] != 1 || seen["reference_preparation.go"] != 1 || seen["capture_source.go"] != 1 {
		t.Fatal("production mint callsite count", count, seen)
	}
}

func TestPreworkNeutralCoreShapeRemainsClosed(t *testing.T) {
	typ := reflect.TypeOf(preworkAcceptance{})
	if typ.NumField() != 2 || typ.Field(0).Name != "issuer" || typ.Field(0).Type != reflect.TypeOf((*preworkIssuer)(nil)) || typ.Field(1).Name != "kind" || typ.Field(1).Type != reflect.TypeOf(preworkKind(0)) {
		t.Fatal("neutral core acquired binding/authority fields")
	}
	wrapper := reflect.TypeOf(ReferencePreparationAcceptance{})
	expected := []struct {
		name string
		typ  reflect.Type
	}{{"checkpoint", typ}, {"outcome", reflect.TypeOf(ReferencePreparationRef{})}, {"request", reflect.TypeOf(ReferencePreparationRef{})}, {"reservation", reflect.TypeOf(ReferencePreparationRef{})}, {"storeBinding", reflect.TypeOf("")}, {"writer", reflect.TypeOf("")}, {"policy", reflect.TypeOf(runner.ReferencePreparationPolicyRef{})}}
	if wrapper.NumField() != len(expected) {
		t.Fatal("reference wrapper shape changed")
	}
	for i, want := range expected {
		actual := wrapper.Field(i)
		if actual.Name != want.name || actual.Type != want.typ || actual.IsExported() {
			t.Fatal("typed immutable binding drift", actual.Name)
		}
	}
}

// PW-1: adding a file to BOTH the directory family and embedded family cannot
// leave its bytes outside the identity. Both views use the ONE production recipe.
func TestPreworkAddedEmbeddedFamilyBytesAndNamesBindIdentity(t *testing.T) {
	names, e := referenceBridgeSourceNames(referencePreparationSources)
	if e != nil {
		t.Fatal(e)
	}
	directory := fstest.MapFS{}
	embedded := fstest.MapFS{}
	for _, name := range names {
		raw, e := fs.ReadFile(referencePreparationSources, name)
		if e != nil {
			t.Fatal(e)
		}
		directory[name] = &fstest.MapFile{Data: raw}
		embedded[name] = &fstest.MapFile{Data: append([]byte(nil), raw...)}
	}
	base := referenceBridgeIdentityFrom(embedded)
	const extra = "reference_preparation_extra.go"
	directory[extra] = &fstest.MapFile{Data: []byte("package project\nconst extraRule=1\n")}
	embedded[extra] = &fstest.MapFile{Data: append([]byte(nil), directory[extra].Data...)}
	dirNames, e := referenceBridgeSourceNames(directory)
	if e != nil {
		t.Fatal(e)
	}
	embedNames, e := referenceBridgeSourceNames(embedded)
	if e != nil || !reflect.DeepEqual(dirNames, embedNames) {
		t.Fatal("both source sets must match", e)
	}
	first := referenceBridgeIdentityFrom(embedded)
	if first == "" || first == base {
		t.Fatal("added embedded membership is unbound")
	}
	embedded[extra] = &fstest.MapFile{Data: []byte("package project\nconst extraRule=2\n")}
	if changed := referenceBridgeIdentityFrom(embedded); changed == "" || changed == first {
		t.Fatal("PW-1: added embedded file bytes are unbound")
	}
	delete(embedded, extra)
	embedded["reference_preparation_renamed.go"] = directory[extra]
	if referenceBridgeIdentityFrom(embedded) == first {
		t.Fatal("source filename membership is unbound")
	}
}
