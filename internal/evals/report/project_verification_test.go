package report

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	githint "github.com/neprel/git-hint"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestProjectVerificationTransportDoesNotEnableWriter(t *testing.T) {
	if Version != "0.2.1" {
		t.Fatal("03 writer is not authorized")
	}
	raw, err := os.ReadFile("../../../tests/evalreport/valid/report-v03-project-verification.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	var record ProjectVerification
	b, _ := json.Marshal(doc["project_verifications"].([]any)[0])
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(record)
	var projected map[string]any
	json.Unmarshal(again, &projected)
	if !sameValue(projected, doc["project_verifications"].([]any)[0]) {
		t.Fatal("typed record loses a contract field")
	}
	for _, old := range [][]byte{githint.SchemaEvalReportV01, githint.SchemaEvalReportV02} {
		c := jsonschema.NewCompiler()
		schemaDoc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(old))
		c.AddResource("urn:test:legacy", schemaDoc)
		s, err := c.Compile("urn:test:legacy")
		if err != nil {
			t.Fatal(err)
		}
		if s.Validate(doc) == nil {
			t.Fatal("legacy schema accepted03")
		}
	}
}
