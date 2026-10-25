package project

// Generic local input records only. Neither creation nor reload creates a unit,
// proves prospective order, or authorizes source capture/verification/completion.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"sort"
	"sync"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const WorkDefinitionInputsVersion = "project-work-definition-inputs-v1"
const WorkDefinitionRecordMax int64 = 1 << 20
const definitionStateMax int64 = 64 << 20
const definitionMetadataMax int64 = 8 << 20
const definitionEntriesMax = 1024

var ErrWorkDefinition = errors.New("work definition inputs refused")

//go:embed work_definition.go work_definition_history.go
var definitionSources embed.FS

func definitionSourceIdentity() string {
	h := sha256.New()
	for _, n := range []string{"work_definition.go", "work_definition_history.go"} {
		b, _ := definitionSources.ReadFile(n)
		h.Write(b)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
func definitionProblem(ctx context.Context, code string) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(ErrWorkDefinition, ctx.Err())
	}
	return fmt.Errorf("%w: %s", ErrWorkDefinition, code)
}

// DefinitionRequest selects exact stored input bytes. Configuration is declared
// input only; it is never an effective launch observation. No handle/authority flag.
type DefinitionRequest struct {
	Label                 string
	ExactSetBytes         []byte
	PlanID                string
	Family                string
	DeclaredConfiguration activity.Configuration
	QualifiedIO           qualify.ResolveIORequest
}
type WorkDefinitionRef struct {
	RecordRef    string `json:"record_ref"`
	RecordDigest string `json:"record_digest"`
}
type DefinitionPlanBinding struct {
	SetID              string `json:"set_id"`
	SetContentDigest   string `json:"set_content_digest"`
	PlanID             string `json:"plan_id"`
	RepoID             string `json:"repo_id"`
	BaseSnapshotID     string `json:"base_snapshot_id"`
	BaseSnapshotDigest string `json:"base_snapshot_digest"`
}
type DefinitionObject struct {
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
}
type DefinitionSnapshotInput struct {
	RepoID          string `json:"repo_id"`
	SnapshotObject  string `json:"snapshot_object"`
	LatestRefObject string `json:"latest_ref_object"`
}
type WorkDefinitionIdentity struct {
	Kind                        string                    `json:"kind"`
	Basis                       string                    `json:"basis"`
	ResolutionState             string                    `json:"resolution_state"`
	RecordRef                   string                    `json:"record_ref"`
	Namespace                   string                    `json:"namespace"`
	UnitRef                     string                    `json:"unit_ref"`
	StoreBinding                string                    `json:"store_binding"`
	DefinitionBindingDigest     string                    `json:"definition_binding_digest,omitempty"`
	PlanBinding                 DefinitionPlanBinding     `json:"plan_binding"`
	FamilyRef                   string                    `json:"family_ref"`
	DeclaredConfigurationDigest string                    `json:"declared_configuration_digest"`
	DeclaredConfiguration       activity.Configuration    `json:"declared_configuration"`
	QualifiedInputRequest       qualify.ResolveIORequest  `json:"qualified_input_request"`
	QualifiedInputIdentity      qualify.IOInputIdentity   `json:"qualified_input_identity"`
	QualifiedObjects            []DefinitionObject        `json:"qualified_objects"`
	ResolverSourceIdentity      string                    `json:"resolver_source_identity"`
	WriterSourceIdentity        string                    `json:"writer_source_identity"`
	BoundedReaderIdentity       string                    `json:"bounded_reader_identity"`
	RawSetObject                string                    `json:"raw_set_object"`
	ExportStateObject           string                    `json:"export_state_object"`
	SnapshotInputs              []DefinitionSnapshotInput `json:"snapshot_inputs"`
	InputValidatedAt            string                    `json:"input_validated_at"`
	ObservedAbsentDefaults      []string                  `json:"observed_absent_defaults"`
}

// WorkDefinitionInputs has no prospective, complete, qualified or accepted state.
// Identity returns a copy. A ref+digest always resolves exact frozen input facts.
type WorkDefinitionInputs struct{ identity WorkDefinitionIdentity }

func (h *WorkDefinitionInputs) Identity() WorkDefinitionIdentity {
	b, _ := json.Marshal(h.identity)
	var v WorkDefinitionIdentity
	_ = json.Unmarshal(b, &v)
	return v
}

var definitionConfigOnce sync.Once
var definitionConfigSchema *jsonschema.Schema
var definitionConfigError error

func definitionConfigurationValid(c activity.Configuration) bool {
	// Bound typed strings/slices before JSON allocation. Semantics use the existing schema.
	var bounded func(reflect.Value, int) bool
	bounded = func(v reflect.Value, d int) bool {
		if d > 16 {
			return false
		}
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				return true
			}
			return bounded(v.Elem(), d+1)
		case reflect.String:
			return v.Len() <= 256
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !bounded(v.Field(i), d+1) {
					return false
				}
			}
		case reflect.Slice:
			if v.Len() > 100 {
				return false
			}
			for i := 0; i < v.Len(); i++ {
				if !bounded(v.Index(i), d+1) {
					return false
				}
			}
		}
		return true
	}
	if !bounded(reflect.ValueOf(c), 0) {
		return false
	}
	definitionConfigOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(githint.SchemaEvalReportV031))
		if e != nil {
			definitionConfigError = e
			return
		}
		if e = compiler.AddResource("urn:hint:definition:configuration", doc); e != nil {
			definitionConfigError = e
			return
		}
		definitionConfigSchema, definitionConfigError = compiler.Compile("urn:hint:definition:configuration#/$defs/Configuration")
	})
	if definitionConfigError != nil {
		return false
	}
	raw, e := json.Marshal(c)
	if e != nil {
		return false
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	return e == nil && definitionConfigSchema.Validate(doc) == nil
}
func definitionStoreBinding(ctx context.Context, s *store.Store) (string, error) {
	root, e := s.StateRootIdentity(ctx)
	if e != nil {
		return "", e
	}
	return hint.DeriveID(s.Key(), "work-definition-store", root, s.Root, s.Cfg.KeyID, s.Cfg.ProjectNamespace), nil
}
func definitionBinding(v WorkDefinitionIdentity) (string, error) {
	v.DefinitionBindingDigest = ""
	return hint.Digest(v)
}
func definitionDecode(raw []byte, v any) error {
	if !uniqueBatchJSON(raw) {
		return ErrWorkDefinition
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return ErrWorkDefinition
	}
	return nil
}

type definitionStateRead struct {
	ctx  context.Context
	s    *store.Store
	used int64
}

func (r *definitionStateRead) read(limit int64, elem ...string) ([]byte, error) {
	left := definitionStateMax - r.used
	if limit > left {
		limit = left
	}
	if limit < 1 {
		return nil, ErrWorkDefinition
	}
	b, e := r.s.ReadStateBounded(r.ctx, limit, elem...)
	if e == nil {
		r.used += int64(len(b))
	}
	return b, e
}
func (r *definitionStateRead) dir(elem ...string) ([]fs.DirEntry, error) {
	return r.s.ReadStateDirBounded(r.ctx, definitionEntriesMax, elem...)
}

func definitionBase(s *store.Store, now time.Time, exports []byte, inputs []DefinitionSnapshotInput, objects map[string][]byte) (*Current, *exportState, error) {
	var st exportState
	if definitionDecode(exports, &st) != nil || st.SnapshotDigests == nil {
		return nil, nil, ErrWorkDefinition
	}
	base := &Current{Repos: map[string]*CurrentRepo{}, Names: map[string]string{}, Local: hint.LocalState{SupportedVersions: version.Supported, ProjectNamespace: s.Cfg.ProjectNamespace, Now: now.UTC().Format(time.RFC3339Nano), SnapshotDigests: map[string]string{}, SnapshotDigestSets: map[string][]string{}, Resolvable: []string{}, Ambiguous: []string{}}}
	if len(inputs) != len(s.Cfg.Repos) || len(inputs) == 0 || len(inputs) > 16 {
		return nil, nil, ErrWorkDefinition
	}
	for i, in := range inputs {
		repo := s.Cfg.Repos[i]
		if in.RepoID != s.RepoID(repo) {
			return nil, nil, ErrWorkDefinition
		}
		var latest latestRef
		var snap snapshot.Local
		if definitionDecode(objects[in.LatestRefObject], &latest) != nil || definitionDecode(objects[in.SnapshotObject], &snap) != nil {
			return nil, nil, ErrWorkDefinition
		}
		if !batchID.MatchString(latest.SnapshotID) || snap.Record.SnapshotID != latest.SnapshotID || snap.Record.RepoID != in.RepoID {
			return nil, nil, ErrWorkDefinition
		}
		dg, e := snapshot.Digest(snap.Record, snap.Entities, snap.Relations)
		if e != nil || dg != snap.Record.Digest {
			return nil, nil, ErrWorkDefinition
		}
		if len(st.SnapshotDigests[latest.SnapshotID]) == 0 {
			return nil, nil, ErrWorkDefinition
		}
		base.Repos[in.RepoID] = &CurrentRepo{Name: repo.Name, Snapshot: &snap, Stored: true}
		base.Local.SnapshotDigests[in.RepoID] = snap.Record.Digest
		base.Local.SnapshotDigestSets[latest.SnapshotID] = st.SnapshotDigests[latest.SnapshotID]
		for _, entity := range snap.Entities {
			if entity.RepoID != in.RepoID || entity.SnapshotID != latest.SnapshotID {
				return nil, nil, ErrWorkDefinition
			}
			base.Local.Resolvable = append(base.Local.Resolvable, entity.ID)
		}
		for id := range snap.Ambiguous {
			base.Local.Ambiguous = append(base.Local.Ambiguous, id)
		}
	}
	sort.Strings(base.Local.Resolvable)
	sort.Strings(base.Local.Ambiguous)
	return base, &st, nil
}
func definitionPlan(pr planRef) DefinitionPlanBinding {
	return DefinitionPlanBinding{pr.SetID, pr.SetDigest, pr.PlanID, pr.RepoID, pr.SnapshotID, pr.SnapshotDigest}
}
func definitionQualifiedObjects(h *qualify.ResolvedQualifiedIO) []DefinitionObject {
	out := []DefinitionObject{}
	for _, d := range h.ObjectDigests() {
		b, _ := h.Object(d)
		out = append(out, DefinitionObject{d, int64(len(b))})
	}
	return out
}

// WriteWorkDefinitionInputs freezes exact STORED input consistency only. Creation
// and identical create-replay both inspect current history. After a unit/epoch or
// ambiguous/prior activity exists, creation refuses even if an old record matches.
// Use LoadWorkDefinitionInputs for later factual reload; it never recertifies order.
func WriteWorkDefinitionInputs(parent context.Context, s *store.Store, request DefinitionRequest, now time.Time) (WorkDefinitionRef, error) {
	zero := WorkDefinitionRef{}
	if parent == nil || s == nil {
		return zero, definitionProblem(parent, "request_invalid")
	}
	ctx, cancel := context.WithTimeout(parent, qualify.ResolveTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return zero, definitionProblem(ctx, "deadline")
	}
	if !ValidTaskID(request.Label) || !ValidTaskID(request.Family) || !batchID.MatchString(request.PlanID) || len(request.ExactSetBytes) == 0 || len(request.ExactSetBytes) > MaxDocumentBytes || !definitionConfigurationValid(request.DeclaredConfiguration) || now.IsZero() || !batchID.MatchString(s.Cfg.ProjectNamespace) || len(s.Key()) == 0 {
		return zero, definitionProblem(ctx, "request_invalid")
	}
	// Copy mutable request material before any state query.
	request.ExactSetBytes = append([]byte(nil), request.ExactSetBytes...)
	cfg, _ := json.Marshal(request.DeclaredConfiguration)
	_ = json.Unmarshal(cfg, &request.DeclaredConfiguration)
	lock, e := s.TryLockStateContext(ctx)
	if e != nil {
		if errors.Is(e, store.ErrBusy) {
			return zero, errors.Join(ErrWorkDefinition, store.ErrBusy)
		}
		return zero, definitionProblem(ctx, "state_lock_refused")
	}
	defer lock.Unlock()
	unit := hint.EvalRef(s.Key(), "work-unit", request.Label)
	ref := hint.EvalRef(s.Key(), WorkDefinitionInputsVersion, unit)
	r := &definitionStateRead{ctx: ctx, s: s}
	absent, e := definitionHistory(r, request.Label, unit)
	if e != nil {
		return zero, definitionProblem(ctx, "history_refused")
	}
	path := []string{"state", library.WorkDefinitionStateDir, ref + ".json"}
	if old, err := r.read(WorkDefinitionRecordMax, path...); err == nil {
		existing := WorkDefinitionRef{ref, library.Digest(old)}
		h, e := loadWorkDefinition(ctx, s, existing)
		if e != nil {
			return zero, e
		}
		v := h.identity
		if v.UnitRef != unit || v.FamilyRef != activity.FamilyRef(s.Key(), request.Family) || v.DeclaredConfigurationDigest != request.DeclaredConfiguration.Digest() || !reflect.DeepEqual(v.DeclaredConfiguration, request.DeclaredConfiguration) || v.PlanBinding.PlanID != request.PlanID || v.RawSetObject != library.Digest(request.ExactSetBytes) || v.QualifiedInputRequest != request.QualifiedIO {
			return zero, definitionProblem(ctx, "immutable_conflict")
		}
		return existing, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return zero, definitionProblem(ctx, "record_unreadable")
	}
	objects := map[string][]byte{}
	add := func(b []byte) string { d := library.Digest(b); objects[d] = b; return d }
	exp, e := r.read(definitionMetadataMax, "state", "exports", "state.json")
	if e != nil {
		return zero, definitionProblem(ctx, "stored_base_missing")
	}
	exportObject := add(exp)
	snapshots := []DefinitionSnapshotInput{}
	if len(s.Cfg.Repos) == 0 || len(s.Cfg.Repos) > 16 {
		return zero, definitionProblem(ctx, "stored_base_missing")
	}
	for _, repo := range s.Cfg.Repos {
		rid := s.RepoID(repo)
		raw, e := r.read(definitionMetadataMax, "state", "snapshots", rid, "latest.json")
		if e != nil {
			return zero, definitionProblem(ctx, "stored_base_missing")
		}
		var latest latestRef
		if definitionDecode(raw, &latest) != nil || !batchID.MatchString(latest.SnapshotID) {
			return zero, definitionProblem(ctx, "stored_base_invalid")
		}
		snap, e := r.read(definitionMetadataMax, "state", "snapshots", rid, latest.SnapshotID+".json")
		if e != nil {
			return zero, definitionProblem(ctx, "stored_base_missing")
		}
		snapshots = append(snapshots, DefinitionSnapshotInput{rid, add(snap), add(raw)})
	}
	base, st, e := definitionBase(s, now, exp, snapshots, objects)
	if e != nil {
		return zero, definitionProblem(ctx, "stored_base_invalid")
	}
	pr, e := resolvePlanForBase(s, request.ExactSetBytes, request.PlanID, base, func() (*exportState, error) { return st, nil })
	if e != nil {
		return zero, definitionProblem(ctx, "plan_input_binding_refused")
	}
	if base.Repos[pr.RepoID].Snapshot.Record.SnapshotID != pr.SnapshotID {
		return zero, definitionProblem(ctx, "plan_base_mismatch")
	}
	l := library.Open(s, 0)
	// The state lock excludes destructive library retention before this reservation.
	// An incomplete/failed reservation stays fail-closed; never auto-delete evidence.
	if e = s.EnsureStateDirContext(ctx, "state", library.WorkDefinitionStateDir); e != nil {
		return zero, definitionProblem(ctx, "reference_retention_refused")
	}
	q, refusal := qualify.ResolveQualifiedIO(ctx, l, request.QualifiedIO)
	if refusal != nil {
		return zero, definitionProblem(ctx, "qualified_inputs_refused")
	}
	binding, e := definitionStoreBinding(ctx, s)
	if e != nil {
		return zero, definitionProblem(ctx, "store_binding_refused")
	}
	value := WorkDefinitionIdentity{Kind: WorkDefinitionInputsVersion, Basis: "stored_input_consistency_only", ResolutionState: "resolved_inputs", RecordRef: ref, Namespace: s.Cfg.ProjectNamespace, UnitRef: unit, StoreBinding: binding, PlanBinding: definitionPlan(pr), FamilyRef: activity.FamilyRef(s.Key(), request.Family), DeclaredConfigurationDigest: request.DeclaredConfiguration.Digest(), DeclaredConfiguration: request.DeclaredConfiguration, QualifiedInputRequest: request.QualifiedIO, QualifiedInputIdentity: q.Identity(), QualifiedObjects: definitionQualifiedObjects(q), ResolverSourceIdentity: qualify.QualifiedInputSourceIdentity(), WriterSourceIdentity: definitionSourceIdentity(), BoundedReaderIdentity: store.BoundedStateSourceIdentity(), RawSetObject: add(request.ExactSetBytes), ExportStateObject: exportObject, SnapshotInputs: snapshots, InputValidatedAt: now.UTC().Format(time.RFC3339Nano), ObservedAbsentDefaults: absent}
	for _, d := range q.ObjectDigests() {
		b, _ := q.Object(d)
		objects[d] = b
	}
	for d, b := range objects {
		got, e := l.PutObjectContext(ctx, b)
		if e != nil || got != d {
			return zero, definitionProblem(ctx, "source_object_write_refused")
		}
	}
	value.DefinitionBindingDigest, e = definitionBinding(value)
	if e != nil {
		return zero, definitionProblem(ctx, "record_invalid")
	}
	raw, e := hint.Canonical(value)
	if e != nil || int64(len(raw)) > WorkDefinitionRecordMax {
		return zero, definitionProblem(ctx, "record_limit")
	}
	if _, e = definitionHistory(r, request.Label, unit); e != nil {
		return zero, definitionProblem(ctx, "history_refused")
	}
	if e = s.WriteStateImmutableContext(ctx, raw, WorkDefinitionRecordMax, path...); e != nil {
		return zero, definitionProblem(ctx, "record_commit_refused")
	}
	return WorkDefinitionRef{ref, library.Digest(raw)}, nil
}

// LoadWorkDefinitionInputs reloads ONLY exact immutable input consistency. It does
// not inspect current work history, select latest inputs or certify prospective start.
func LoadWorkDefinitionInputs(parent context.Context, s *store.Store, ref WorkDefinitionRef) (*WorkDefinitionInputs, error) {
	if parent == nil || s == nil {
		return nil, definitionProblem(parent, "reference_invalid")
	}
	ctx, cancel := context.WithTimeout(parent, qualify.ResolveTimeout)
	defer cancel()
	return loadWorkDefinition(ctx, s, ref)
}
func loadWorkDefinition(ctx context.Context, s *store.Store, ref WorkDefinitionRef) (*WorkDefinitionInputs, error) {
	if !batchID.MatchString(ref.RecordRef) || !digestPattern.MatchString(ref.RecordDigest) {
		return nil, definitionProblem(ctx, "reference_invalid")
	}
	raw, e := s.ReadStateBounded(ctx, WorkDefinitionRecordMax, "state", library.WorkDefinitionStateDir, ref.RecordRef+".json")
	if e != nil || library.Digest(raw) != ref.RecordDigest {
		return nil, definitionProblem(ctx, "record_unavailable_or_changed")
	}
	var v WorkDefinitionIdentity
	if definitionDecode(raw, &v) != nil {
		return nil, definitionProblem(ctx, "record_invalid")
	}
	canonical, e := hint.Canonical(v)
	if e != nil || !bytes.Equal(canonical, raw) {
		return nil, definitionProblem(ctx, "record_invalid")
	}
	storeBinding, e := definitionStoreBinding(ctx, s)
	if e != nil {
		return nil, definitionProblem(ctx, "store_binding_refused")
	}
	dg, e := definitionBinding(v)
	if e != nil || dg != v.DefinitionBindingDigest || v.Kind != WorkDefinitionInputsVersion || v.Basis != "stored_input_consistency_only" || v.ResolutionState != "resolved_inputs" || v.Namespace != s.Cfg.ProjectNamespace || v.StoreBinding != storeBinding || v.RecordRef != ref.RecordRef || hint.EvalRef(s.Key(), WorkDefinitionInputsVersion, v.UnitRef) != ref.RecordRef || !batchID.MatchString(v.UnitRef) || !batchID.MatchString(v.FamilyRef) || !definitionConfigurationValid(v.DeclaredConfiguration) || v.DeclaredConfiguration.Digest() != v.DeclaredConfigurationDigest || v.WriterSourceIdentity != definitionSourceIdentity() || v.ResolverSourceIdentity != qualify.QualifiedInputSourceIdentity() || v.BoundedReaderIdentity != store.BoundedStateSourceIdentity() {
		return nil, definitionProblem(ctx, "record_binding_refused")
	}
	if len(v.SnapshotInputs) == 0 || len(v.SnapshotInputs) > 16 || len(v.SnapshotInputs) != len(s.Cfg.Repos) || len(v.ObservedAbsentDefaults) > 6 {
		return nil, definitionProblem(ctx, "record_shape_refused")
	}
	allowedAbsent := map[string]bool{"work_directory": true, "membership_directory": true, "activity_sources": true, "activity_spool": true, "activity_batches": true, "controlled_run_index": true}
	previous := ""
	for _, name := range v.ObservedAbsentDefaults {
		if !allowedAbsent[name] || name <= previous {
			return nil, definitionProblem(ctx, "record_shape_refused")
		}
		previous = name
	}
	l := library.Open(s, 0)
	q, why := qualify.ResolveQualifiedIO(ctx, l, v.QualifiedInputRequest)
	if why != nil || !reflect.DeepEqual(q.Identity(), v.QualifiedInputIdentity) || !reflect.DeepEqual(definitionQualifiedObjects(q), v.QualifiedObjects) {
		return nil, definitionProblem(ctx, "qualified_inputs_refused")
	}
	objects := map[string][]byte{}
	var frozenReadBytes int64
	read := func(d string, cap int64) error {
		if _, ok := objects[d]; ok {
			return nil
		}
		if left := int64(80<<20) - frozenReadBytes; cap > left {
			cap = left
		}
		if cap < 1 {
			return ErrWorkDefinition
		}
		if !digestPattern.MatchString(d) {
			return ErrWorkDefinition
		}
		b, e := l.ResolveBoundedRead(ctx, "object", d, cap)
		if e == nil {
			objects[d] = b
			frozenReadBytes += int64(len(b))
		}
		return e
	}
	if read(v.RawSetObject, MaxDocumentBytes) != nil || read(v.ExportStateObject, definitionMetadataMax) != nil {
		return nil, definitionProblem(ctx, "frozen_input_unavailable")
	}
	for _, in := range v.SnapshotInputs {
		if read(in.SnapshotObject, definitionMetadataMax) != nil || read(in.LatestRefObject, definitionMetadataMax) != nil {
			return nil, definitionProblem(ctx, "frozen_input_unavailable")
		}
	}
	at, e := time.Parse(time.RFC3339Nano, v.InputValidatedAt)
	if e != nil || at.IsZero() {
		return nil, definitionProblem(ctx, "record_invalid")
	}
	base, st, e := definitionBase(s, at, objects[v.ExportStateObject], v.SnapshotInputs, objects)
	if e != nil {
		return nil, definitionProblem(ctx, "frozen_base_invalid")
	}
	pr, e := resolvePlanForBase(s, objects[v.RawSetObject], v.PlanBinding.PlanID, base, func() (*exportState, error) { return st, nil })
	if e != nil || definitionPlan(pr) != v.PlanBinding {
		return nil, definitionProblem(ctx, "frozen_plan_binding_refused")
	}
	if ctx.Err() != nil {
		return nil, definitionProblem(ctx, "deadline")
	}
	return &WorkDefinitionInputs{identity: v}, nil
}
