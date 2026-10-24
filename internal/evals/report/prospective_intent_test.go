package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// All 0.3.1 fixtures are synthetic receipt assertions. None is a producer,
// qualification attestation, actual work observation or completion authority.
func prospectiveFixture(t *testing.T, group, name string) ([]byte, map[string]any) {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join("../../../tests/evalreport", group, name))
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	return raw, doc
}
func TestProspectiveIntentV031TypedRoundTripAndLegacyRefusal(t *testing.T) {
	raw, doc := prospectiveFixture(t, "valid", "report-v031-project-verification.json")
	if e := Check(raw); e != nil {
		t.Fatal(e)
	}
	p := array(doc["project_verifications"])[0]
	b, _ := json.Marshal(p)
	var typed ProjectVerification
	if e := json.Unmarshal(b, &typed); e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(typed)
	var again map[string]any
	json.Unmarshal(b, &again)
	if !sameValue(again, p) {
		t.Fatal("neutral wire types lost prospective identities")
	}
	if typed.Verifier.IntentVersion != "project-verification-intent-v1" || typed.Verifier.ReferenceInput.Domain != "hint-library-reference-patch-archive-v1" {
		t.Fatal("input domain lost")
	}
	if typed.Work.StartScopeDigest == typed.Work.FrozenOpenRevisionDigest || typed.Work.VerificationIntentDigest == typed.Work.FrozenOpenRevisionDigest {
		t.Fatal("original and frozen identities collapsed")
	}
	for _, old := range [][]byte{githint.SchemaEvalReportV01, githint.SchemaEvalReportV02, githint.SchemaEvalReportV03} {
		c := jsonschema.NewCompiler()
		d, e := jsonschema.UnmarshalJSON(bytes.NewReader(old))
		if e != nil {
			t.Fatal(e)
		}
		if e = c.AddResource("urn:test:old", d); e != nil {
			t.Fatal(e)
		}
		s, e := c.Compile("urn:test:old")
		if e != nil {
			t.Fatal(e)
		}
		if s.Validate(doc) == nil {
			t.Fatal("old reader admitted031")
		}
	}
	// Strict decode applies to the new exact version too.
	duplicate := bytes.Replace(raw, []byte(`"report_version": "0.3.1"`), []byte(`"report_version": "0.3.1", "report_version": "0.3.1"`), 1)
	if e := Check(duplicate); !errors.Is(e, ErrInvalid) {
		t.Fatal("duplicate031 field admitted", e)
	}
}

func TestProspectiveIntentV031SemanticNegativesAreSchemaValid(t *testing.T) {
	all, e := compiled()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"base-not-in-scope", "set-digest-substitution", "verifier-source-echo", "late-intent", "changed-scope", "started-with-member", "missing-original", "intent-only-late-intent", "intent-only-base-not-in-scope", "intent-only-started-with-member"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := prospectiveFixture(t, "invalid", "v031-"+name+".json")
			d, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if e != nil {
				t.Fatal(e)
			}
			if e = all["0.3.1"].Validate(d); e != nil {
				t.Fatalf("negative did not reach consistency rule: %v", e)
			}
			if e = Check(raw); !errors.Is(e, ErrInvalid) {
				t.Fatal("inconsistent prospective receipt accepted", e)
			}
		})
	}
}

func TestProspectiveIntentCannotEnableOrDowngradeDefaultWriter(t *testing.T) {
	if Version != "0.2.1" {
		t.Fatal("default writer changed")
	}
	_, doc := prospectiveFixture(t, "valid", "report-v031-prospective-intent-only.json")
	l, names := seeded(t)
	b := Builder{Key: fixedKey, Namespace: fixedNS, WorkUnits: array(doc["work_units"])}
	out, used, e := b.Build(l, names, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if !errors.Is(e, ErrInvalid) || out != nil || used != nil {
		t.Fatal("legacy writer emitted or dropped prospective fields", e)
	}
}
