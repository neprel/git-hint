package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/hint"
)

// v021 turns the 0.2.0 fixture into a 0.2.1 report with trust values and a work unit
// attempt bound to its first trial; the digest is recomputed.
func v021(t *testing.T, edit func(m map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../tests/evalreport/valid/report-v02-three-trials.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["report_version"] = "0.2.1"
	for _, x := range m["trials"].([]any) {
		tr := x.(map[string]any)
		tr["result_trust"] = "same_process"
		tr["qualification"].(map[string]any)["result_trust"] = "unknown"
	}
	if edit != nil {
		edit(m)
	}
	delete(m, "content_digest")
	d, err := hint.Digest(m)
	if err != nil {
		t.Fatal(err)
	}
	m["content_digest"] = d
	out, _ := json.Marshal(m)
	return out
}

// 07B-J §11.4: from 0.2.1 every trial carries its value and the qualification cap; 0.2.0
// stays valid without them.
func TestResultTrustInReports(t *testing.T) {
	legacy, _ := os.ReadFile("../../../tests/evalreport/valid/report-v02-three-trials.json")
	if err := Check(legacy); err != nil {
		t.Fatalf("a 0.2.0 report without result_trust: %v", err)
	}
	if err := Check(v021(t, nil)); err != nil {
		t.Fatalf("0.2.1: %v", err)
	}
	for name, edit := range map[string]func(m map[string]any){
		"trial without value": func(m map[string]any) { delete(m["trials"].([]any)[0].(map[string]any), "result_trust") },
		"no cap": func(m map[string]any) {
			delete(m["trials"].([]any)[0].(map[string]any)["qualification"].(map[string]any), "result_trust")
		},
		"invalid value": func(m map[string]any) { m["trials"].([]any)[0].(map[string]any)["result_trust"] = "trusted" },
	} {
		if err := Check(v021(t, edit)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A closure equals the minimum of its trial's value and the cap; a closure above that, or
// without a value, is refused.
func TestClosureTrustRule(t *testing.T) {
	doc := func(closure string, rt, qt string) map[string]any {
		return map[string]any{"report_version": "0.2.1",
			"trials":     []any{map[string]any{"trial_ref": "t1", "result_trust": rt, "qualification": map[string]any{"result_trust": qt}}},
			"work_units": []any{map[string]any{"attempts": []any{map[string]any{"trial_ref": "t1", "closure": map[string]any{"result_trust": closure}}}}}}
	}
	for _, c := range []struct {
		closure, rt, qt string
		ok              bool
	}{
		{"same_process", "same_process", "process_separated", true},
		{"unknown", "process_separated", "unknown", true},
		{"process_separated", "process_separated", "same_process", false}, // above the cap
		{"process_separated", "same_process", "process_separated", false}, // above the trial
		{"", "same_process", "same_process", false},
	} {
		m := doc(c.closure, c.rt, c.qt)
		if c.closure == "" {
			delete(m["work_units"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)["closure"].(map[string]any), "result_trust")
		}
		if err := trustProblem(m); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// 07B-J §11.4(7): the strict report gate. A server with the old 0.2 schema (no
// result_trust) refuses a 0.2.1 report that carries it.
func TestResultTrustReportDowngrade(t *testing.T) {
	var d map[string]any
	json.Unmarshal(githint.SchemaEvalReportV02, &d)
	defs := d["$defs"].(map[string]any)
	delete(defs, "ResultTrust")
	delete(defs["Trial"].(map[string]any)["properties"].(map[string]any), "result_trust")
	delete(defs["Trial"].(map[string]any)["properties"].(map[string]any)["qualification"].(map[string]any)["properties"].(map[string]any), "result_trust")
	delete(defs["Attempt"].(map[string]any)["properties"].(map[string]any)["closure"].(map[string]any)["properties"].(map[string]any), "result_trust")
	raw, _ := json.Marshal(d)
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	c.AddResource("urn:old", doc)
	old, err := c.Compile("urn:old")
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := jsonschema.UnmarshalJSON(bytes.NewReader(v021(t, nil)))
	if old.Validate(rep) == nil {
		t.Fatal("the old schema accepts a 0.2.1 report with result_trust")
	}
	legacy, _ := os.ReadFile("../../../tests/evalreport/valid/report-v02-three-trials.json")
	ld, _ := jsonschema.UnmarshalJSON(bytes.NewReader(legacy))
	if err := old.Validate(ld); err != nil {
		t.Fatalf("the old schema refuses its own 0.2.0 report: %v", err)
	}
}
