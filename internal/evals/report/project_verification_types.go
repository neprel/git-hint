package report

// VerificationIntent freezes qualified policy identities in the original project scope.
// These transport types are receipt assertions, not a trusted runner projection.
// Builder intentionally has no 0.3 emission path.
// New fields are required by 0.3.1 schema only; omission preserves legacy 0.3.0 bytes.
// A typed value never establishes source/runtime authority.
type VerificationIntent struct {
	IntentVersion              string                 `json:"intent_version,omitempty"`
	ReferenceInput             *ProjectReferenceInput `json:"reference_input,omitempty"`
	ReferencePreparationRef    string                 `json:"reference_preparation_ref,omitempty"`
	ReferencePreparationDigest string                 `json:"reference_preparation_digest,omitempty"`
	CapturePolicyRef           string                 `json:"capture_policy_ref,omitempty"`
	CapturePolicyDigest        string                 `json:"capture_policy_digest,omitempty"`
	CaptureSourceRef           string                 `json:"capture_source_ref,omitempty"`
	CaptureSourceDigest        string                 `json:"capture_source_digest,omitempty"`
	StartBinding               *ProjectStartBinding   `json:"start_binding,omitempty"`
	DefinitionRef              string                 `json:"definition_ref"`
	DefinitionDigest           string                 `json:"definition_digest"`
	QualificationRef           string                 `json:"qualification_ref"`
	SpecDigest                 string                 `json:"spec_digest"`
	InputsDigest               string                 `json:"inputs_digest"`
	ReferenceArtifactDigest    string                 `json:"reference_artifact_digest"`
	MandatoryChecksDigest      string                 `json:"mandatory_checks_digest"`
}

type ProjectVerification struct {
	VerificationRef          string                     `json:"verification_ref"`
	ContentDigest            string                     `json:"content_digest"`
	Work                     ProjectVerificationWork    `json:"work"`
	Plan                     ProjectVerificationPlan    `json:"plan"`
	Result                   ProjectVerificationResult  `json:"result"`
	VerificationExecutionRef string                     `json:"verification_execution_ref"`
	Verifier                 ProjectVerifier            `json:"verifier"`
	Verdict                  ProjectVerificationVerdict `json:"verdict"`
	Outputs                  ProjectVerificationOutputs `json:"outputs"`
}
type ProjectVerificationWork struct {
	UnitRef                  string `json:"unit_ref"`
	AttemptRef               string `json:"attempt_ref"`
	StartScopeDigest         string `json:"start_scope_digest"`
	VerificationIntentDigest string `json:"verification_intent_digest"`
	FrozenOpenRevisionDigest string `json:"frozen_open_revision_digest"`
}
type ProjectVerificationPlan struct {
	RecommendationSetID     string `json:"recommendation_set_id"`
	RecommendationSetDigest string `json:"recommendation_set_digest"`
	PlanID                  string `json:"plan_id"`
}
type ProjectVerificationResult struct {
	SnapshotID     string `json:"snapshot_id"`
	SnapshotDigest string `json:"snapshot_digest"`
	ArtifactDigest string `json:"artifact_digest"`
}
type ProjectVerifier struct {
	VerificationIntent
	MandatoryChecks          []string `json:"mandatory_checks"`
	EvaluatorImageDigest     string   `json:"evaluator_image_digest"`
	RunnerName               string   `json:"runner_name"`
	RunnerVersion            string   `json:"runner_version"`
	Interface                string   `json:"interface"`
	QualificationState       string   `json:"qualification_state"`
	QualificationResultTrust string   `json:"qualification_result_trust"`
}
type ProjectVerificationCheck struct {
	CheckID     string `json:"check_id"`
	Status      string `json:"status"`
	ResultTrust string `json:"result_trust"`
}
type ProjectVerificationVerdict struct {
	Outcome     string                     `json:"outcome"`
	Checks      []ProjectVerificationCheck `json:"checks"`
	ResultTrust string                     `json:"result_trust"`
}
type ProjectVerificationOutputs struct {
	CandidateOutputsDigest string `json:"candidate_outputs_digest"`
	OracleOutputsDigest    string `json:"oracle_outputs_digest"`
	EvaluatorResultDigest  string `json:"evaluator_result_digest"`
}

// ProjectReferenceInput identifies input bytes, never a prepared final artifact.
type ProjectReferenceInput struct {
	Domain string `json:"domain"`
	Digest string `json:"digest"`
}

// ProjectStartBinding records prospective plan/base assertions. Resolution of
// original bytes, source location and actual preparation remains a separate gate.
type ProjectStartBinding struct {
	RecommendationSetDigest string `json:"recommendation_set_digest"`
	RepoID                  string `json:"repo_id"`
	BaseSnapshotID          string `json:"base_snapshot_id"`
	BaseSnapshotDigest      string `json:"base_snapshot_digest"`
	BaseHintTreeDigest      string `json:"base_hint_tree_digest"`
}
