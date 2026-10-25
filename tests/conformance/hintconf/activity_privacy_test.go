package hintconf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestActivityPrivacyMinorAndLegacyPatches(t *testing.T) {
	sch, err := LoadSchemas("../../../schemas/v0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ file, wire string }{
		{"examples/v0.1/activity-trace.json", "0.1.7"},
		{"examples/v0.2/activity-model-ledger-agent-run.json", "0.2.1"},
		{"examples/v0.2/activity-model-ledger-agent-run.json", "0.2.19"},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			b, e := os.ReadFile(filepath.Join("../../..", tc.file))
			if e != nil {
				t.Fatal(e)
			}
			var d map[string]any
			json.Unmarshal(b, &d)
			d["hint_version"] = tc.wire
			b, _ = json.Marshal(d)
			decoded, e := Decode(b)
			if e != nil || sch.Validate(decoded) != nil {
				t.Fatal("legacy patch compatibility changed")
			}
		})
	}
	b, e := os.ReadFile(filepath.Join("../../..", "examples/v0.2/activity-model-ledger-agent-run.json"))
	if e != nil {
		t.Fatal(e)
	}
	var d map[string]any
	json.Unmarshal(b, &d)
	for _, v := range []string{"0.3.0", "0.3.7", "0.4.0"} {
		d["hint_version"] = v
		b, _ = json.Marshal(d)
		decoded, _ := Decode(b)
		if sch.Validate(decoded) == nil {
			t.Fatal("raw legacy body accepted as keyed or unknown minor")
		}
	}
}

// Version changes must not bypass inherited v0.2 semantic gates. These frozen
// synthetic fixtures need no private-content conversion; all selected bodies lack B/image/A.
func TestActivityPrivacyInheritsSemanticGates(t *testing.T) {
	sch, err := LoadSchemas("../../../schemas/v0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, code string }{
		{"cli-root-observation-trace", CodeSourceCLIRoot},
		{"cli-root-usage-normalization", CodeSourceCLIRoot},
		{"call-link-other-kind", CodeCallLink},
		{"ledger-canonical-usage-known", CodeLedger},
		{"ledger-complete-with-late", CodeLedger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range []string{"0.3.0", "0.3.7"} {
				b, e := os.ReadFile(filepath.Join("../../..", "tests/conformance/invalid", tc.name+".json"))
				if e != nil {
					t.Fatal(e)
				}
				var d map[string]any
				json.Unmarshal(b, &d)
				d["hint_version"] = v
				for _, raw := range d["sources"].([]any) {
					a := raw.(map[string]any)["adapter"].(map[string]any)
					if a["name"] == "hint-model-ledger" {
						a["version"] = "0.3.0"
					}
				}
				d["content_digest"], _ = Digest(d["payload"])
				b, _ = json.Marshal(d)
				decoded, _ := Decode(b)
				if e := sch.Validate(decoded); e != nil {
					t.Fatalf("fixture no longer exercises semantic check:%v", e)
				}
				found := false
				for _, f := range Check(d, nil) {
					found = found || f.Code == tc.code
				}
				if !found {
					t.Fatal("new version bypassed inherited semantic gate")
				}
			}
		})
	}
}
