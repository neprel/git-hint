package report

import "github.com/neprel/git-hint/internal/activity"

// These are closed v0.4 receipt assertion types, not producer/observer authority.
// Legacy v0.3 types retain their exact fields and meaning.
type ProjectQualificationSourceV2 struct {
	Domain string `json:"domain"`
	Policy string `json:"policy"`
}
type ProjectReferenceInputV2 struct {
	Domain string `json:"domain"`
	Ref    string `json:"ref"`
}
type ProjectStartBindingV2 struct {
	RecommendationSetDigest string `json:"recommendation_set_digest"`
	RepoID                  string `json:"repo_id"`
	BaseSnapshotID          string `json:"base_snapshot_id"`
	BaseSnapshotDigest      string `json:"base_snapshot_digest"`
	BaseHintTreeRef         string `json:"base_hint_tree_ref"`
}

// OriginalStartBindingInput contains only canonical HINT plan/snapshot values.
// The private tree commitment enters only through Intent.SelectedStartTree.
type OriginalStartBindingInput struct {
	RecommendationSetDigest string `json:"recommendation_set_digest"`
	RepoID                  string `json:"repo_id"`
	BaseSnapshotID          string `json:"base_snapshot_id"`
	BaseSnapshotDigest      string `json:"base_snapshot_digest"`
}
type VerificationIntentV2 struct {
	IntentVersion                  string                       `json:"intent_version"`
	DefinitionRef                  string                       `json:"definition_ref"`
	DefinitionVersionRef           string                       `json:"definition_version_ref"`
	QualificationSource            ProjectQualificationSourceV2 `json:"qualification_source"`
	SpecRef                        string                       `json:"spec_ref"`
	InputsRef                      string                       `json:"inputs_ref"`
	ReferenceInput                 ProjectReferenceInputV2      `json:"reference_input"`
	ReferenceArtifactRef           string                       `json:"reference_artifact_ref"`
	ReferencePreparationRef        string                       `json:"reference_preparation_ref"`
	ReferencePreparationVersionRef string                       `json:"reference_preparation_version_ref"`
	CapturePolicyRef               string                       `json:"capture_policy_ref"`
	CapturePolicyVersionRef        string                       `json:"capture_policy_version_ref"`
	CaptureSourceRef               string                       `json:"capture_source_ref"`
	CaptureSourceVersionRef        string                       `json:"capture_source_version_ref"`
	MandatoryChecksDigest          string                       `json:"mandatory_checks_digest"`
	StartBinding                   ProjectStartBindingV2        `json:"start_binding"`
}
type ProjectVerificationResultV2 struct {
	SnapshotID     string `json:"snapshot_id"`
	SnapshotDigest string `json:"snapshot_digest"`
	// Exact FULL actual B1 candidate frozen-final-TAR commitment under the existing role;
	// not the Git-selected tree or structural snapshot.
	ArtifactRef string `json:"artifact_ref"`
}
type ProjectVerifierV2 struct {
	VerificationIntentV2
	MandatoryChecks          []string `json:"mandatory_checks"`
	EvaluatorImageRef        string   `json:"evaluator_image_ref"`
	RunnerName               string   `json:"runner_name"`
	RunnerVersion            string   `json:"runner_version"`
	Interface                string   `json:"interface"`
	QualificationState       string   `json:"qualification_state"`
	QualificationResultTrust string   `json:"qualification_result_trust"`
}
type ProjectVerificationOutputsV2 struct {
	CandidateOutputsRef string `json:"candidate_outputs_ref"`
	OracleOutputsRef    string `json:"oracle_outputs_ref"`
	EvaluatorResultRef  string `json:"evaluator_result_ref"`
}
type ProjectVerificationV2 struct {
	VerificationRef          string                       `json:"verification_ref"`
	ContentDigest            string                       `json:"content_digest"`
	Work                     ProjectVerificationWork      `json:"work"`
	Plan                     ProjectVerificationPlan      `json:"plan"`
	Result                   ProjectVerificationResultV2  `json:"result"`
	VerificationExecutionRef string                       `json:"verification_execution_ref"`
	Verifier                 ProjectVerifierV2            `json:"verifier"`
	Verdict                  ProjectVerificationVerdict   `json:"verdict"`
	Outputs                  ProjectVerificationOutputsV2 `json:"outputs"`
}

// OriginalScopeFields are the existing ordinary project scope fields. Configuration
// is already canonical/keyed and remains byte-identical through this pure projection.
// There is no independently supplied final configuration or raw model-name input.
type OriginalScopeFields struct {
	DeclaredAt      string                 `json:"declared_at"`
	Plan            OriginalPlanIDs        `json:"plan"`
	ObjectiveRef    *string                `json:"objective_ref"`
	BaseSnapshots   []string               `json:"base_snapshots"`
	FamilyRef       string                 `json:"family_ref"`
	Configuration   activity.Configuration `json:"configuration"`
	EnvironmentRef  *string                `json:"environment_ref"`
	RunRef          *string                `json:"run_ref"`
	EvalRef         *string                `json:"eval_ref"`
	BaselineRef     *string                `json:"baseline_ref"`
	Arm             *string                `json:"arm"`
	PlannedAttempts *int                   `json:"planned_attempts"`
}
type OriginalPlanIDs struct {
	RecommendationSetID string `json:"recommendation_set_id"`
	PlanID              string `json:"plan_id"`
}
