package report

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

func fixture04(t *testing.T, full bool) ([]byte, map[string]any) {
	t.Helper()
	name := "report-v04-synthetic-original-only.json"
	if full {
		name = "report-v04-synthetic-project-verification.json"
	}
	raw, e := os.ReadFile("../../../tests/evalreport/valid/" + name)
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatal("literal JSON")
	}
	return raw, doc
}
func reseal04(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	delete(doc, "content_digest")
	var e error
	doc["content_digest"], e = hint.Digest(doc)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestProjectV04LiteralFixturesAndTypedRoundTrip(t *testing.T) {
	for _, full := range []bool{false, true} {
		raw, m := fixture04(t, full)
		if e := Check(raw); e != nil {
			t.Fatal(e)
		}
		if full {
			p := array(m["project_verifications"])[0]
			b, _ := json.Marshal(p)
			var v ProjectVerificationV2
			if e := json.Unmarshal(b, &v); e != nil {
				t.Fatal(e)
			}
			b, _ = json.Marshal(v)
			var got any
			json.Unmarshal(b, &got)
			if !sameValue(got, p) {
				t.Fatal("v2 typed fields changed")
			}
		}
	}
}
func TestProjectV04PatchAndLegacyIsolation(t *testing.T) {
	_, doc := fixture04(t, true)
	for _, v := range []string{"0.4.0", "0.4.1", "0.4.27"} {
		doc["report_version"] = v
		if e := Check(reseal04(t, doc)); e != nil {
			t.Fatal(v, e)
		}
	}
	for _, v := range []string{"0.3.0", "0.3.1", "0.3.2", "0.4.01", "0.4.1\n", "0.5.0"} {
		doc["report_version"] = v
		if Check(reseal04(t, doc)) == nil {
			t.Fatal("version swap accepted", v)
		}
	}
	if Version != "0.2.1" {
		t.Fatal("normal writer changed")
	}
	_, original := fixture04(t, false)
	b := Builder{Namespace: str(original["project_namespace"]), Key: bytes.Repeat([]byte{1}, 32), WorkUnits: array(original["work_units"])}
	if _, _, e := b.Build(nil, nil, time.Now()); e == nil {
		t.Fatal("normal legacy builder accepted verified original")
	}
}
func TestProjectV04StrictWireAndForbiddenRawSlots(t *testing.T) {
	raw, _ := fixture04(t, true)
	bad := bytes.Replace(raw, []byte(`"report_version": "0.4.0"`), []byte(`"report_version": "0.4.0", "report_version": "0.4.0"`), 1)
	if Check(bad) == nil {
		t.Fatal("duplicate accepted")
	}
	if Check(append(raw, []byte("{}")...)) == nil {
		t.Fatal("trailing value")
	}
	for _, field := range []string{"definition_digest", "qualification_ref", "spec_digest", "inputs_digest", "reference_artifact_digest", "reference_preparation_digest", "capture_policy_digest", "capture_source_digest"} {
		_, doc := fixture04(t, false)
		intent := object(object(object(array(doc["work_units"])[0])["scope"])["verification_intent"])
		intent[field] = nil
		if Check(reseal04(t, doc)) == nil {
			t.Fatal("forbidden raw/legacy slot", field)
		}
	}
	for _, field := range []string{"eval_name", "previous_name", "manifest_digest", "archive_digests", "version"} {
		_, doc := fixture04(t, false)
		intent := object(object(object(array(doc["work_units"])[0])["scope"])["verification_intent"])
		object(intent["qualification_source"])[field] = nil
		if Check(reseal04(t, doc)) == nil {
			t.Fatal("qualification leak slot", field)
		}
	}
}

// Refresh seals without repairing relational claims: negatives must reach semantics.
func resealProject04(t *testing.T, doc map[string]any) {
	t.Helper()
	_, original := fixture04(t, true)
	oldStart := object(array(original["work_units"])[0])
	oldIntent := object(object(oldStart["scope"])["verification_intent"])
	oldIntentDigest, _ := hint.Digest(oldIntent)
	revisions := map[string]string{}
	scopes := map[string]string{}
	for _, x := range array(doc["work_units"]) {
		u := object(x)
		oldRevision := str(u["revision_digest"])
		oldScope := str(u["scope_digest"])
		if next, ok := revisions[str(u["predecessor"])]; ok {
			u["predecessor"] = next
		}
		u["scope_digest"], _ = hint.Digest(u["scope"])
		scopes[oldScope] = str(u["scope_digest"])
		delete(u, "revision_digest")
		u["revision_digest"], _ = hint.Digest(u)
		revisions[oldRevision] = str(u["revision_digest"])
	}
	var newIntent map[string]any
	if rows := array(doc["work_units"]); len(rows) > 0 {
		newIntent = object(object(object(rows[0])["scope"])["verification_intent"])
	}
	for _, x := range array(doc["project_verifications"]) {
		p := object(x)
		w := object(p["work"])
		if next, ok := scopes[str(w["start_scope_digest"])]; ok {
			w["start_scope_digest"] = next
		}
		if next, ok := revisions[str(w["frozen_open_revision_digest"])]; ok {
			w["frozen_open_revision_digest"] = next
		}
		if newIntent != nil {
			if str(w["verification_intent_digest"]) == oldIntentDigest {
				w["verification_intent_digest"], _ = hint.Digest(newIntent)
			}
			v := object(p["verifier"])
			for k, val := range oldIntent {
				if reflect.DeepEqual(v[k], val) {
					v[k] = newIntent[k]
				}
			}
		}
		delete(p, "content_digest")
		p["content_digest"], _ = hint.Digest(p)
	}
}
func TestProjectV04SemanticMutationsReachSharedChecks(t *testing.T) {
	all, e := compiled()
	if e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(map[string]any){
		"foreign_preparation_unit": func(m map[string]any) {
			for _, x := range array(m["work_units"]) {
				object(object(object(x)["scope"])["verification_intent"])["reference_preparation_ref"] = strings.Repeat("f", 32) + "." + strings.Repeat("e", 32) + ".outcome"
			}
		},
		"missing_closure_trust": func(m map[string]any) {
			rs := array(m["work_units"])
			delete(object(object(array(object(rs[1])["attempts"])[0])["closure"]), "result_trust")
		},
		"late_intent": func(m map[string]any) {
			delete(object(object(array(m["work_units"])[0])["scope"]), "verification_intent")
		},
		"missing_original": func(m map[string]any) { m["work_units"] = array(m["work_units"])[1:] },
		"member_at_start": func(m map[string]any) {
			rs := array(m["work_units"])
			object(rs[0])["attempts"] = object(rs[1])["attempts"]
		},
		"base_not_in_scope": func(m map[string]any) {
			for _, x := range array(m["work_units"]) {
				object(object(object(object(x)["scope"])["verification_intent"])["start_binding"])["base_snapshot_id"] = strings.Repeat("f", 32)
			}
		},
		"echo_mismatch": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["verifier"])["spec_ref"] = strings.Repeat("f", 32)
		},
		"set_digest_mismatch": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["plan"])["recommendation_set_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		"frozen_wrong": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["work"])["frozen_open_revision_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		"execution_collision": func(m map[string]any) {
			rs := array(m["work_units"])
			a := object(array(object(rs[1])["attempts"])[0])
			object(array(m["project_verifications"])[0])["verification_execution_ref"] = a["attempt_ref"]
		},
		"trust_inflation": func(m map[string]any) {
			p := object(array(m["project_verifications"])[0])
			object(p["verifier"])["qualification_result_trust"] = "unknown"
		},
		"closure_mismatch": func(m map[string]any) {
			rs := array(m["work_units"])
			object(object(array(object(rs[2])["attempts"])[0])["closure"])["project_verification_ref"] = strings.Repeat("f", 32)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, m := fixture04(t, true)
			mutate(m)
			resealProject04(t, m)
			raw := reseal04(t, m)
			var v any
			json.Unmarshal(raw, &v)
			if e := all["0.4"].Validate(v); e != nil {
				t.Fatal("mutation failed schema instead of semantics", e)
			}
			if Check(raw) == nil {
				t.Fatal("semantic mutation accepted")
			}
		})
	}
}

var intent04Fields = []string{"intent_version", "definition_ref", "definition_version_ref", "qualification_source", "spec_ref", "inputs_ref", "reference_input", "reference_artifact_ref", "reference_preparation_ref", "reference_preparation_version_ref", "capture_policy_ref", "capture_policy_version_ref", "capture_source_ref", "capture_source_version_ref", "mandatory_checks_digest", "start_binding"}

func TestProjectV04ClosedRequiredIntentFields(t *testing.T) {
	for _, field := range intent04Fields {
		for _, mode := range []string{"missing", "null", "number", "case_variant"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				_, m := fixture04(t, false)
				intent := object(object(object(array(m["work_units"])[0])["scope"])["verification_intent"])
				switch mode {
				case "missing":
					delete(intent, field)
				case "null":
					intent[field] = nil
				case "number":
					intent[field] = 7
				case "case_variant":
					intent[strings.ToUpper(field)] = intent[field]
					delete(intent, field)
				}
				if Check(reseal04(t, m)) == nil {
					t.Fatal("closed required field bypass")
				}
			})
		}
	}
}
func TestProjectV04SchemaOnlyPrivacyAndProtocolMutations(t *testing.T) {
	all, e := compiled()
	if e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(map[string]any){
		"non_null_application": func(m map[string]any) {
			object(array(object(array(m["work_units"])[1])["attempts"])[0])["application_change_set"] = strings.Repeat("f", 32)
		},
		"legacy_check_ref": func(m map[string]any) {
			object(array(object(object(array(m["project_verifications"])[0])["verdict"])["checks"])[0])["check_id"] = strings.Repeat("f", 32)
		},
		"wrong_check_digest": func(m map[string]any) {
			for _, x := range array(m["work_units"]) {
				object(object(object(x)["scope"])["verification_intent"])["mandatory_checks_digest"] = "sha256:" + strings.Repeat("f", 64)
			}
		},
		"missing_check": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["verdict"])["checks"] = []any{}
		},
		"extra_check": func(m map[string]any) {
			v := object(object(array(m["project_verifications"])[0])["verdict"])
			v["checks"] = append(array(v["checks"]), array(v["checks"])[0])
		},
		"raw_result": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["result"])["artifact_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		"raw_image": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["verifier"])["evaluator_image_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		"raw_outputs": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["outputs"])["candidate_outputs_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		"legacy_tree": func(m map[string]any) {
			object(object(object(object(array(m["work_units"])[0])["scope"])["verification_intent"])["start_binding"])["base_hint_tree_digest"] = "sha256:" + strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, m := fixture04(t, true)
			mutate(m)
			if all["0.4"].Validate(m) == nil {
				t.Fatal("privacy/protocol mutation did not refuse at schema")
			}
			if Check(reseal04(t, m)) == nil {
				t.Fatal("schema mutation accepted")
			}
		})
	}
}

func TestProjectV04SchemaRejectsFinalNewlinesIndependently(t *testing.T) {
	all, e := compiled()
	if e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(map[string]any){
		"ref": func(m map[string]any) {
			i := object(object(object(array(m["work_units"])[0])["scope"])["verification_intent"])
			i["definition_version_ref"] = str(i["definition_version_ref"]) + "\n"
		},
		"preparation": func(m map[string]any) {
			i := object(object(object(array(m["work_units"])[0])["scope"])["verification_intent"])
			i["reference_preparation_ref"] = str(i["reference_preparation_ref"]) + "\n"
		},
		"producer_version": func(m map[string]any) { object(m["producer"])["version"] = "0.0.0-dev\n" },
		"runner_version": func(m map[string]any) {
			object(object(array(m["project_verifications"])[0])["verifier"])["runner_version"] = "0.1.0\n"
		},
		"report_version": func(m map[string]any) { m["report_version"] = "0.4.0\n" },
	} {
		t.Run(name, func(t *testing.T) {
			_, m := fixture04(t, true)
			mutate(m)
			if all["0.4"].Validate(m) == nil {
				t.Fatal("schema admitted final newline")
			}
		})
	}
}
