package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
)

func originalProjectionFixture() OriginalProjectionInput {
	c := activity.Configuration{}
	c.Agent.Name = "opencode"
	c.Agent.Version = activity.VersionState{State: "known", Value: "2.0.26"}
	c.Model = activity.ModelState{State: "known", Ref: strings.Repeat("6", 32), Provenance: "launch_parameter"}
	c.Harness.Kind = "hint-docker"
	c.Harness.Version = activity.VersionState{State: "known", Value: "0.1.0"}
	c.ContextPolicy.State = "disabled"
	c.ConfigRefs = activity.ConfigRefs{State: "known", Refs: []string{}}
	c.Budget = activity.Budget{MaxRequests: activity.Limit{State: "known", Value: 8}, MaxSeconds: activity.Limit{State: "known", Value: 480}}
	intent := projectV2Fixture().Intent
	return OriginalProjectionInput{Namespace: "0123456789abcdef0123456789abcdef", UnitRef: intent.ReferencePreparation.RecordRef[:32], Intent: intent,
		Scope:        OriginalScopeFields{DeclaredAt: "2026-10-24T10:00:00Z", Plan: OriginalPlanIDs{strings.Repeat("3", 32), strings.Repeat("4", 32)}, BaseSnapshots: []string{strings.Repeat("2", 32)}, FamilyRef: strings.Repeat("5", 32), Configuration: c},
		StartBinding: OriginalStartBindingInput{"sha256:" + strings.Repeat("c", 64), strings.Repeat("1", 32), strings.Repeat("2", 32), "sha256:" + strings.Repeat("d", 64)}}
}

// Independently computed Python stdlib HMAC + canonical ASCII JSON SHA256 vectors.
// All input identities and keys are synthetic, not loaded producer facts.
var originalLiteralSeals = [2][6]string{
	{"sha256:8184cac4b140d76cb6ab7ac5540b123b0fcc2f5f2e0fd33fd718048ec02c6aea", "sha256:655b0e16ca9aefb6f1c30367c570db11b29a744cb29b16efbc0b1ebee842e2e5", "sha256:9f08878b841ba99ce7415bd4a191997457845e51a372431a8657f1245c01f5d7", "sha256:4f42710c3638037053fed5385b9037c82a7b5eb05fd3fe65fb96bc4e8bd3f1a5", "sha256:dce4f83318c03258b8bda52905328258e87cdc97ae73b6db5bd3862c41187972", "sha256:1cb25b9c21452837b54f2258ec078fdc288ff8a8c93ef232291e025dd49c44cf"},
	{"sha256:8184cac4b140d76cb6ab7ac5540b123b0fcc2f5f2e0fd33fd718048ec02c6aea", "sha256:655b0e16ca9aefb6f1c30367c570db11b29a744cb29b16efbc0b1ebee842e2e5", "sha256:9f08878b841ba99ce7415bd4a191997457845e51a372431a8657f1245c01f5d7", "sha256:b9bd2b024920c15ac8d84f5a72868f8aa5f5f744cc6fdeef25d682a9e3068bea", "sha256:afb6c2fe10c61ff09f5a358968e95d15afa8cc610198b1adf0952113bd7e07e5", "sha256:ade7ba0fa6dd8f296a94072e7a2e923a7d2f8641d604bf0dedec938dc425b14b"},
}
var oldSchemaHashes = []string{"45919494c3ddda2cbf8960ee41a87911555eb486d6b7132ac17aa0ff6b1da9c6", "b9be9d55735d7443ca0b0db20f7c96f175d13b6db1705a2bea81af442a0128c3", "b154d46a95d88f4fb28158fbf5269f0580c6deb4a73a2e5f47e013e94779e020", "a171d2ac132cceb08318f9cd11d38b5a28ef0f67397d1053aeae95862cc11ac1"}

func originalSeals(p *OriginalProjection) [6]string {
	n, f := p.Native(), p.Final()
	return [6]string{n.IntentDigest, n.ScopeDigest, n.RevisionDigest, f.IntentDigest, f.ScopeDigest, f.RevisionDigest}
}
func TestOriginalProjectionIndependentSealsAndConfiguration(t *testing.T) {
	input := originalProjectionFixture()
	before, _ := json.Marshal(input)
	var native CanonicalOriginal
	for n := 0; n < 2; n++ {
		key := projectV2Key(t, n)
		keyBefore := bytes.Clone(key)
		p, e := ProjectOriginal(key, input)
		if e != nil {
			t.Fatal(e)
		}
		if originalSeals(p) != originalLiteralSeals[n] {
			t.Fatalf("literal seal mismatch key%d: %v", n, originalSeals(p))
		}
		if n == 0 {
			native = p.Native()
		} else if !bytes.Equal(native.Scope, p.Native().Scope) {
			t.Fatal("key changed native scope")
		}
		metadata := p.Metadata()
		if metadata.Namespace != input.Namespace || metadata.UnitRef != input.UnitRef || metadata.ConfigurationDigest != input.Scope.Configuration.Digest() || metadata.Policy.Configuration != "canonical-activity-configuration-preserved" {
			t.Fatal("context lost")
		}
		cfg, _ := hint.Canonical(input.Scope.Configuration)
		for _, v := range []CanonicalOriginal{p.Native(), p.Final()} {
			var sc map[string]json.RawMessage
			if json.Unmarshal(v.Scope, &sc) != nil {
				t.Fatal("scope JSON")
			}
			if !bytes.Equal(sc["configuration"], cfg) {
				t.Fatal("configuration changed during projection")
			}
			var intent any
			json.Unmarshal(v.Intent, &intent)
			d, _ := hint.Digest(intent)
			if d != v.IntentDigest {
				t.Fatal("intent seal")
			}
			var scope any
			json.Unmarshal(v.Scope, &scope)
			d, _ = hint.Digest(scope)
			if d != v.ScopeDigest {
				t.Fatal("scope seal")
			}
		}
		final := p.Final()
		for _, d := range projectV2VectorDigests[:9] {
			if bytes.Contains(final.Revision, []byte(d)) {
				t.Fatal("private content commitment escaped")
			}
		}
		if !bytes.Contains(final.Revision, []byte(input.StartBinding.RecommendationSetDigest)) || !bytes.Contains(final.Revision, []byte(input.StartBinding.BaseSnapshotDigest)) {
			t.Fatal("canonical HINT commitments were replaced")
		}
		if bytes.Equal(p.Native().Intent, p.Final().Intent) || p.Native().ScopeDigest == p.Final().ScopeDigest || p.Native().RevisionDigest == p.Final().RevisionDigest {
			t.Fatal("native/projected seals collapsed")
		}
		if !bytes.Equal(key, keyBefore) {
			t.Fatal("key modified")
		}
	}
	after, _ := json.Marshal(input)
	if !bytes.Equal(before, after) {
		t.Fatal("native input mutated")
	}
}
func TestOriginalProjectionCopiesAndNoWireAuthority(t *testing.T) {
	in := originalProjectionFixture()
	p, e := ProjectOriginal(projectV2Key(t, 0), in)
	if e != nil {
		t.Fatal(e)
	}
	want := p.Final()
	n := p.Native()
	f := p.Final()
	for _, b := range [][]byte{n.Intent, n.Scope, n.Revision, f.Intent, f.Scope, f.Revision} {
		b[0] = '!'
	}
	if !bytes.Equal(p.Final().Revision, want.Revision) {
		t.Fatal("getter aliases projection")
	}
	in.Scope.BaseSnapshots[0] = strings.Repeat("f", 32)
	in.Scope.Configuration.ConfigRefs.Refs = append(in.Scope.Configuration.ConfigRefs.Refs, strings.Repeat("e", 32))
	if !bytes.Equal(p.Final().Revision, want.Revision) {
		t.Fatal("input aliases projection")
	}
	if _, e := json.Marshal(p); e == nil {
		t.Fatal("projection became public wire")
	}
	if _, e := json.Marshal(p.Native()); e == nil {
		t.Fatal("native bytes became public wire")
	}
	// Final bytes are schema/consistency assertions only, never a real original writer.
	var r map[string]any
	json.Unmarshal(want.Revision, &r)
	all, e := compiled()
	if e != nil || all["0.4/revision"].Validate(r) != nil {
		t.Fatal("projected original does not match complete04 schema", e)
	}
	if e := projectProblem(map[string]any{"report_version": "0.4.9", "trials": []any{}, "work_units": []any{r}}); e != nil {
		t.Fatal(e)
	}
}
func TestOriginalProjectionRejectsBadBindingsAndBounds(t *testing.T) {
	for name, mutate := range map[string]func(*OriginalProjectionInput){
		"namespace":                   func(i *OriginalProjectionInput) { i.Namespace = "raw native namespace" },
		"unit":                        func(i *OriginalProjectionInput) { i.UnitRef = strings.Repeat("f", 32) },
		"missing_plan":                func(i *OriginalProjectionInput) { i.Scope.Plan.PlanID = "" },
		"missing_base":                func(i *OriginalProjectionInput) { i.Scope.BaseSnapshots = nil },
		"foreign_base":                func(i *OriginalProjectionInput) { i.Scope.BaseSnapshots = []string{strings.Repeat("e", 32)} },
		"minute":                      func(i *OriginalProjectionInput) { i.Scope.DeclaredAt = "2026-10-24T10:01:00Z" },
		"unknown_configuration_extra": func(i *OriginalProjectionInput) { i.Scope.Configuration.Model.State = "unknown" },
		"bad_source_record":           func(i *OriginalProjectionInput) { i.Intent.CaptureSource.RecordRef = "private/path" },
		"missing_reference_digest":    func(i *OriginalProjectionInput) { i.Intent.ReferencePreparation.RecordDigest = "" },
		"bad_canonical_set":           func(i *OriginalProjectionInput) { i.StartBinding.RecommendationSetDigest = "raw private set" },
		"too_many_snapshots":          func(i *OriginalProjectionInput) { i.Scope.BaseSnapshots = make([]string, 101) },
		"too_many_config_refs":        func(i *OriginalProjectionInput) { i.Scope.Configuration.ConfigRefs.Refs = make([]string, 101) },
		"oversize_config":             func(i *OriginalProjectionInput) { i.Scope.Configuration.Agent.Version.Value = strings.Repeat("v", 257) },
	} {
		t.Run(name, func(t *testing.T) {
			i := originalProjectionFixture()
			mutate(&i)
			p, e := ProjectOriginal(projectV2Key(t, 0), i)
			if e != ErrOriginalProjection || p != nil {
				t.Fatal("invalid projection returned partial success", p, e)
			}
		})
	}
	for _, key := range [][]byte{nil, make([]byte, 31), make([]byte, 32), make([]byte, 33)} {
		if p, e := ProjectOriginal(key, originalProjectionFixture()); e != ErrOriginalProjection || p != nil {
			t.Fatal("invalid key")
		}
	}
}
func TestOriginalProjectionChangesOnlyCorrespondingIntentAndSeals(t *testing.T) {
	in := originalProjectionFixture()
	a, e := ProjectOriginal(projectV2Key(t, 0), in)
	if e != nil {
		t.Fatal(e)
	}
	in.Intent.Spec = "sha256:" + strings.Repeat("f", 64)
	b, e := ProjectOriginal(projectV2Key(t, 0), in)
	if e != nil {
		t.Fatal(e)
	}
	if a.Final().ScopeDigest == b.Final().ScopeDigest || a.Final().RevisionDigest == b.Final().RevisionDigest {
		t.Fatal("changed private input did not change seals")
	}
	var x, y map[string]any
	json.Unmarshal(a.Final().Intent, &x)
	json.Unmarshal(b.Final().Intent, &y)
	for k, v := range x {
		if (k == "spec_ref") == reflect.DeepEqual(v, y[k]) {
			t.Fatal("wrong intent correspondence", k)
		}
	}
}
func TestProjectV04LegacySchemaBytesFrozen(t *testing.T) {
	for i, raw := range [][]byte{githint.SchemaEvalReportV01, githint.SchemaEvalReportV02, githint.SchemaEvalReportV03, githint.SchemaEvalReportV031} {
		h := sha256.Sum256(raw)
		if hex.EncodeToString(h[:]) != oldSchemaHashes[i] {
			t.Fatal("legacy schema changed", i)
		}
	}
}

func TestOriginalProjectionPreservesDeclaredConfigurationBytes(t *testing.T) {
	in := originalProjectionFixture()
	in.Scope.Configuration.Declared = &activity.Declared{Model: &activity.RefState{State: "known", Ref: strings.Repeat("f", 32)}}
	p, e := ProjectOriginal(projectV2Key(t, 0), in)
	if e != nil {
		t.Fatal(e)
	}
	cfg, _ := hint.Canonical(in.Scope.Configuration)
	for _, scope := range [][]byte{p.Native().Scope, p.Final().Scope} {
		var v map[string]json.RawMessage
		if json.Unmarshal(scope, &v) != nil || !bytes.Equal(v["configuration"], cfg) {
			t.Fatal("declared configuration changed")
		}
	}
	in.Scope.Configuration.Declared.Model.Ref = strings.Repeat("e", 32)
	var stored map[string]json.RawMessage
	json.Unmarshal(p.Final().Scope, &stored)
	if !bytes.Equal(stored["configuration"], cfg) {
		t.Fatal("declared pointer aliased projection")
	}
}
