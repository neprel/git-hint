package evals

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
)

// 07B-J §11.4(7): the strict local manifest gate. An old binary (the schemas without
// result_trust) refuses a new eval or trial record that carries it; the new schemas accept
// old records without it (read as unknown).
func TestResultTrustManifestDowngrade(t *testing.T) {
	old := fstest.MapFS{}
	files, _ := githint.SchemasEvalsV1.ReadDir("schemas/evals-v1")
	for _, f := range files {
		b, _ := githint.SchemasEvalsV1.ReadFile("schemas/evals-v1/" + f.Name())
		var d map[string]any
		json.Unmarshal(b, &d)
		props, _ := d["properties"].(map[string]any)
		delete(props, "result_trust") // trial
		if q, _ := props["qualification"].(map[string]any); q != nil {
			delete(q["properties"].(map[string]any), "result_trust") // eval
		}
		if defs, _ := d["$defs"].(map[string]any); defs != nil {
			delete(defs, "ResultTrust")
		}
		out, _ := json.Marshal(d)
		old["schemas/evals-v1/"+f.Name()] = &fstest.MapFile{Data: out}
	}
	oldSchemas, err := compile(old, "schemas/evals-v1")
	if err != nil {
		t.Fatal(err)
	}
	for kind, fixture := range map[string]string{"eval": "eval-bugfix-validated.json", "trial": "trial-passed.json"} {
		b, err := os.ReadFile("../../tests/evals/valid/" + fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(kind, b); err != nil {
			t.Fatalf("%s: an old record without result_trust is refused: %v", kind, err)
		}
		var rec map[string]any
		json.Unmarshal(b, &rec)
		if kind == "eval" {
			rec["qualification"].(map[string]any)["result_trust"] = "same_process"
		} else {
			rec["result_trust"] = "same_process"
		}
		nb, _ := json.Marshal(rec)
		if err := Validate(kind, nb); err != nil {
			t.Fatalf("%s: the new record is refused: %v", kind, err)
		}
		doc, _ := jsonUnmarshal(nb)
		if oldSchemas[kind].Validate(doc) == nil {
			t.Fatalf("%s: the old schema accepts result_trust (no fail-closed downgrade)", kind)
		}
		bad := strings.Replace(string(nb), `"same_process"`, `"trusted"`, 1)
		if Validate(kind, []byte(bad)) == nil {
			t.Fatalf("%s: an invalid trust value is accepted", kind)
		}
	}
}

func jsonUnmarshal(b []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}
