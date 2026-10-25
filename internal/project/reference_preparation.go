package project

// Local preparation persistence is factual only. It supplies no source, start,
// completion, capture or producer authority. Production runner policy is empty.
import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

const ReferencePreparationVersion = "project-reference-preparation-v1"
const referenceMetadataMax int64 = 1 << 20
const referenceOutputMax int64 = 256 << 20
const referenceFinalCommitMax = 60 * time.Second

var ErrReferencePreparation = errors.New("project_reference_preparation_unknown")

//go:embed reference_preparation.go reference_preparation_records.go reference_preparation_unix.go reference_preparation_other.go prework_acceptance.go
var referencePreparationSources embed.FS

// This declared source family and its dependency identities are not a complete
// transitive compiled-source closure or source-to-binary attestation.
func referenceBridgeIdentity() string {
	return referenceBridgeIdentityFrom(referencePreparationSources)
}
func referenceBridgeIdentityFrom(sources fs.FS) string {
	names, e := referenceBridgeSourceNames(sources)
	if e != nil || len(names) == 0 {
		return ""
	}
	values := []string{ReferencePreparationVersion, preworkAcceptanceVersion}
	for _, name := range names {
		raw, e := fs.ReadFile(sources, name)
		if e != nil {
			return ""
		}
		values = append(values, name, library.Digest(raw))
	}
	values = append(values, definitionSourceIdentity(), store.BoundedStateSourceIdentity(), qualify.QualifiedInputSourceIdentity())
	digest, e := hint.Digest(values)
	if e != nil {
		return ""
	}
	return digest
}
func referenceBridgeSourceNames(fsys fs.FS) ([]string, error) {
	entries, e := fs.ReadDir(fsys, ".")
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, entry := range entries {
		n := entry.Name()
		if (!strings.HasPrefix(n, "reference_preparation") && !strings.HasPrefix(n, "prework_acceptance")) || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fs.ErrInvalid
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
func referenceRefused(code string) error {
	return fmt.Errorf("%w: %s", ErrReferencePreparation, code)
}

type ReferencePreparationRef struct{ RecordRef, RecordDigest string }
type PrepareReferenceRequest struct {
	Definition         WorkDefinitionRef
	Label, OperationID string
	Policy             runner.ReferencePreparationPolicyRef
	Replay             *ReferencePreparationAcceptance
}
type ReferencePreparationAttempt struct {
	reservation, outcome ReferencePreparationRef
	prepared             *ReferencePreparationRef
	acceptance           *ReferencePreparationAcceptance
}

func (a *ReferencePreparationAttempt) Reservation() ReferencePreparationRef {
	if a == nil {
		return ReferencePreparationRef{}
	}
	return a.reservation
}
func (a *ReferencePreparationAttempt) Outcome() *ReferencePreparationRef {
	if a == nil || a.outcome.RecordRef == "" {
		return nil
	}
	v := a.outcome
	return &v
}
func (a *ReferencePreparationAttempt) Prepared() *ReferencePreparationRef {
	if a == nil || a.prepared == nil {
		return nil
	}
	v := *a.prepared
	return &v
}

// The opaque wrapper binds its exact reference domains to the shared process
// checkpoint. Factual loading cannot mint it. Restart loses this eligibility.
type ReferencePreparationAcceptance struct {
	checkpoint                    preworkAcceptance
	outcome, request, reservation ReferencePreparationRef
	storeBinding, writer          string
	policy                        runner.ReferencePreparationPolicyRef
}

func (a *ReferencePreparationAttempt) Acceptance() *ReferencePreparationAcceptance {
	if a == nil || a.acceptance == nil {
		return nil
	}
	v := *a.acceptance
	return &v
}
func (a ReferencePreparationAcceptance) MarshalJSON() ([]byte, error) {
	return nil, referenceRefused("acceptance_not_serializable")
}
func (a *ReferencePreparationAcceptance) UnmarshalJSON([]byte) error {
	return referenceRefused("acceptance_not_deserializable")
}
func referenceAcceptanceMatches(a *ReferencePreparationAcceptance, outcome, request, reservation ReferencePreparationRef, v referenceRequest) bool {
	return a != nil && matchPreworkAcceptance(a.checkpoint, preworkReferencePreparation) && a.outcome == outcome && a.request == request && a.reservation == reservation && a.storeBinding == v.Definition.StoreBinding && a.writer == referenceBridgeIdentity() && a.policy == v.Policy.Policy
}
func referenceCurrent(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	d, ok := ctx.Deadline()
	return !ok || time.Now().Before(d)
}

type PreparedFinalReference struct {
	identity PreparedReferenceIdentity
	objects  map[string][]byte
}

func (h *PreparedFinalReference) Identity() PreparedReferenceIdentity {
	b, _ := json.Marshal(h.identity)
	var v PreparedReferenceIdentity
	_ = json.Unmarshal(b, &v)
	return v
}
func (h *PreparedFinalReference) Object(d string) ([]byte, bool) {
	b, ok := h.objects[d]
	return append([]byte(nil), b...), ok
}

// PrepareFinalReference owns the original endpoint before lookup, locking or IO.
// Every successful terminal/replay rechecks history; factual Load does not.
func PrepareFinalReference(parent context.Context, s *store.Store, req PrepareReferenceRequest, now time.Time) (*ReferencePreparationAttempt, error) {
	if parent == nil || s == nil || now.IsZero() || !ValidTaskID(req.Label) || len(req.OperationID) < 1 || len(req.OperationID) > 128 || strings.IndexFunc(req.OperationID, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
		return nil, referenceRefused("request_invalid")
	}
	anchor := time.Now()
	ctx, cancel := context.WithDeadline(parent, anchor.Add(1800*time.Second))
	defer cancel()
	end, _ := ctx.Deadline()
	if !end.After(anchor.Add(60 * time.Second)) {
		return nil, referenceRefused("insufficient_budget")
	}
	initial, ic := context.WithDeadline(ctx, minReferenceTime(anchor.Add(30*time.Second), end.Add(-60*time.Second)))
	defer ic()
	info, e := runner.ResolveReferencePreparationPolicy(req.Policy)
	if e != nil {
		return nil, referenceRefused("policy_unavailable")
	}
	lock, e := s.TryLockStateContext(initial)
	if e != nil {
		return nil, referenceRefused("state_lock_refused")
	}
	prepared, attempt, task, base, e := reserveReference(initial, s, req, info, now, anchor, end.Add(-60*time.Second))
	lock.Unlock()
	if e != nil {
		return attempt, e
	}
	if prepared != nil {
		if !referenceCurrent(initial) {
			return attempt, referenceRefused("late_replay")
		}
		ref := attempt.outcome
		attempt.prepared = &ref
		copy := *req.Replay
		attempt.acceptance = &copy
		return attempt, nil
	}
	runctx, rc := context.WithDeadline(ctx, end.Add(-60*time.Second))
	var d *runner.Docker
	de := referencePolicyWindow(info, end.Add(-60*time.Second))
	if de == nil {
		openctx, oc := context.WithTimeout(runctx, 10*time.Second)
		d, de = runner.OpenReferencePreparationEngine(openctx, req.Policy)
		oc()
	}
	audit := runner.ReferencePrepareAudit{State: "unknown", Authority: "unknown", FailureCodes: []string{"engine_unavailable"}}
	if de == nil {
		audit, de = d.PrepareReferenceForPolicy(runctx, task, runner.ReferencePolicyPrepareRequest{Policy: req.Policy, OutputDir: s.Path(append(base, "output")...), Expected: baseInput(s, runctx, base)})
		_ = d.Close()
	}
	rc()
	// Cleanup and terminal failure persistence use only the original endpoint. Caller
	// cancellation never becomes success; cancellation-independent context adds no grace.
	finalEnd := minReferenceTime(time.Now().Add(referenceFinalCommitMax), end)
	final, fc := context.WithDeadline(ctx, finalEnd)
	defer fc()
	commit, cc := context.WithDeadline(context.WithoutCancel(ctx), finalEnd)
	defer cc()
	lock, e = s.TryLockStateContext(commit)
	if e != nil {
		return attempt, referenceRefused("terminal_lock_refused")
	}
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()
	v, e := readReferenceRequest(commit, s, base)
	if e != nil {
		return attempt, e
	}
	success := de == nil && ctx.Err() == nil && time.Now().Before(end) && audit.State == "prepared_reference_observed" && audit.Authority == "unknown" && audit.Inputs == v.Inputs && audit.InstalledPolicy == req.Policy && audit.PolicyBundleDigest == v.Policy.BundleDigest && audit.PolicyCatalogueDigest == v.Policy.CatalogueDigest && audit.PolicyEndpointDigest == v.Policy.EndpointDigest && audit.PolicySocketIdentityDigest == v.Policy.SocketIdentityDigest
	h, q, he := loadWorkDefinitionResolved(commit, s, req.Definition)
	if he != nil || !reflect.DeepEqual(h.identity, v.Definition) || !reflect.DeepEqual(q.Cases(), v.Cases) {
		success = false
	}
	if _, he = definitionHistory(&definitionStateRead{ctx: commit, s: s}, req.Label, v.Definition.UnitRef); he != nil {
		success = false
	}
	pi, pe := runner.ResolveReferencePreparationPolicy(req.Policy)
	if pe != nil || !reflect.DeepEqual(pi, v.Policy) {
		success = false
	}
	files := []ReferenceRetainedArtifact{}
	if success && validateReferenceAudit(v, audit) != nil {
		success = false
	}
	if success {
		files, e = ingestReferenceOutputs(final, s, base, audit)
		if e == nil {
			e = reconcileReferenceTranscript(final, s, files, audit, v.Cases)
		}
		if e != nil {
			success = false
		}
	}
	terminal := referenceTerminal{Eligibility: "requires_process_local_acceptance_witness", Version: ReferencePreparationVersion, Authority: "unknown", State: "unknown", Request: referenceRecordRef(base, "request", referenceEncoded(base, "request", v)), Reservation: attempt.reservation, Audit: audit, Artifacts: files, Writer: referenceBridgeIdentity(), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if success {
		terminal.State = "prepared_reference_observed"
	}
	if !success {
		terminal.FailureCodes = []string{"terminal_binding_history_or_output_unknown"}
	}
	writeCtx := commit
	if success {
		writeCtx = final
	}
	ref, e := writeReferenceRecord(writeCtx, s, base, "outcome", terminal)
	if e != nil {
		return attempt, e
	}
	attempt.outcome = ref
	if !success {
		return attempt, referenceRefused("preparation_unknown")
	}
	// Validate persisted records/objects through the same factual loader before returning.
	if _, e = loadPreparedReference(writeCtx, s, ref); e != nil {
		return attempt, e
	}
	lock.Unlock()
	locked = false
	if !referenceCurrent(final) {
		return attempt, referenceRefused("late_completion")
	}
	attempt.prepared = &ref
	attempt.acceptance = &ReferencePreparationAcceptance{mintPreworkAcceptance(preworkReferencePreparation), ref, terminal.Request, attempt.reservation, v.Definition.StoreBinding, v.Writer, v.Policy.Policy}
	return attempt, nil
}

// This envelope is intentionally conservative. It does not copy the private
// per-case scheduler. A large installed policy may not fit this project wrapper.
func referencePolicyWindow(info runner.ReferencePreparationPolicyInfo, end time.Time) error {
	if time.Until(end) < info.Bounds.Operation+10*time.Second {
		return referenceRefused("policy_window_refused")
	}
	return nil
}
func minReferenceTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func baseInput(s *store.Store, ctx context.Context, base []string) runner.ReferenceInputIdentity {
	v, e := readReferenceRequest(ctx, s, base)
	if e != nil {
		return runner.ReferenceInputIdentity{}
	}
	return v.Inputs
}

func reserveReference(ctx context.Context, s *store.Store, req PrepareReferenceRequest, info runner.ReferencePreparationPolicyInfo, now, anchor, workEnd time.Time) (*PreparedFinalReference, *ReferencePreparationAttempt, string, []string, error) {
	fail := func(code string) (*PreparedFinalReference, *ReferencePreparationAttempt, string, []string, error) {
		return nil, nil, "", nil, referenceRefused(code)
	}
	h, q, e := loadWorkDefinitionResolved(ctx, s, req.Definition)
	if e != nil {
		return fail("definition_unavailable")
	}
	v := h.Identity()
	if hint.EvalRef(s.Key(), "work-unit", req.Label) != v.UnitRef {
		return fail("unit_binding")
	}
	absent, e := definitionHistory(&definitionStateRead{ctx: ctx, s: s}, req.Label, v.UnitRef)
	if e != nil {
		return fail("history_refused")
	}
	op := hint.EvalRef(s.Key(), ReferencePreparationVersion, v.UnitRef+"\x00"+req.OperationID)
	base := []string{"state", library.ReferencePreparationStateDir, v.UnitRef, op}
	parent := base[:len(base)-1]
	// All received operation directories participate, including empty crash remnants.
	reader := &definitionStateRead{ctx: ctx, s: s}
	entries, e := reader.dir(parent...)
	if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return fail("operation_inventory")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !batchID.MatchString(entry.Name()) {
			return fail("operation_inventory")
		}
		if entry.Name() == op {
			continue
		}
		other := append(append([]string{}, parent...), entry.Name())
		raw, er := reader.read(referenceMetadataMax, append(other, "outcome.json")...)
		var t referenceTerminal
		if er != nil || decodeReferenceRecord(raw, other, "outcome", &t) != nil || t.Version != ReferencePreparationVersion || t.Writer != referenceBridgeIdentity() {
			return fail("pending_operation")
		}
		prior, pe := readReferenceRequest(ctx, s, other)
		rr, re := reader.read(referenceMetadataMax, append(other, "reservation.json")...)
		var reservation referenceReservation
		if pe != nil || re != nil || decodeReferenceRecord(rr, other, "reservation", &reservation) != nil || prior.Definition.UnitRef != v.UnitRef || prior.Version != ReferencePreparationVersion || prior.Writer != referenceBridgeIdentity() || t.Request != referenceRecordRef(other, "request", referenceEncoded(other, "request", prior)) || t.Reservation != referenceRecordRef(other, "reservation", rr) || reservation.Request != t.Request || reservation.Version != ReferencePreparationVersion || reservation.State != "reserved" || reservation.Writer != referenceBridgeIdentity() || (t.State != "unknown" && t.State != "prepared_reference_observed") {
			return fail("pending_operation")
		}

	}
	if raw, er := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "request.json")...); er == nil {
		var old referenceRequest
		if decodeReferenceRecord(raw, base, "request", &old) != nil || old.DefinitionRef != req.Definition || old.Policy.Policy != req.Policy || !reflect.DeepEqual(old.Definition, v) || old.Writer != referenceBridgeIdentity() {
			return fail("immutable_conflict")
		}
		rr, er := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "reservation.json")...)
		if er != nil {
			return fail("orphan_operation")
		}
		a := &ReferencePreparationAttempt{reservation: referenceRecordRef(base, "reservation", rr)}
		out, er := s.ReadStateBounded(ctx, referenceMetadataMax, append(base, "outcome.json")...)
		if er != nil {
			return nil, a, "", base, referenceRefused("orphan_operation")
		}
		ref := referenceRecordRef(base, "outcome", out)
		a.outcome = ref
		loaded, er := loadPreparedReference(ctx, s, ref)
		if er != nil {
			return nil, a, "", base, referenceRefused("terminal_unknown")
		}
		if !referenceAcceptanceMatches(req.Replay, ref, loaded.identity.Request, a.reservation, old) {
			return nil, a, "", base, referenceRefused("replay_acceptance_witness_required")
		}
		if !referenceCurrent(ctx) {
			return nil, a, "", base, referenceRefused("late_replay")
		}
		return loaded, a, "", base, nil
	} else if !errors.Is(er, fs.ErrNotExist) {
		return fail("operation_unreadable")
	}
	if req.Replay != nil {
		return fail("witness_without_exact_outcome")
	}
	// Refuse any partial allocation instead of resuming it.
	if _, er := s.ReadStateDirBounded(ctx, 1, base...); er == nil || !errors.Is(er, fs.ErrNotExist) {
		return fail("orphan_operation")
	}
	if referencePolicyWindow(info, workEnd) != nil {
		return fail("policy_window_refused")
	}
	l := library.Open(s, 0)
	if !reflect.DeepEqual(q.Identity(), v.QualifiedInputIdentity) {
		return fail("qualified_inputs")
	}
	raw, e := l.ResolveBoundedRead(ctx, "eval", v.QualifiedInputIdentity.EvalName, qualify.ResolveMetadataMax)
	if e != nil || library.Digest(raw) != v.QualifiedInputIdentity.ManifestDigest || evals.Validate("eval", raw) != nil {
		return fail("manifest_binding")
	}
	var rec map[string]any
	if json.Unmarshal(raw, &rec) != nil {
		return fail("manifest_invalid")
	}
	if e = s.EnsureStateDirContext(ctx, base...); e != nil {
		return fail("directory_refused")
	}
	taskDir := s.Path(append(base, "task")...)
	task, e := runner.ExportBounded(ctx, l, rec, taskDir, runner.Settings{AgentSeconds: 1800, VerifierSeconds: 600, BuildSeconds: 600, CPUs: 2, MemoryMB: 2048, Pids: 1024})
	if e != nil {
		return fail("export_refused")
	}
	env, _ := rec["environment"].(map[string]any)
	repos, _ := rec["repositories"].([]any)
	if len(repos) != 1 {
		return fail("export_binding")
	}
	repo, _ := repos[0].(map[string]any)
	expected := map[string]string{"instruction": str(rec["instruction"]), "base_archive": str(repo["base_archive"]), "verifier": v.QualifiedInputIdentity.VerifierArchive, "reference": v.QualifiedInputIdentity.ReferencePatchArchive}
	if v.QualifiedInputIdentity.SetupDigest != "" {
		expected["setup"] = v.QualifiedInputIdentity.SetupDigest
	}
	if !reflect.DeepEqual(task.Inputs, expected) || str(env["image"]) != v.QualifiedInputIdentity.Image {
		return fail("export_binding")
	}
	inputs, e := runner.InspectReferencePreparationInputs(ctx, taskDir)
	if e != nil || inputs.SpecDigest != v.QualifiedInputIdentity.SpecDigest || inputs.InputsDigest != v.QualifiedInputIdentity.InputsDigest {
		return fail("derived_input_binding")
	}
	request := referenceRequest{Cases: q.Cases(), ReadBoundary: referenceReadBoundary(), Version: ReferencePreparationVersion, Authority: "unknown", Writer: referenceBridgeIdentity(), DefinitionRef: req.Definition, Definition: v, Policy: info, Inputs: inputs, ExportInputs: task.Inputs, CreatedAt: now.UTC().Format(time.RFC3339Nano), StartedAt: anchor.UTC().Format(time.RFC3339Nano)}
	if referencePolicyWindow(info, workEnd) != nil {
		return fail("policy_window_refused")
	}
	rref, e := writeReferenceRecord(ctx, s, base, "request", request)
	if e != nil {
		return fail("request_commit")
	}
	reservation := referenceReservation{Version: ReferencePreparationVersion, State: "reserved", Authority: "unknown", Writer: referenceBridgeIdentity(), Request: rref, ObservedAbsent: absent, HistorySource: definitionSourceIdentity(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ref, e := writeReferenceRecord(ctx, s, base, "reservation", reservation)
	if e != nil {
		return fail("reservation_commit")
	}
	return nil, &ReferencePreparationAttempt{reservation: ref}, taskDir, base, nil
}
