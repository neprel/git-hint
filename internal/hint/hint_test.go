package hint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// v0.2 is v0.1 plus the context measurement of an observation (and "context" as a missing
// field) and the effort findings of a recommendation set: with the version strings replaced and these additions removed, every
// v0.2 schema equals its v0.1 schema.
func TestV02IsV01PlusContextAndFindings(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "schemas", "v0.1", "*.schema.json"))
	if len(files) == 0 {
		t.Fatal("no v0.1 schemas")
	}
	for _, f := range files {
		a, _ := os.ReadFile(f)
		b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0.2", filepath.Base(f)))
		if err != nil {
			t.Fatal(err)
		}
		var v1, v2 map[string]any
		json.Unmarshal([]byte(strings.NewReplacer("urn:hint:schema:v0.1:", "urn:hint:schema:v0.2:", "HINT v0.1", "HINT v0.2").Replace(string(a))), &v1)
		json.Unmarshal(b, &v2)
		switch filepath.Base(f) {
		case "common.schema.json":
			v1["$defs"].(map[string]any)["SpecVersion"].(map[string]any)["pattern"] = `^0\.2\.(0|[1-9][0-9]*)$`
			fn := v2["$defs"].(map[string]any)["FieldName"].(map[string]any)
			fn["enum"] = fn["enum"].([]any)[:len(fn["enum"].([]any))-1] // "context" (missing context coverage) is the last entry
		case "activity.schema.json":
			o := v2["$defs"].(map[string]any)["Observation"].(map[string]any)
			delete(o["properties"].(map[string]any), "context")
			all := o["allOf"].([]any)
			o["allOf"] = all[:len(all)-1] // the context condition is the last entry
		case "recommendation.schema.json":
			defs := v2["$defs"].(map[string]any)
			delete(defs, "Finding")
			delete(defs, "Abstention")
			delete(defs, "Decision")
			delete(defs, "ContextFeedback")
			delete(defs, "ProjectComparison")
			delete(defs, "ForecastBasis")
			delete(defs["Plan"].(map[string]any)["properties"].(map[string]any), "forecast_basis")
			props := defs["RecommendationPayload"].(map[string]any)["properties"].(map[string]any)
			delete(props, "findings")
			delete(props, "abstentions")
			delete(props, "context_feedback")
			delete(props, "project_comparisons")
		case "policy.schema.json":
			var strip func(v any)
			strip = func(v any) {
				switch n := v.(type) {
				case map[string]any:
					if pl, ok := n["Plan"].(map[string]any); ok && pl["type"] == "array" {
						items := pl["items"].(map[string]any)
						var keep []any
						for _, x := range items["enum"].([]any) {
							if x != "forecast_basis" {
								keep = append(keep, x)
							}
						}
						items["enum"] = keep
					}
					if obs, ok := n["Observation"].(map[string]any); ok && obs["type"] == "array" {
						items := obs["items"].(map[string]any)
						var keep []any
						for _, x := range items["enum"].([]any) {
							if x != "context" {
								keep = append(keep, x)
							}
						}
						items["enum"] = keep
					}
					for _, x := range n {
						strip(x)
					}
				case []any:
					for _, x := range n {
						strip(x)
					}
				}
			}
			strip(v2)
		}
		if !reflect.DeepEqual(v1, v2) {
			t.Errorf("%s: v0.2 differs from v0.1 beyond the context measurement", filepath.Base(f))
		}
	}
}
