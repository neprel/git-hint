package evals

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githint "github.com/neprel/git-hint"
)

func fixtures(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "tests", "evals", dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

func kindOf(name string) string { return strings.SplitN(name, "-", 2)[0] }

func TestValidFixtures(t *testing.T) {
	for name, b := range fixtures(t, "valid") {
		if err := Validate(kindOf(name), b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidFixtures(t *testing.T) {
	for name, b := range fixtures(t, "invalid") {
		if err := Validate(kindOf(name), b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every property of every record schema says how it may be stored and exported.
func TestEveryFieldHasAClass(t *testing.T) {
	classes := map[string]bool{"sensitive": true, "local": true, "reportable": true}
	for _, k := range Kinds {
		b, err := githint.SchemasEvalsV1.ReadFile("schemas/evals-v1/" + k + ".schema.json")
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		for name, p := range s.Properties {
			if name == "record" || name == "manifest_version" || name == "policy_version" {
				continue
			}
			c, _ := p["x-hint-class"].(string)
			if !classes[c] {
				t.Errorf("%s.%s: x-hint-class %q", k, name, c)
			}
		}
	}
}

func TestPolicyDeniesByDefault(t *testing.T) {
	var none *Policy
	for _, op := range []Op{OpCapture, OpModel, OpExecute, OpExport} {
		var d *Denied
		if err := none.Check(op, ""); !errors.As(err, &d) {
			t.Errorf("nil policy allows %s", op)
		}
	}
	p := Enabled(nil)
	if err := p.Check(OpCapture, "opencode"); err != nil {
		t.Fatal(err)
	}
	if p.Check(OpCapture, "codex") == nil {
		t.Error("capture of an agent outside the policy")
	}
	for _, op := range []Op{OpModel, OpExecute, OpExport} {
		if p.Check(op, "") == nil {
			t.Errorf("enabling capture also allows %s", op)
		}
	}
	off := Policy{PolicyVersion: 1, Capture: "off"}
	if off.Check(OpCapture, "opencode") == nil {
		t.Error("capture off allows capture")
	}
}

func TestPolicyPermissionsAreSeparate(t *testing.T) {
	p := Enabled([]string{"opencode"})
	p.Run = &Run{Use: "docker", Runner: "harbor"}
	if err := p.Check(OpExecute, ""); err != nil {
		t.Fatal(err)
	}
	if p.Check(OpModel, "") == nil || p.Check(OpExport, "") == nil {
		t.Error("execution permission leaks into model or export")
	}
	p.Model = &Model{Use: "lan", BaseURL: "http://192.168.1.20:8080/v1", Model: "m", KeyEnv: "HINT_LOCAL_MODEL_KEY"}
	if err := p.Check(OpModel, ""); err != nil {
		t.Fatal(err)
	}
	p.Model.BaseURL = "https://api.example.com/v1" // not a private network address
	if p.Check(OpModel, "") == nil {
		t.Error("a public model endpoint passes the policy")
	}
}

func TestEnabledPolicyIsValidAndFinite(t *testing.T) {
	p := Enabled(nil)
	if err := ValidateValue("policy", p); err != nil {
		t.Fatal(err)
	}
	l := p.Limits()
	if l.RetentionDays <= 0 || l.QuotaBytes <= 0 || l.BatchEpisodes <= 0 || l.BatchSeconds <= 0 || l.TrialSeconds <= 0 || l.ModelRequests <= 0 {
		t.Fatalf("unbounded default: %+v", l)
	}
}
