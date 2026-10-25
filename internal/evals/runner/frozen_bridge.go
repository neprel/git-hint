package runner

// LOCAL factual verification only. Normal wire requires a separate keyed privacy
// projection. There is no project completion, origin or trusted policy activation.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/neprel/git-hint/internal/evals/safefs"
)

var ErrFrozenVerification = errors.New("frozen_verification_refused")

func frozenBridgeFailure(code string) error { return fmt.Errorf("%w: %s", ErrFrozenVerification, code) }

type FrozenVerificationPolicyRef struct{ PolicyID, PolicyDigest string }
type FrozenVerificationRequest struct {
	Policy               FrozenVerificationPolicyRef
	Inputs               *FrozenIOInputs
	Reference, Candidate *FrozenIOArtifact
	ExpectedReference    *FrozenIOReferenceTranscript
}
type FrozenIOCaseIdentity struct {
	ID, Role, InputDigest, OutputDigest string
	Repetition, OutputBytes             int
	Status                              int64
	Complete                            bool
	Phase                               ReferencePhaseAudit
}
type frozenCaseObservation struct {
	identity FrozenIOCaseIdentity
	output   []byte
}
type FrozenIOObservation struct {
	identity FrozenIOObservationIdentity
	cases    []frozenCaseObservation
}
type FrozenIOObservationIdentity struct {
	Version                                                                                                                                   string
	EngineVersion, EngineOS, EngineArchitecture, ImageOS, ImageArchitecture, ImageVariant                                                     string
	ExpectedReference                                                                                                                         FrozenIOReferenceTranscriptIdentity
	PlannedExecutions                                                                                                                         int
	State, Authority, Reason, ImageID, EngineRef, PolicyBundleDigest, PolicyCatalogueDigest, PolicyEndpointDigest, PolicySocketIdentityDigest string
	Policy                                                                                                                                    FrozenVerificationPolicyRef
	Inputs                                                                                                                                    FrozenIOInputsIdentity
	Reference, Candidate                                                                                                                      FrozenIOArtifactIdentity
	OracleOutputsDigest, CandidateOutputsDigest, ScheduleDigest                                                                               string
	Cases                                                                                                                                     []FrozenIOCaseIdentity
}

func (h *FrozenIOObservation) Identity() FrozenIOObservationIdentity {
	if h == nil {
		return FrozenIOObservationIdentity{State: "unknown", Authority: "unknown"}
	}
	v := h.identity
	v.Inputs.SourceInputs = cloneStringMap(v.Inputs.SourceInputs)
	v.Inputs.Cases = append([]string(nil), v.Inputs.Cases...)
	v.Cases = append([]FrozenIOCaseIdentity(nil), v.Cases...)
	return v
}
func (h *FrozenIOObservation) Output(i int) ([]byte, bool) {
	if h == nil || i < 0 || i >= len(h.cases) {
		return nil, false
	}
	return bytes.Clone(h.cases[i].output), true
}
func VerifyFrozenIOForPolicy(parent context.Context, req FrozenVerificationRequest) (out *FrozenIOObservation, err error) {
	anchor := time.Now()
	out = &FrozenIOObservation{identity: FrozenIOObservationIdentity{State: "unknown", Authority: "unknown", Version: "hint-frozen-verification-v1", Policy: req.Policy}}
	fail := func(code string) (*FrozenIOObservation, error) {
		out.identity.State = "unknown"
		out.identity.Reason = code
		return out, frozenBridgeFailure(code)
	}
	if parent == nil || parent.Err() != nil {
		return fail("context_invalid")
	}
	// Purpose lookup precedes materialization and all file/engine IO.
	entry, info, e := resolvePurposeEntry(ReferencePreparationPolicyRef{req.Policy.PolicyID, req.Policy.PolicyDigest}, frozenPurpose)
	if e != nil {
		return fail("verification_policy_unavailable")
	}
	if !req.Inputs.valid() || !req.Reference.valid() || !req.Candidate.valid() || req.ExpectedReference == nil || req.ExpectedReference.inputs != req.Inputs.stamp() {
		return fail("inputs_invalid")
	}
	lim := req.Inputs.limits
	if lim.verifier <= 0 || lim.lan || lim.cpus != 2 || lim.memoryMB != 2048 {
		return fail("runtime_limits")
	}
	total := req.Inputs.byteCount() + int64(len(req.Reference.data)) + int64(len(req.Candidate.data)) + int64(len(req.Reference.identity.Domain)+len(req.Reference.identity.Digest)+len(req.Candidate.identity.Domain)+len(req.Candidate.identity.Digest))
	for _, c := range req.ExpectedReference.cases {
		total += int64(len(c.Output) + len(c.ID) + 8)
	}
	if total > 256<<20 {
		return fail("aggregate_limit")
	}
	end := anchor.Add(1800 * time.Second)
	for _, d := range []time.Duration{lim.verifier, entry.Procedure.operation} {
		if v := anchor.Add(d); v.Before(end) {
			end = v
		}
	}
	if v, ok := parent.Deadline(); ok && v.Before(end) {
		end = v
	}
	out.identity.ExpectedReference = req.ExpectedReference.Identity()
	out.identity.Inputs = req.Inputs.Identity()
	out.identity.Reference = req.Reference.identity
	out.identity.Candidate = req.Candidate.identity
	count := len(req.Inputs.task.spec.Cases) * (OracleRuns + 1)
	out.identity.PlannedExecutions = count
	needed := 60*time.Second + time.Duration(count)*(time.Duration(req.Inputs.task.spec.CaseSeconds)*time.Second+40*time.Second)
	if anchor.Add(needed).After(end) {
		return fail("schedule_capacity")
	}
	ctx, cancel := context.WithDeadline(parent, end)
	defer cancel()
	initial, ic := context.WithDeadline(ctx, minFrozenEndpoint(anchor.Add(30*time.Second), end.Add(-30*time.Second)))
	defer ic()
	if initial.Err() != nil {
		return fail("resolve_late")
	}
	dir, e := os.MkdirTemp("", "hint-frozen-verify-")
	if e != nil {
		return fail("workspace_unavailable")
	}
	defer func() {
		if e := os.RemoveAll(dir); e != nil || ctx.Err() != nil || !time.Now().Before(end) {
			out.identity.State = "unknown"
			out.identity.Reason = "cleanup_or_endpoint_unknown"
			err = frozenBridgeFailure(out.identity.Reason)
		}
	}()
	w, e := safefs.Open(dir)
	if e != nil {
		return fail("workspace_unavailable")
	}
	if e = w.WriteFile("reference.tar", req.Reference.data, 0600); e == nil {
		e = w.WriteFile("candidate.tar", req.Candidate.data, 0600)
	}
	if e == nil {
		e = w.Mkdir("verifier")
	}
	ce := w.Close()
	if e != nil || ce != nil || initial.Err() != nil {
		return fail("materialization_unknown")
	}
	openctx, oc := context.WithTimeout(initial, 10*time.Second)
	d, e := openPinnedProcedureEngine(openctx, entry, info)
	oc()
	if e != nil {
		return fail("engine_unavailable")
	}
	defer func() {
		if e := d.Close(); e != nil {
			out.identity.State = "unknown"
			out.identity.Reason = "engine_close_unknown"
			err = frozenBridgeFailure(out.identity.Reason)
		}
	}()
	if initial.Err() != nil || d.referenceBinding == nil || *d.referenceBinding != *referenceBindingFor(entry, info) {
		return fail("engine_binding")
	}
	im, e := d.resolveImage(initial, entry.Image)
	arch := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}[entry.EngineArchitecture]
	if e != nil || im.ID != entry.Image || im.OS != "linux" || im.Arch != arch || arch == "" || initial.Err() != nil {
		return fail("image_binding")
	}
	out.identity.Inputs = req.Inputs.Identity()
	out.identity.Reference = req.Reference.identity
	out.identity.Candidate = req.Candidate.identity
	out.identity.ImageID = im.ID
	out.identity.ImageOS = im.OS
	out.identity.ImageArchitecture = im.Arch
	out.identity.ImageVariant = im.Variant
	out.identity.EngineVersion = info.EngineVersion
	out.identity.EngineOS = info.EngineOS
	out.identity.EngineArchitecture = info.EngineArchitecture
	out.identity.EngineRef = info.EngineRef
	out.identity.PolicyBundleDigest = info.BundleDigest
	out.identity.PolicyCatalogueDigest = info.CatalogueDigest
	out.identity.PolicyEndpointDigest = info.EndpointDigest
	out.identity.PolicySocketIdentityDigest = info.SocketIdentityDigest
	call := frozenIORequest{reference: frozenIOArtifact{filepath.Join(dir, "reference.tar"), req.Reference.identity.Digest, im.ID}, candidate: frozenIOArtifact{filepath.Join(dir, "candidate.tar"), req.Candidate.identity.Digest, im.ID}, output: dir, expectedReference: req.ExpectedReference.digest, observations: &out.cases}
	_, prov, a, e := d.verifyFrozenIOAt(ctx, req.Inputs.task, lim, call, &frozenVerificationPolicy{frozenVerificationProcess, im.ID}, anchor)
	out.identity.ScheduleDigest = a.ScheduleDigest
	for _, c := range out.cases {
		out.identity.Cases = append(out.identity.Cases, c.identity)
	}
	if v, ok := prov["oracle_outputs"].(string); ok {
		out.identity.OracleOutputsDigest = v
	}
	if v, ok := prov["candidate_outputs"].(string); ok {
		out.identity.CandidateOutputsDigest = v
	}
	if e != nil {
		if out.identity.OracleOutputsDigest != "" && out.identity.OracleOutputsDigest != req.ExpectedReference.digest {
			return fail("reference_transcript_drift")
		}
		return fail("verification_unknown")
	}
	if referenceCheckSocket(ctx, entry) != nil || d.referenceCheckEngine(ctx, entry) != nil || ctx.Err() != nil {
		return fail("final_binding_unknown")
	}
	out.identity.State = a.State
	return out, nil
}
func minFrozenEndpoint(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
