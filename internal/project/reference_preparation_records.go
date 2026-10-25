package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

// These limits state what this producer actually enforces. They are not an
// origin/capture authority claim or protection against a concurrent same-owner writer.
type ReferencePreparationReadBoundary struct{ SourceObjects, GeneratedTaskDirectory, WholeOperationIO string }

func referenceReadBoundary() ReferencePreparationReadBoundary {
	return ReferencePreparationReadBoundary{"bounded_same_descriptor_original_source_objects", "owned_0700_private_operation_no_outside_writer_assumption", "cooperative_context_and_outer_observer_not_adversarial_hard_io_or_memory_bound"}
}

type referenceRequest struct {
	Cases                      []qualify.IOResolvedCase
	ReadBoundary               ReferencePreparationReadBoundary
	Version, Authority, Writer string
	DefinitionRef              WorkDefinitionRef
	Definition                 WorkDefinitionIdentity
	Policy                     runner.ReferencePreparationPolicyInfo
	Inputs                     runner.ReferenceInputIdentity
	ExportInputs               map[string]string
	CreatedAt, StartedAt       string
}
type referenceReservation struct {
	Version, State, Authority, Writer string
	Request                           ReferencePreparationRef
	ObservedAbsent                    []string
	HistorySource, CreatedAt          string
}
type ReferenceRetainedArtifact struct {
	Domain, Digest, File string
	Bytes                int64
}
type referenceTerminal struct {
	Eligibility                       string
	Version, Authority, State, Writer string
	Request, Reservation              ReferencePreparationRef
	Audit                             runner.ReferencePrepareAudit
	Artifacts                         []ReferenceRetainedArtifact
	FinishedAt                        string
	FailureCodes                      []string
}
type PreparedReferenceIdentity struct {
	Eligibility                       string
	Cases                             []qualify.IOResolvedCase
	ReadBoundary                      ReferencePreparationReadBoundary
	Version, Authority, State, Writer string
	Ref, Request, Reservation         ReferencePreparationRef
	DefinitionRef                     WorkDefinitionRef
	Definition                        WorkDefinitionIdentity
	Policy                            runner.ReferencePreparationPolicyInfo
	Inputs                            runner.ReferenceInputIdentity
	Audit                             runner.ReferencePrepareAudit
	Artifacts                         []ReferenceRetainedArtifact
}
type referenceEnvelope struct {
	Kind, RecordRef string
	Body            json.RawMessage
}

func referenceLocator(base []string, kind string) string { return base[2] + "." + base[3] + "." + kind }
func referenceRecordRef(base []string, kind string, raw []byte) ReferencePreparationRef {
	return ReferencePreparationRef{referenceLocator(base, kind), library.Digest(raw)}
}
func referenceLocation(ref ReferencePreparationRef, kind string) ([]string, error) {
	p := strings.Split(ref.RecordRef, ".")
	if len(p) != 3 || !batchID.MatchString(p[0]) || !batchID.MatchString(p[1]) || p[2] != kind || !digestPattern.MatchString(ref.RecordDigest) {
		return nil, referenceRefused("record_reference")
	}
	return []string{"state", library.ReferencePreparationStateDir, p[0], p[1]}, nil
}
func mustReferenceBytes(v any) []byte { b, _ := hint.Canonical(v); return b }
func referenceEncoded(base []string, kind string, v any) []byte {
	return mustReferenceBytes(referenceEnvelope{kind, referenceLocator(base, kind), mustReferenceBytes(v)})
}
func decodeReferenceRecord(raw []byte, base []string, kind string, v any) error {
	var env referenceEnvelope
	if definitionDecode(raw, &env) != nil || env.Kind != kind || env.RecordRef != referenceLocator(base, kind) || definitionDecode(env.Body, v) != nil || !reflect.DeepEqual(raw, referenceEncoded(base, kind, v)) {
		return referenceRefused("record_invalid")
	}
	return nil
}
func writeReferenceRecord(ctx context.Context, s *store.Store, base []string, kind string, v any) (ReferencePreparationRef, error) {
	raw := referenceEncoded(base, kind, v)
	if int64(len(raw)) > referenceMetadataMax {
		return ReferencePreparationRef{}, referenceRefused("metadata_limit")
	}
	if e := s.WriteStateImmutableContext(ctx, raw, referenceMetadataMax, append(base, kind+".json")...); e != nil {
		return ReferencePreparationRef{}, referenceRefused("record_commit")
	}
	return referenceRecordRef(base, kind, raw), nil
}
func readReferenceRequest(ctx context.Context, s *store.Store, base []string) (referenceRequest, error) {
	var v referenceRequest
	b, e := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "request.json")...)
	if e != nil || decodeReferenceRecord(b, base, "request", &v) != nil {
		return v, referenceRefused("request_unavailable")
	}
	return v, nil
}

// Only these actual operation artifacts are accepted. This is a closed output
// directory check using the shared bounded descriptor reader, not a source reader.
func ingestReferenceOutputs(ctx context.Context, s *store.Store, base []string, a runner.ReferencePrepareAudit) ([]ReferenceRetainedArtifact, error) {
	if a.FinalTAR.Domain != "hint-b1-frozen-final-tar-v1" || a.FinalTAR.Size <= 0 || a.FinalTAR.Size > 96<<20 || len(a.Cases) < 2 || len(a.Cases) > 128 || a.OracleRuns != 2 || a.Interface != runner.IOInterface {
		return nil, referenceRefused("audit_shape")
	}
	output := append(append([]string{}, base...), "output")
	expected := map[string]ReferenceRetainedArtifact{"final.tar": {a.FinalTAR.Domain, a.FinalTAR.Digest, "final.tar", a.FinalTAR.Size}}
	for _, c := range a.Cases {
		name := fmt.Sprintf("case-outputs/%d-%d.stdout", c.CaseIndex, c.Repetition)
		if c.CaseIndex < 0 || c.Repetition < 0 || c.Repetition > 1 || !c.Complete || !c.NetworkNone || c.Status != 0 || c.Size < 0 || c.Size > 1<<20 || filepath.ToSlash(c.File) != name {
			return nil, referenceRefused("case_binding")
		}
		if _, ok := expected[name]; ok {
			return nil, referenceRefused("case_duplicate")
		}
		expected[name] = ReferenceRetainedArtifact{"hint-b1-stdout-bytes-v1", c.OutputDigest, name, int64(c.Size)}
	}
	if a.FinalTAR.File != "final.tar" {
		return nil, referenceRefused("tar_binding")
	}
	entries, e := s.ReadStateDirBounded(ctx, 4, output...)
	if e != nil || len(entries) != 3 {
		return nil, referenceRefused("output_inventory")
	}
	for _, entry := range entries {
		if entry.Name() == "case-outputs" {
			if !entry.IsDir() {
				return nil, referenceRefused("output_inventory")
			}
			continue
		}
		if entry.IsDir() || (entry.Name() != "final.tar" && entry.Name() != "oracle.txt") {
			return nil, referenceRefused("output_inventory")
		}
	}
	cases, e := s.ReadStateDirBounded(ctx, 128, append(output, "case-outputs")...)
	if e != nil || len(cases) != len(a.Cases) {
		return nil, referenceRefused("output_inventory")
	}
	for _, entry := range cases {
		if entry.IsDir() {
			return nil, referenceRefused("output_inventory")
		}
		if _, ok := expected["case-outputs/"+entry.Name()]; !ok {
			return nil, referenceRefused("output_inventory")
		}
	}
	log, e := s.ReadStateBounded(ctx, 64<<10, append(output, "oracle.txt")...)
	if e != nil {
		return nil, referenceRefused("application_log")
	}
	expected["oracle.txt"] = ReferenceRetainedArtifact{"hint-b1-application-log-bytes-v1", library.Digest(log), "oracle.txt", int64(len(log))}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []ReferenceRetainedArtifact{}
	var used int64
	l := library.Open(s, 0)
	for _, name := range names {
		v := expected[name]
		if !digestPattern.MatchString(v.Digest) || v.Bytes < 0 || v.Bytes > referenceOutputMax-used {
			return nil, referenceRefused("output_limit")
		}
		parts := append(append([]string{}, output...), strings.Split(name, "/")...)
		before, err := referenceUnaliasedFile(s.Path(parts...))
		if err != nil {
			return nil, err
		}
		b, e := s.ReadStateBounded(ctx, max(v.Bytes, 1), parts...)
		after, err := referenceUnaliasedFile(s.Path(parts...))
		if err != nil || !os.SameFile(before, after) {
			return nil, referenceRefused("artifact_changed")
		}
		if e != nil || int64(len(b)) != v.Bytes || library.Digest(b) != v.Digest {
			return nil, referenceRefused("output_binding")
		}
		used += v.Bytes
		d, e := l.PutObjectContext(ctx, b)
		if e != nil || d != v.Digest {
			return nil, referenceRefused("object_commit")
		}
		out = append(out, v)
	}
	return out, nil
}

// Audit validation reconciles the actual fixed procedure and ordered case slots.
// It never evaluates candidate/reference behavior or creates qualification evidence.
func validateReferenceAudit(v referenceRequest, a runner.ReferencePrepareAudit) error {
	n := len(v.Definition.QualifiedInputIdentity.MandatoryTests)
	if n < 1 || n > 64 || len(a.Cases) != 2*n || len(a.Phases) != 2*n+2 || len(a.FailureCodes) != 0 || a.Version != runner.ReferencePrepareVersion || a.Authority != "unknown" || a.State != "prepared_reference_observed" || a.OracleRuns != 2 || a.Interface != runner.IOInterface || a.BuildContextDigest != v.Inputs.EnvironmentDigest || a.SetupDigest != v.Definition.QualifiedInputIdentity.SetupDigest || !digestPattern.MatchString(a.OracleOutputsDigest) || a.ImageID == "" {
		return referenceRefused("audit_incomplete")
	}
	for i, p := range a.Phases {
		role := "reference_case"
		if i == 0 {
			role = "build"
		}
		if i == 1 {
			role = "apply_reference"
		}
		if p.Role != role || p.ImageID != a.ImageID || p.StartedMS < 0 || p.FinishedMS < p.StartedMS || p.WorkEndMS <= p.StartedMS || p.StopEndMS < p.WorkEndMS || p.RemoveEndMS < p.StopEndMS || p.JoinEndMS < p.RemoveEndMS || p.FinishedMS > p.JoinEndMS {
			return referenceRefused("phase_schedule")
		}
		if i == 0 {
			continue
		}
		if !p.Started || !p.WaitObserved || !p.Exited || !p.Removed || !p.NetworkNone || p.ExitStatus != 0 || p.ContainerRef == "" || p.CommandDigest == "" || p.CPUsNano != 2e9 || p.MemoryBytes != 2048<<20 || p.PidsLimit != 1024 || (i > 1 && !p.StreamsJoined) {
			return referenceRefused("phase_incomplete")
		}
	}
	for i, c := range a.Cases {
		if c.CaseIndex != i/2 || c.Repetition != i%2 || !c.Complete || !c.NetworkNone || c.Status != 0 || c.Size < 0 || c.Size > 1<<20 || !digestPattern.MatchString(c.InputDigest) || !digestPattern.MatchString(c.OutputDigest) || filepath.ToSlash(c.File) != fmt.Sprintf("case-outputs/%d-%d.stdout", i/2, i%2) {
			return referenceRefused("case_schedule")
		}
		if i%2 == 1 {
			prev := a.Cases[i-1]
			if c.InputDigest != prev.InputDigest || c.OutputDigest != prev.OutputDigest || c.Size != prev.Size {
				return referenceRefused("oracle_disagreement")
			}
		}
	}
	return nil
}

func LoadPreparedFinalReference(parent context.Context, s *store.Store, ref ReferencePreparationRef) (*PreparedFinalReference, error) {
	if parent == nil || s == nil {
		return nil, referenceRefused("request_invalid")
	}
	ctx, c := context.WithTimeout(parent, 30*time.Second)
	defer c()
	return loadPreparedReference(ctx, s, ref)
}
func loadPreparedReference(ctx context.Context, s *store.Store, ref ReferencePreparationRef) (*PreparedFinalReference, error) {
	base, e := referenceLocation(ref, "outcome")
	if e != nil {
		return nil, e
	}
	raw, e := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "outcome.json")...)
	var t referenceTerminal
	if e != nil || library.Digest(raw) != ref.RecordDigest || decodeReferenceRecord(raw, base, "outcome", &t) != nil || t.State != "prepared_reference_observed" || t.Eligibility != "requires_process_local_acceptance_witness" || t.Authority != "unknown" || t.Version != ReferencePreparationVersion || t.Writer != referenceBridgeIdentity() || len(t.FailureCodes) != 0 {
		return nil, referenceRefused("terminal_invalid")
	}
	v, e := readReferenceRequest(ctx, s, base)
	if e != nil {
		return nil, e
	}
	requestRaw := referenceEncoded(base, "request", v)
	if t.Request != referenceRecordRef(base, "request", requestRaw) || v.Version != ReferencePreparationVersion || v.Writer != referenceBridgeIdentity() || v.Authority != "unknown" || v.Definition.UnitRef != base[2] || v.ReadBoundary != referenceReadBoundary() {
		return nil, referenceRefused("request_binding")
	}
	rraw, e := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "reservation.json")...)
	var r referenceReservation
	if e != nil || t.Reservation != referenceRecordRef(base, "reservation", rraw) || decodeReferenceRecord(rraw, base, "reservation", &r) != nil || r.Request != t.Request || r.Version != ReferencePreparationVersion || r.Authority != "unknown" || r.State != "reserved" || r.Writer != referenceBridgeIdentity() || r.HistorySource != definitionSourceIdentity() {
		return nil, referenceRefused("reservation_binding")
	}
	def, q, e := loadWorkDefinitionResolved(ctx, s, v.DefinitionRef)
	if e != nil || !reflect.DeepEqual(def.Identity(), v.Definition) || !reflect.DeepEqual(q.Cases(), v.Cases) {
		return nil, referenceRefused("definition_binding")
	}
	pi, e := runner.ResolveReferencePreparationPolicy(v.Policy.Policy)
	if e != nil || !reflect.DeepEqual(pi, v.Policy) {
		return nil, referenceRefused("policy_binding")
	}
	a := t.Audit
	if a.State != "prepared_reference_observed" || a.Authority != "unknown" || a.Inputs != v.Inputs || a.InstalledPolicy != pi.Policy || a.PolicyBundleDigest != pi.BundleDigest || a.PolicyCatalogueDigest != pi.CatalogueDigest || a.PolicyEndpointDigest != pi.EndpointDigest || a.PolicySocketIdentityDigest != pi.SocketIdentityDigest || a.EngineRef != pi.EngineRef || a.EngineVersion != pi.EngineVersion || a.EngineOS != pi.EngineOS || a.EngineArchitecture != pi.EngineArchitecture {
		return nil, referenceRefused("audit_binding")
	}
	if validateReferenceAudit(v, a) != nil {
		return nil, referenceRefused("audit_incomplete")
	}
	for _, p := range a.Phases {
		if p.Role == "build" {
			continue
		}
		if !p.Started || !p.WaitObserved || !p.Exited || !p.Removed || !p.NetworkNone || p.ExitStatus != 0 || p.ContainerRef == "" || p.FinishedMS > p.JoinEndMS || ((p.Role != "apply_reference") && !p.StreamsJoined) {
			return nil, referenceRefused("phase_incomplete")
		}
	}
	// Verify retained object bytes independently of scratch paths. No latest tree,
	// new run, or current work-history eligibility participates in factual loading.
	objects := map[string][]byte{}
	l := library.Open(s, 0)
	var used int64
	if len(t.Artifacts) != len(a.Cases)+2 {
		return nil, referenceRefused("artifact_count")
	}
	seen := map[string]bool{}
	for _, v := range t.Artifacts {
		cap := int64(1 << 20)
		domain := "hint-b1-stdout-bytes-v1"
		switch v.File {
		case "final.tar":
			cap = 96 << 20
			domain = "hint-b1-frozen-final-tar-v1"
		case "oracle.txt":
			cap = 64 << 10
			domain = "hint-b1-application-log-bytes-v1"
		default:
			valid := false
			for _, c := range a.Cases {
				if v.File == fmt.Sprintf("case-outputs/%d-%d.stdout", c.CaseIndex, c.Repetition) {
					valid = true
				}
			}
			if !valid {
				return nil, referenceRefused("artifact_inventory")
			}
		}
		if v.Domain != domain || v.Bytes > cap || seen[v.File] || v.Bytes < 0 || v.Bytes > referenceOutputMax-used {
			return nil, referenceRefused("artifact_limit")
		}
		seen[v.File] = true
		b, e := l.ResolveBoundedRead(ctx, "object", v.Digest, max(v.Bytes, 1))
		if e != nil || int64(len(b)) != v.Bytes || library.Digest(b) != v.Digest {
			return nil, referenceRefused("artifact_binding")
		}
		used += v.Bytes
		objects[v.Digest] = b
	}
	if !seen["final.tar"] || !seen["oracle.txt"] {
		return nil, referenceRefused("artifact_inventory")
	}
	for _, c := range a.Cases {
		name := fmt.Sprintf("case-outputs/%d-%d.stdout", c.CaseIndex, c.Repetition)
		found := false
		for _, v := range t.Artifacts {
			if v.File == name && v.Digest == c.OutputDigest && v.Bytes == int64(c.Size) && v.Domain == "hint-b1-stdout-bytes-v1" {
				found = true
			}
		}
		if !found || !c.Complete || !c.NetworkNone || c.Status != 0 {
			return nil, referenceRefused("case_object")
		}
	}
	found := false
	for _, v := range t.Artifacts {
		if v.File == "final.tar" && v.Digest == a.FinalTAR.Digest && v.Bytes == a.FinalTAR.Size && v.Domain == "hint-b1-frozen-final-tar-v1" {
			found = true
		}
	}
	if !found {
		return nil, referenceRefused("tar_object")
	}
	if e := reconcileReferenceTranscript(ctx, s, t.Artifacts, a, v.Cases); e != nil {
		return nil, e
	}
	if !referenceCurrent(ctx) {
		return nil, referenceRefused("late_load")
	}
	return &PreparedFinalReference{PreparedReferenceIdentity{"requires_process_local_acceptance_witness", v.Cases, referenceReadBoundary(), ReferencePreparationVersion, "unknown", t.State, t.Writer, ref, t.Request, t.Reservation, v.DefinitionRef, v.Definition, pi, v.Inputs, a, t.Artifacts}, objects}, nil
}

// Reconcile actual retained bytes, not a caller's digest assertion. Framing belongs
// to the sole pure runner helper; qualified cases come from its already-parsed handle.
func reconcileReferenceTranscript(ctx context.Context, s *store.Store, artifacts []ReferenceRetainedArtifact, a runner.ReferencePrepareAudit, cases []qualify.IOResolvedCase) error {
	if len(cases) < 1 || len(cases) > 64 || len(a.Cases) != 2*len(cases) {
		return referenceRefused("transcript_count")
	}
	byFile := map[string]ReferenceRetainedArtifact{}
	for _, v := range artifacts {
		if _, ok := byFile[v.File]; ok {
			return referenceRefused("transcript_duplicate")
		}
		byFile[v.File] = v
	}
	frames := make([]runner.IOTranscriptCase, 0, len(cases))
	l := library.Open(s, 0)
	for i, c := range cases {
		var first []byte
		for k := 0; k < 2; k++ {
			r := a.Cases[i*2+k]
			if r.CaseIndex != i || r.Repetition != k || r.InputDigest != c.InputDigest || r.Status != 0 || !r.Complete || !r.NetworkNone {
				return referenceRefused("transcript_input_or_order")
			}
			name := fmt.Sprintf("case-outputs/%d-%d.stdout", i, k)
			v, ok := byFile[name]
			if !ok || v.Digest != r.OutputDigest || v.Bytes != int64(r.Size) || v.Bytes < 0 || v.Bytes > 1<<20 {
				return referenceRefused("transcript_output")
			}
			b, e := l.ResolveBoundedRead(ctx, "object", v.Digest, max(v.Bytes, 1))
			if e != nil || int64(len(b)) != v.Bytes || library.Digest(b) != r.OutputDigest {
				return referenceRefused("transcript_bytes")
			}
			if k == 0 {
				first = b
			} else if !reflect.DeepEqual(first, b) {
				return referenceRefused("transcript_disagreement")
			}
		}
		frames = append(frames, runner.IOTranscriptCase{ID: c.ID, Status: 0, Output: first})
	}
	digest, e := runner.IOTranscriptDigest(frames)
	if e != nil || digest != a.OracleOutputsDigest {
		return referenceRefused("transcript_aggregate")
	}
	if !referenceCurrent(ctx) {
		return referenceRefused("late_transcript")
	}
	return nil
}
