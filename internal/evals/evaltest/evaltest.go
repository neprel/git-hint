// Package evaltest builds synthetic eval fixtures for tests of the eval packages.
package evaltest

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// Library opens an eval library in a new temporary project.
func Library(t testing.TB) *library.Library {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return library.Open(s, 0)
}

// Tree stores a tree of regular files.
func Tree(t testing.TB, l *library.Library, files map[string]string) string {
	t.Helper()
	var fs []trees.File
	for p, c := range files {
		d, err := l.PutObject([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		fs = append(fs, trees.File{Path: p, Mode: "100644", Size: int64(len(c)), Digest: d})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	b, _ := json.Marshal(trees.Tree{TreeVersion: 1, Files: fs})
	d, err := l.PutObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Go bug-fix project (standard library only).
const (
	GoMod   = "module shop\n\ngo 1.25\n"
	GoBug   = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price }\n"
	GoFix   = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price - discount }\n"
	GoOld   = "package shop\n\nimport \"testing\"\n\nfunc TestNoDiscount(t *testing.T) {\n\tif Total(10, 0) != 10 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	GoAdded = "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif got := Total(10, 3); got != 7 {\n\t\tt.Fatalf(\"Total(10, 3) = %d\", got)\n\t}\n}\n"
)

// GoBugFix packages the synthetic Go bug fix and returns its eval record.
func GoBugFix(t testing.TB, l *library.Library) map[string]any {
	t.Helper()
	base := Tree(t, l, map[string]string{"go.mod": GoMod, "cart.go": GoBug, "cart_test.go": GoOld})
	result := Tree(t, l, map[string]string{"go.mod": GoMod, "cart.go": GoFix, "cart_test.go": GoOld, "discount_test.go": GoAdded})
	at := map[string]any{"source": library.Digest([]byte("synthetic-session")), "record": "u1"}
	b, err := build.Build(l, build.Input{EpisodeID: "ep-00000000000000e1", Family: "bug_fix", Repo: "shop", Base: base, Result: result,
		Paths: []string{"cart.go", "discount_test.go"}, Attribution: "exclusive", Outcome: "test_passed",
		Request: "Total ignores the discount. Fix Total so it subtracts the discount from the price.", RequestAt: at,
		CheckHint: "go test ./...", Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return b.Record
}

// Family fixtures: each is a complete Go project change with the request that asked for it.
type Change struct {
	Family  string
	Request string
	Base    map[string]string
	Result  map[string]string
	Paths   []string
	// Links and Context are the user's requirement mapping of the request.
	Links   []build.Link
	Context []string
}

// Families returns one complete change per task family.
func Families() []Change {
	shop := func(extra map[string]string) map[string]string {
		m := map[string]string{"go.mod": GoMod, "cart_test.go": GoOld}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	fixed := map[string]string{"cart.go": GoFix}
	return []Change{
		{"bug_fix", "Total ignores the discount. Fix Total so it subtracts the discount from the price.",
			shop(map[string]string{"cart.go": GoBug}), shop(map[string]string{"cart.go": GoFix, "discount_test.go": GoAdded}), []string{"cart.go", "discount_test.go"},
			[]build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: []string{GoBugFixTest}}}, []string{"Total ignores the discount."}},
		{"feature", "Add a Tax function that returns 10 percent of the price, rounded down.",
			shop(fixed), shop(map[string]string{"cart.go": GoFix + "\n// Tax returns 10 percent of the price, rounded down.\nfunc Tax(price int) int { return price / 10 }\n",
				"tax_test.go": "package shop\n\nimport \"testing\"\n\nfunc TestTax(t *testing.T) {\n\tif Tax(99) != 9 || Tax(100) != 10 {\n\t\tt.Fatal(\"tax\")\n\t}\n}\n"}),
			[]string{"cart.go", "tax_test.go"}, []build.Link{{Source: "R1", Quote: "Add a Tax function that returns 10 percent of the price, rounded down", Tests: []string{"tax_test.go::TestTax"}}}, nil},
		{"compatibility", "Change the Total API to accept an optional list of coupons; keep existing callers compatible.",
			shop(fixed), shop(map[string]string{"cart.go": "package shop\n\n// Total returns the price after the discount and the coupons.\nfunc Total(price, discount int, coupons ...int) int {\n\tt := price - discount\n\tfor _, c := range coupons {\n\t\tt -= c\n\t}\n\treturn t\n}\n",
				"coupon_test.go": "package shop\n\nimport \"testing\"\n\nfunc TestCoupons(t *testing.T) {\n\tif Total(10, 1, 2, 3) != 4 || Total(10, 1) != 9 {\n\t\tt.Fatal(\"coupons\")\n\t}\n}\n"}),
			[]string{"cart.go", "coupon_test.go"}, []build.Link{
				{Source: "R1", Quote: "Change the Total API to accept an optional list of coupons", Tests: []string{"coupon_test.go::TestCoupons"}},
				{Source: "R1", Quote: "keep existing callers compatible", Tests: []string{"C2"}}}, nil},
		{"refactor", "Refactor Total: rename its parameters p and d to price and discount; keep the behavior.",
			shop(map[string]string{"cart.go": "package shop\n\n// Total returns the price after the discount.\nfunc Total(p, d int) int { return p - d }\n"}),
			shop(fixed), []string{"cart.go"}, []build.Link{
				{Source: "R1", Quote: "Refactor Total: rename its parameters p and d to price and discount", Tests: []string{"C1"}},
				{Source: "R1", Quote: "keep the behavior", Tests: []string{"C2"}}}, nil},
	}
}

// RefactorExtract is a refactoring whose request is only partly visible in the exported API
// ("make Total use it" is internal): qualification must not validate it.
func RefactorExtract() Change {
	return Change{"refactor", "Refactor: extract the discount calculation into an exported function Discounted(price, discount int) int and make Total use it.",
		map[string]string{"go.mod": GoMod, "cart_test.go": GoOld, "cart.go": GoFix},
		map[string]string{"go.mod": GoMod, "cart_test.go": GoOld, "cart.go": "package shop\n\n// Discounted returns the price minus the discount.\nfunc Discounted(price, discount int) int { return price - discount }\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return Discounted(price, discount) }\n"},
		[]string{"cart.go"}, []build.Link{
			{Source: "R1", Quote: "extract the discount calculation into an exported function Discounted(price, discount int) int", Tests: []string{"C1"}},
			{Source: "R1", Quote: "make Total use it", Tests: nil}}, []string{"Refactor:"}}
}

// PanicRequirement is the reviewer's case of G10-3: the request adds a requirement that
// no test checks and that the reference breaks. It must not be validated.
func PanicRequirement() Change {
	c := Families()[0]
	c.Request = "Total ignores the discount. Fix Total so it subtracts the discount from the price. Total must panic on a negative discount."
	c.Links = append(c.Links, build.Link{Source: "R1", Quote: "Total must panic on a negative discount", Tests: nil})
	return c
}

// MapChange applies the user's mapping of c to a packaged record.
func MapChange(t testing.TB, l *library.Library, rec map[string]any, c Change) map[string]any {
	t.Helper()
	reqs, err := build.ApplyMapping(l, rec, build.Mapping{By: "user", Links: c.Links, Context: c.Context})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for k, v := range rec {
		out[k] = v
	}
	out["requirements"] = reqs
	return out
}

// Package builds the eval record of a family change.
func Package(t testing.TB, l *library.Library, c Change, check string) (*build.Built, error) {
	t.Helper()
	at := map[string]any{"source": library.Digest([]byte("synthetic-session-" + c.Family)), "record": "u1"}
	return build.Build(l, build.Input{EpisodeID: "ep-" + fmt16(c.Family), Family: c.Family, Repo: "shop",
		Base: Tree(t, l, c.Base), Result: Tree(t, l, c.Result), Paths: c.Paths, Attribution: "exclusive", Outcome: "test_passed",
		Request: c.Request, RequestAt: at, CheckHint: check, Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)})
}

func fmt16(s string) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 16)
	for i := range b {
		b[i] = hex[(int(s[i%len(s)])+i)%16]
	}
	return string(b)
}

// GoBugFixTest is the hidden test of GoBugFix.
const GoBugFixTest = "discount_test.go::TestDiscount"

// MapGoBugFix links the request of a GoBugFix record to its hidden test, as a user would,
// and returns the record with the new requirements.
func MapGoBugFix(t testing.TB, l *library.Library, rec map[string]any) map[string]any {
	t.Helper()
	reqs, err := build.ApplyMapping(l, rec, build.Mapping{By: "user",
		Links:   []build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: []string{GoBugFixTest}}},
		Context: []string{"Total ignores the discount."}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for k, v := range rec {
		out[k] = v
	}
	out["requirements"] = reqs
	return out
}
