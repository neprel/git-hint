package report

import (
	"errors"

	"github.com/neprel/git-hint/internal/hint"
)

// Local privacy inputs are not report/wire types. Record lexical checks do not
// prove producer origin. In particular, capture producers remain missing. A
// future writer must resolve genuine typed records before this pure mapping.
type ProjectV2RecordVersion struct{ RecordRef, RecordDigest string }
type ProjectV2IntentPrivacyInput struct {
	Definition, ReferencePreparation                          ProjectV2RecordVersion
	CapturePolicy, CaptureSource                              ProjectV2RecordVersion
	Spec, Inputs, ReferencePatchArchive, PreparedReferenceTAR string
	SelectedStartTree                                         string
}
type ProjectV2VerificationPrivacyInput struct {
	ResultArtifact, EvaluatorImage                   string
	CandidateOutputs, OracleOutputs, EvaluatorResult string
}
type ProjectV2LocalPrivacyInput struct {
	Intent       ProjectV2IntentPrivacyInput
	Verification *ProjectV2VerificationPrivacyInput
}
type ProjectV2IntentPrivacyRefs struct {
	DefinitionVersionRef, ReferencePreparationVersionRef        string
	CapturePolicyVersionRef, CaptureSourceVersionRef            string
	SpecRef, InputsRef, ReferenceInputRef, ReferenceArtifactRef string
	BaseHintTreeRef                                             string
}
type ProjectV2VerificationPrivacyRefs struct {
	ArtifactRef, EvaluatorImageRef                            string
	CandidateOutputsRef, OracleOutputsRef, EvaluatorResultRef string
}
type ProjectV2PrivacyRefs struct {
	Intent       ProjectV2IntentPrivacyRefs
	Verification *ProjectV2VerificationPrivacyRefs
}

var (
	ErrProjectV2PrivacyKey    = errors.New("project_v2_privacy_key_invalid")
	ErrProjectV2PrivacyRecord = errors.New("project_v2_privacy_record_invalid")
	ErrProjectV2PrivacyDigest = errors.New("project_v2_privacy_digest_invalid")
)

func projectV2LowerHex(v string, size int) bool {
	if len(v) != size {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func projectV2PrivateDigest(v string) bool {
	return len(v) == 71 && v[:7] == "sha256:" && projectV2LowerHex(v[7:], 64)
}
func projectV2PreparationLocator(v string) bool {
	// Exact existing local outcome locator, including the keyed unit component.
	// No enclosing unit is an input here; equality belongs to its actual producer.
	return len(v) == 73 && v[32] == '.' && v[65:] == ".outcome" && projectV2LowerHex(v[:32], 32) && projectV2LowerHex(v[33:65], 32)
}

// RenderProjectV2Privacy maps the complete fixed private commitment set to keyed
// references. It does not load records, validate actual producer domains or grant
// source/start/completion authority. Canonical HINT snapshot/set digests and all
// existing identifiers remain outside this helper and retain their normal joins.
// Every error returns the zero result; no raw input appears in an error.
func RenderProjectV2Privacy(key []byte, in ProjectV2LocalPrivacyInput) (ProjectV2PrivacyRefs, error) {
	var zero ProjectV2PrivacyRefs
	if len(key) != 32 {
		return zero, ErrProjectV2PrivacyKey
	}
	var owned [32]byte
	copy(owned[:], key)
	var nonzero byte
	for _, b := range owned {
		nonzero |= b
	}
	if nonzero == 0 {
		return zero, ErrProjectV2PrivacyKey
	}
	i := in.Intent
	var v ProjectV2VerificationPrivacyInput
	if in.Verification != nil {
		v = *in.Verification
	}
	if !projectV2LowerHex(i.Definition.RecordRef, 32) || !projectV2LowerHex(i.CapturePolicy.RecordRef, 32) || !projectV2LowerHex(i.CaptureSource.RecordRef, 32) || !projectV2PreparationLocator(i.ReferencePreparation.RecordRef) {
		return zero, ErrProjectV2PrivacyRecord
	}
	for _, d := range [9]string{i.Definition.RecordDigest, i.ReferencePreparation.RecordDigest, i.CapturePolicy.RecordDigest, i.CaptureSource.RecordDigest, i.Spec, i.Inputs, i.ReferencePatchArchive, i.PreparedReferenceTAR, i.SelectedStartTree} {
		if !projectV2PrivateDigest(d) {
			return zero, ErrProjectV2PrivacyDigest
		}
	}
	if in.Verification != nil {
		for _, d := range [5]string{v.ResultArtifact, v.EvaluatorImage, v.CandidateOutputs, v.OracleOutputs, v.EvaluatorResult} {
			if !projectV2PrivateDigest(d) {
				return zero, ErrProjectV2PrivacyDigest
			}
		}
	}
	// Fixed literal roles only. Each composite locator remains ONE EvalRef part.
	out := ProjectV2PrivacyRefs{Intent: ProjectV2IntentPrivacyRefs{
		DefinitionVersionRef:           hint.EvalRef(owned[:], "project-v2-definition-version", i.Definition.RecordRef, i.Definition.RecordDigest),
		ReferencePreparationVersionRef: hint.EvalRef(owned[:], "project-v2-reference-preparation-version", i.ReferencePreparation.RecordRef, i.ReferencePreparation.RecordDigest),
		CapturePolicyVersionRef:        hint.EvalRef(owned[:], "project-v2-capture-policy-version", i.CapturePolicy.RecordRef, i.CapturePolicy.RecordDigest),
		CaptureSourceVersionRef:        hint.EvalRef(owned[:], "project-v2-capture-source-version", i.CaptureSource.RecordRef, i.CaptureSource.RecordDigest),
		SpecRef:                        hint.EvalRef(owned[:], "project-v2-io-spec", i.Spec),
		InputsRef:                      hint.EvalRef(owned[:], "project-v2-io-inputs", i.Inputs),
		ReferenceInputRef:              hint.EvalRef(owned[:], "project-v2-reference-patch-archive", i.ReferencePatchArchive),
		ReferenceArtifactRef:           hint.EvalRef(owned[:], "project-v2-prepared-reference-tar", i.PreparedReferenceTAR),
		BaseHintTreeRef:                hint.EvalRef(owned[:], "project-v2-selected-start-tree", i.SelectedStartTree),
	}}
	if in.Verification != nil {
		out.Verification = &ProjectV2VerificationPrivacyRefs{
			ArtifactRef:         hint.EvalRef(owned[:], "project-v2-selected-result-artifact", v.ResultArtifact),
			EvaluatorImageRef:   hint.EvalRef(owned[:], "project-v2-evaluator-image", v.EvaluatorImage),
			CandidateOutputsRef: hint.EvalRef(owned[:], "project-v2-candidate-outputs", v.CandidateOutputs),
			OracleOutputsRef:    hint.EvalRef(owned[:], "project-v2-oracle-outputs", v.OracleOutputs),
			EvaluatorResultRef:  hint.EvalRef(owned[:], "project-v2-evaluator-result", v.EvaluatorResult),
		}
	}
	return out, nil
}
