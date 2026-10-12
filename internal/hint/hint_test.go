package hint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The client implements the contract itself. These tests run it on the fixtures of the
// conformance suite and require the expected results there.
const conf = "../../tests/conformance"

func load(t *testing.T, p string) (any, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Plain(dec)
	if err != nil {
		t.Fatal(err)
	}
	return dec, doc
}

func TestFixturesAgreeWithConformanceSuite(t *testing.T) {
	sch, err := LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := filepath.Glob(filepath.Join(conf, "valid", "*.json"))
	ex, _ := filepath.Glob("../../examples/v0.1/*.json")
	for _, f := range append(valid, ex...) {
		dec, doc := load(t, f)
		if err := sch.Validate(dec); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if fs := Check(doc, nil); len(fs) > 0 {
			t.Errorf("%s: %v", f, fs)
		}
	}
	var expected map[string]struct{ Level, Code string }
	b, _ := os.ReadFile(filepath.Join(conf, "invalid", "expected.json"))
	if err := json.Unmarshal(b, &expected); err != nil || len(expected) < 70 {
		t.Fatalf("expected.json: %v (%d)", err, len(expected))
	}
	for name, e := range expected {
		dec, doc := load(t, filepath.Join(conf, "invalid", name))
		err := sch.Validate(dec)
		if e.Level == "schema" {
			if err == nil {
				t.Errorf("%s: schema accepted it", name)
			}
			continue
		}
		found := false
		for _, f := range Check(doc, nil) {
			found = found || f.Code == e.Code
		}
		if err != nil || !found {
			t.Errorf("%s: want semantic %s (schema err %v)", name, e.Code, err)
		}
	}
}

func TestImportAgreesWithConformanceSuite(t *testing.T) {
	sch, _ := LoadSchemas()
	var expected map[string]struct {
		Local    string
		Expected ImportResult
	}
	b, _ := os.ReadFile(filepath.Join(conf, "resolve", "expected.json"))
	if err := json.Unmarshal(b, &expected); err != nil || len(expected) == 0 {
		t.Fatal(err)
	}
	pb, _ := os.ReadFile("../../examples/v0.1/export-policy.json")
	var pd map[string]any
	_ = json.Unmarshal(pb, &pd)
	policy := pd["payload"].(map[string]any)
	for name, e := range expected {
		var local LocalState
		lb, _ := os.ReadFile(filepath.Join(conf, "resolve", e.Local))
		if err := json.Unmarshal(lb, &local); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(conf, "resolve", "cases", name))
		got := ImportRecommendations(raw, sch, local, policy)
		if len(got.Plans) == 0 {
			got.Plans = nil
		}
		want := e.Expected
		if len(want.Plans) == 0 {
			want.Plans = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestIDVectors(t *testing.T) {
	var v struct {
		Keys    map[string]string
		Vectors []struct {
			Key, Domain, ID string
			Parts           []string
		}
	}
	b, _ := os.ReadFile(filepath.Join(conf, "vectors", "ids-v0.1.json"))
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, x := range v.Vectors {
		k := make([]byte, 32)
		for i := range k {
			var c byte
			for j := 0; j < 2; j++ {
				h := v.Keys[x.Key][2*i+j]
				switch {
				case h >= 'a':
					c = c*16 + h - 'a' + 10
				default:
					c = c*16 + h - '0'
				}
			}
			k[i] = c
		}
		var id string
		switch x.Domain {
		case "trace":
			id, _ = TraceID(k, x.Parts[0])
		case "span":
			id, _ = SpanID(k, x.Parts[0], x.Parts[1])
		default:
			id = DeriveID(k, x.Domain, x.Parts...)
		}
		if id != x.ID {
			t.Errorf("%s %v: %s != %s", x.Domain, x.Parts, id, x.ID)
		}
	}
}
