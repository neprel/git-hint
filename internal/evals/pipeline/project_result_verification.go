package pipeline

// Local factual linkage only. No wire, completion issuer or trust activation.
import (
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
	"reflect"
	"time"
)

type ObservedProjectResult struct {
	input    *RetainedTaskInput
	artifact *runner.TaskResultArtifact
}

func (*ObservedProjectResult) MarshalJSON() ([]byte, error) { return nil, runner.ErrTaskResult }

type SelectedProjectResult struct {
	mapping  *SelectedProjectMapping
	input    *RetainedTaskInput
	artifact *runner.TaskResultArtifact
}

func (*SelectedProjectResult) MarshalJSON() ([]byte, error) { return nil, runner.ErrTaskResult }
func CheckSelectedProjectResult(ctx context.Context, m *SelectedProjectMapping, v *ObservedProjectResult) (*SelectedProjectResult, error) {
	if ctx == nil || ctx.Err() != nil || m == nil || m.input == nil || v == nil || v.input != m.input || !v.artifact.MatchesPrelaunch(v.input.occurrence) {
		return nil, runner.ErrTaskResult
	}
	id := v.artifact.Identity()
	if !reflect.DeepEqual(id.Inputs, v.input.frozen.Identity()) || id.Capture.Digest != m.identity.Capture.Digest || id.Capture.ExecutionDigest != m.identity.Capture.ExecutionDigest {
		return nil, runner.ErrTaskResult
	}
	return &SelectedProjectResult{m, v.input, v.artifact}, nil
}
func (v *SelectedProjectResult) Ready() bool {
	return v != nil && v.input != nil && v.mapping != nil && v.mapping.input == v.input && v.artifact.Ready() && v.artifact.MatchesPrelaunch(v.input.occurrence)
}
func (v *SelectedProjectResult) Identity() (SelectedMappingIdentity, runner.TaskResultIdentity) {
	if v == nil {
		return SelectedMappingIdentity{}, runner.TaskResultIdentity{}
	}
	return v.mapping.Identity(), v.artifact.Identity()
}
func (v *SelectedProjectResult) ResolveSelectedInputs(ctx context.Context, limits trees.Limits) (*project.SelectedResultInputs, error) {
	if !v.Ready() {
		return nil, runner.ErrTaskResult
	}
	m := v.mapping.Identity()
	return project.ResolveSelectedResultInputs(ctx, v.input.store, project.SelectedResultRequest{RepoID: m.Baseline.RepoID, Definition: m.Definition, Baseline: m.Baseline.Observation, Source: m.Source, Result: v.artifact, Limits: limits})
}

type SelectedProjectVerification struct {
	result      *SelectedProjectResult
	prepared    project.PreparedReferenceIdentity
	selected    *project.SelectedResultInputs
	observation *runner.FrozenIOObservation
}

func (*SelectedProjectVerification) MarshalJSON() ([]byte, error) { return nil, runner.ErrTaskResult }
func (v *SelectedProjectVerification) Observation() *runner.FrozenIOObservation {
	if v == nil {
		return nil
	}
	return v.observation
}
func (v *SelectedProjectVerification) SelectedInputs() *project.SelectedResultInputs {
	if v == nil {
		return nil
	}
	return v.selected
}

// One entry endpoint charges reload, selection and verification. Policy and
// verifier keep their own EARLIER bounds and fixed schedule-capacity refusal.
func VerifySelectedProjectResult(parent context.Context, v *SelectedProjectResult, prepared *project.PreparedFinalReference, policy runner.FrozenVerificationPolicyRef) (*SelectedProjectVerification, error) {
	anchor := time.Now()
	if parent == nil || parent.Err() != nil || !v.Ready() || prepared == nil {
		return nil, runner.ErrTaskResult
	}
	info, e := runner.ResolveFrozenVerificationPolicy(policy)
	if e != nil {
		return nil, e
	}
	limit := v.input.frozen.Identity().VerifierLimit
	if info.Bounds.Operation < limit {
		limit = info.Bounds.Operation
	}
	if limit <= 0 {
		return nil, runner.ErrTaskResult
	}
	if limit > 1800*time.Second {
		limit = 1800 * time.Second
	}
	ctx, cancel := context.WithDeadline(parent, anchor.Add(limit))
	defer cancel()
	pi := prepared.Identity()
	actual, e := project.LoadPreparedFinalReference(ctx, v.input.store, pi.Ref)
	if e != nil {
		return nil, e
	}
	derived := v.input.frozen.PreparationIdentity()
	// EnvironmentDigest includes generated compose resource settings. Retain both
	// actual identities; preparation and agent export can use different Pids.
	if !reflect.DeepEqual(actual.Identity(), pi) || pi.DefinitionRef != v.mapping.identity.Definition || pi.Inputs.SpecDigest != derived.SpecDigest || pi.Inputs.InputsDigest != derived.InputsDigest || pi.Inputs.ReferenceDigest != derived.ReferenceDigest || !reflect.DeepEqual(pi.ExportInputs, v.input.frozen.Identity().SourceInputs) {
		return nil, runner.ErrTaskResult
	}
	reference, transcript, e := actual.FrozenVerificationInputs(ctx, v.input.frozen)
	if e != nil {
		return nil, e
	}
	candidate, e := v.artifact.FrozenArtifact()
	if e != nil {
		return nil, e
	}
	selected, e := v.ResolveSelectedInputs(ctx, trees.Limits{})
	if e != nil {
		return nil, e
	}
	observation, e := runner.VerifyFrozenIOForPolicy(ctx, runner.FrozenVerificationRequest{Policy: policy, Inputs: v.input.frozen, Reference: reference, Candidate: candidate, ExpectedReference: transcript})
	if e != nil {
		return nil, e
	}
	end, _ := ctx.Deadline()
	if ctx.Err() != nil || !time.Now().Before(end) || observation.Identity().ImageID != pi.Audit.ImageID {
		return nil, runner.ErrTaskResult
	}
	return &SelectedProjectVerification{v, pi, selected, observation}, nil
}

// Identity is LOCAL copied provenance. No verification-ref or authority is minted.
type SelectedProjectVerificationIdentity struct {
	Mapping     SelectedMappingIdentity
	Result      runner.TaskResultIdentity
	Preparation project.PreparedReferenceIdentity
	Selected    project.SelectedResultIdentity
	Observation runner.FrozenIOObservationIdentity
}

func (v *SelectedProjectVerification) Identity() SelectedProjectVerificationIdentity {
	if v == nil {
		return SelectedProjectVerificationIdentity{}
	}
	m, r := v.result.Identity()
	out := SelectedProjectVerificationIdentity{m, r, v.prepared, v.selected.Identity(), v.observation.Identity()}
	b, _ := json.Marshal(out)
	var copied SelectedProjectVerificationIdentity
	_ = json.Unmarshal(b, &copied)
	return copied
}
