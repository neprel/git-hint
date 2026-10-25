package scan

import "testing"

func TestResultCopyRetainsPrivateFieldsWithoutAliasing(t *testing.T) {
	p := "parent"
	l := "go"
	n := 1.0
	r := &Result{Entities: []Entity{{Path: "x.go", Name: "symbol", Line: 3, Hash: "hash", LocalKey: "local", ParentID: &p, Language: &l, Flags: []string{"f"}, Features: map[string]Quantity{"n": {Value: &n}}}}, Ambiguous: map[string][]string{"a": {"b"}}, Relations: []Relation{{Source: "x"}}, Languages: []string{"go"}}
	c := r.Copy()
	if c.Entities[0].Path != "x.go" || c.Entities[0].LocalKey != "local" || c.Entities[0].Hash != "hash" {
		t.Fatal(c)
	}
	*c.Entities[0].ParentID = "new"
	*c.Entities[0].Language = "new"
	*c.Entities[0].Features["n"].Value = 2
	c.Entities[0].Flags[0] = "new"
	c.Ambiguous["a"][0] = "new"
	c.Relations[0].Source = "new"
	c.Languages[0] = "new"
	if p != "parent" || l != "go" || n != 1 || r.Entities[0].Flags[0] != "f" || r.Ambiguous["a"][0] != "b" || r.Relations[0].Source != "x" || r.Languages[0] != "go" {
		t.Fatal("alias")
	}
}

func TestResultCopyPreservesEmptyAndNilSlices(t *testing.T) {
	r := &Result{Entities: []Entity{{Flags: []string{}}}, Languages: []string{}, Relations: []Relation{}, Ambiguous: map[string][]string{"empty": {}, "nil": nil}}
	c := r.Copy()
	if c.Entities[0].Flags == nil || c.Languages == nil || c.Relations == nil || c.Ambiguous["empty"] == nil || c.Ambiguous["nil"] != nil {
		t.Fatal("nil/empty changed")
	}
}
