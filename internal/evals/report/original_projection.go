package report

// Pure local correspondence only. No Store, record, original-start witness,
// independent issuer or normal report writer is created by this serializer.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/hint"
)

const OriginalProjectionVersion = "report04-original-projection-v1"

var ErrOriginalProjection = errors.New("original_projection_invalid")

type OriginalProjectionInput struct {
	Namespace, UnitRef string
	Scope              OriginalScopeFields
	Intent             ProjectV2IntentPrivacyInput
	// Canonical HINT plan/snapshot bindings, never raw content/tree digests.
	StartBinding OriginalStartBindingInput
}

// CanonicalOriginal keeps local bytes and exact seals. Native bytes include private
// commitments. Only the Final result contains the closed report04 wire projection.
type CanonicalOriginal struct {
	Intent, Scope, Revision                   []byte
	IntentDigest, ScopeDigest, RevisionDigest string
}

func (v CanonicalOriginal) copy() CanonicalOriginal {
	v.Intent = bytes.Clone(v.Intent)
	v.Scope = bytes.Clone(v.Scope)
	v.Revision = bytes.Clone(v.Revision)
	return v
}
func (CanonicalOriginal) MarshalJSON() ([]byte, error) { return nil, ErrOriginalProjection }

type OriginalProjectionPolicy struct {
	Version, ReportMinor, Configuration, SchemaDigest string
}
type OriginalProjectionMetadata struct {
	Namespace, UnitRef  string
	Policy              OriginalProjectionPolicy
	ConfigurationDigest string
}
type OriginalProjection struct {
	native, final CanonicalOriginal
	metadata      OriginalProjectionMetadata
}

func (p *OriginalProjection) Native() CanonicalOriginal {
	if p == nil {
		return CanonicalOriginal{}
	}
	return p.native.copy()
}
func (p *OriginalProjection) Final() CanonicalOriginal {
	if p == nil {
		return CanonicalOriginal{}
	}
	return p.final.copy()
}
func (p *OriginalProjection) Metadata() OriginalProjectionMetadata {
	if p == nil {
		return OriginalProjectionMetadata{}
	}
	return p.metadata
}
func (*OriginalProjection) MarshalJSON() ([]byte, error) { return nil, ErrOriginalProjection }

// ProjectOriginal is the sole neutral original intent/scope/revision projection.
// Typed inputs/lexical checks cannot prove origin. A future project adapter must
// resolve actual records, key/store/namespace and current plan/config binding first.
func ProjectOriginal(key []byte, in OriginalProjectionInput) (*OriginalProjection, error) {
	if !projectV2LowerHex(in.Namespace, 32) || !projectV2LowerHex(in.UnitRef, 32) || !originalInputBounded(in.Scope) {
		return nil, ErrOriginalProjection
	}
	if !projectV2PrivateDigest(in.StartBinding.RecommendationSetDigest) || !projectV2PrivateDigest(in.StartBinding.BaseSnapshotDigest) || !projectV2LowerHex(in.StartBinding.RepoID, 32) || !projectV2LowerHex(in.StartBinding.BaseSnapshotID, 32) {
		return nil, ErrOriginalProjection
	}
	refs, e := RenderProjectV2Privacy(key, ProjectV2LocalPrivacyInput{Intent: in.Intent})
	if e != nil {
		return nil, ErrOriginalProjection
	}
	if !projectV2PreparationLocator(in.Intent.ReferencePreparation.RecordRef) || in.Intent.ReferencePreparation.RecordRef[:32] != in.UnitRef {
		return nil, ErrOriginalProjection
	}
	// No caller-supplied final tree reference can override the sole renderer.
	start := ProjectStartBindingV2{in.StartBinding.RecommendationSetDigest, in.StartBinding.RepoID, in.StartBinding.BaseSnapshotID, in.StartBinding.BaseSnapshotDigest, refs.Intent.BaseHintTreeRef}
	mandatory, e := hint.Digest([]string{"B1"})
	if e != nil {
		return nil, ErrOriginalProjection
	}
	wire := VerificationIntentV2{IntentVersion: "project-verification-intent-v2", DefinitionRef: in.Intent.Definition.RecordRef, DefinitionVersionRef: refs.Intent.DefinitionVersionRef,
		QualificationSource: ProjectQualificationSourceV2{"hint-work-definition-qualified-inputs-v1", "q3"}, SpecRef: refs.Intent.SpecRef, InputsRef: refs.Intent.InputsRef,
		ReferenceInput: ProjectReferenceInputV2{"hint-library-reference-patch-archive-v1", refs.Intent.ReferenceInputRef}, ReferenceArtifactRef: refs.Intent.ReferenceArtifactRef,
		ReferencePreparationRef: in.Intent.ReferencePreparation.RecordRef, ReferencePreparationVersionRef: refs.Intent.ReferencePreparationVersionRef,
		CapturePolicyRef: in.Intent.CapturePolicy.RecordRef, CapturePolicyVersionRef: refs.Intent.CapturePolicyVersionRef, CaptureSourceRef: in.Intent.CaptureSource.RecordRef, CaptureSourceVersionRef: refs.Intent.CaptureSourceVersionRef, MandatoryChecksDigest: mandatory, StartBinding: start}
	scope, e := originalScopeMap(in.Scope)
	if e != nil {
		return nil, ErrOriginalProjection
	}
	final, e := sealOriginal(in.UnitRef, scope, wire)
	if e != nil {
		return nil, ErrOriginalProjection
	}
	// Compile and use the SAME schema resource/semantic core as Check, not a second parser.
	all, e := compiled()
	if e != nil {
		return nil, ErrOriginalProjection
	}
	var revision map[string]any
	if json.Unmarshal(final.Revision, &revision) != nil || all["0.4/revision"].Validate(revision) != nil {
		return nil, ErrOriginalProjection
	}
	if projectProblem(map[string]any{"report_version": "0.4.0", "trials": []any{}, "work_units": []any{revision}}) != nil {
		return nil, ErrOriginalProjection
	}
	// Native form is explicitly private, not legacy030/031 or an emitted record.
	nativeIntent := map[string]any{"kind": "project-original-native-intent-v1", "definition": in.Intent.Definition, "reference_preparation": in.Intent.ReferencePreparation,
		"capture_policy": in.Intent.CapturePolicy, "capture_source": in.Intent.CaptureSource, "spec": in.Intent.Spec, "inputs": in.Intent.Inputs, "reference_patch_archive": in.Intent.ReferencePatchArchive,
		"prepared_reference_tar": in.Intent.PreparedReferenceTAR, "selected_start_tree": in.Intent.SelectedStartTree, "start_binding": in.StartBinding, "mandatory_checks_digest": mandatory}
	nativeScope, e := originalScopeMap(in.Scope)
	if e != nil {
		return nil, ErrOriginalProjection
	}
	native, e := sealOriginal(in.UnitRef, nativeScope, nativeIntent)
	if e != nil {
		return nil, ErrOriginalProjection
	}
	h := sha256.Sum256(githint.SchemaEvalReportV04)
	return &OriginalProjection{native: native, final: final, metadata: OriginalProjectionMetadata{Namespace: in.Namespace, UnitRef: in.UnitRef, ConfigurationDigest: in.Scope.Configuration.Digest(), Policy: OriginalProjectionPolicy{Version: OriginalProjectionVersion, ReportMinor: "0.4", Configuration: "canonical-activity-configuration-preserved", SchemaDigest: "sha256:" + hex.EncodeToString(h[:])}}}, nil
}
func originalScopeMap(v OriginalScopeFields) (map[string]any, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var m map[string]any
	e = json.Unmarshal(b, &m)
	return m, e
}
func sealOriginal(unit string, scope map[string]any, intent any) (CanonicalOriginal, error) {
	var out CanonicalOriginal
	var e error
	out.Intent, e = hint.Canonical(intent)
	if e != nil {
		return out, e
	}
	out.IntentDigest, e = hint.Digest(intent)
	if e != nil {
		return out, e
	}
	scope["verification_intent"] = intent
	out.Scope, e = hint.Canonical(scope)
	if e != nil {
		return out, e
	}
	out.ScopeDigest, e = hint.Digest(scope)
	if e != nil {
		return out, e
	}
	r := map[string]any{"unit_ref": unit, "origin": "project", "scope": scope, "scope_digest": out.ScopeDigest, "revision": 1, "predecessor": nil, "state": "open", "attempts": []any{}, "declared_by": "user"}
	out.RevisionDigest, e = hint.Digest(r)
	if e != nil {
		return out, e
	}
	r["revision_digest"] = out.RevisionDigest
	out.Revision, e = hint.Canonical(r)
	return out, e
}

// These local projection bounds narrow before JSON allocation. Schema validation
// remains the sole semantic validator. They do not change the report accepted set.
func originalInputBounded(s OriginalScopeFields) bool {
	if len(s.BaseSnapshots) > 100 || len(s.Configuration.ConfigRefs.Refs) > 100 || len(s.DeclaredAt) > 32 {
		return false
	}
	fixed := []string{s.Plan.RecommendationSetID, s.Plan.PlanID, s.FamilyRef}
	fixed = append(fixed, s.BaseSnapshots...)
	for _, p := range []*string{s.ObjectiveRef, s.EnvironmentRef, s.RunRef, s.EvalRef, s.BaselineRef, s.Arm} {
		if p != nil {
			fixed = append(fixed, *p)
		}
	}
	for _, v := range fixed {
		if len(v) > 32 {
			return false
		}
	}
	c := s.Configuration
	texts := []string{c.Agent.Name, c.Agent.Version.State, c.Agent.Version.Value, c.Model.State, c.Model.Ref, c.Model.Provenance, c.Harness.Kind, c.Harness.Version.State, c.Harness.Version.Value, c.ContextPolicy.State, c.ContextPolicy.Ref, c.ConfigRefs.State, c.Budget.MaxRequests.State, c.Budget.MaxSeconds.State}
	texts = append(texts, c.ConfigRefs.Refs...)
	if c.Declared != nil && c.Declared.Model != nil {
		texts = append(texts, c.Declared.Model.State, c.Declared.Model.Ref)
	}
	for _, v := range texts {
		if len(v) > 256 {
			return false
		}
	}
	return true
}
