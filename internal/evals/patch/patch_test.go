package patch

import "testing"

const base = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price }\n\n// helper helps.\n\nfunc helper() {}\n"
const result = "package shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price - discount }\n\n// helper helps.\n\nfunc helper() {}\n"

func TestPortOntoRefactoredFile(t *testing.T) {
	// the refactoring renamed an unrelated function and added a file header
	target := "// Package shop sells.\npackage shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price }\n\n// helper helps.\n\nfunc assist() {}\n"
	got, ok := Port(base, result, target)
	want := "// Package shop sells.\npackage shop\n\n// Total returns the price after the discount.\nfunc Total(price, discount int) int { return price - discount }\n\n// helper helps.\n\nfunc assist() {}\n"
	if !ok || got != want {
		t.Fatalf("%v %q", ok, got)
	}
}

func TestPortRefusesUnclearTargets(t *testing.T) {
	for name, target := range map[string]string{
		"changed line": "package shop\n\n// Total returns the price after the discount.\nfunc Total(p, d int) int { return p }\n\n// helper helps.\n\nfunc helper() {}\n",
		"moved away":   "package shop\n",
		"twice":        base + base,
	} {
		if _, ok := Port(base, result, target); ok {
			t.Fatalf("%s: applied", name)
		}
	}
}

func TestApplyExceptAll(t *testing.T) {
	if got := ApplyExcept(base, Hunks(base, result), -1); got != result {
		t.Fatalf("%q", got)
	}
}
