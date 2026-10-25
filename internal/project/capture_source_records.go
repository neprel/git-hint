package project

// These records and raw objects stay local. Consistency is not source authority.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/readgit"
	"github.com/neprel/git-hint/internal/store"
)

const CaptureSourceV2 = "project-capture-source-v2"
const CaptureSourceVersion = "project-capture-source-v1"
const capturePolicyVersion = "project-capture-policy-v1"
const captureReservationVersion = "project-capture-source-reservation-v1"
const captureRecordMax int64 = 8 << 20
const captureSmallMax int64 = 1 << 20

type CaptureSourceRef struct {
	RecordRef    string `json:"record_ref"`
	RecordDigest string `json:"record_digest"`
}
type CapturePolicyRef = CaptureSourceRef
type CaptureReservationRef = CaptureSourceRef

type capturePolicy struct {
	Kind          string                 `json:"kind"`
	RecordRef     string                 `json:"record_ref"`
	DefinitionRef WorkDefinitionRef      `json:"definition_ref"`
	Definition    WorkDefinitionIdentity `json:"definition"`
	RootBinding   string                 `json:"configured_root_binding"`
	ConfigObject  DefinitionObject       `json:"store_config_object"`
	Limits        trees.Limits           `json:"effective_limits"`
	Selector      string                 `json:"selector"`
	Scope         string                 `json:"scope"`
	Reader        string                 `json:"reader_policy"`
	Repeat        string                 `json:"repeat_policy"`
	Invocation    string                 `json:"invocation_identity"`
	Writer        string                 `json:"implementation_bundle"`
	Basis         string                 `json:"basis"`
	Authority     string                 `json:"authority"`
	CreatedAt     string                 `json:"created_at"`
}
type captureReservation struct {
	Kind            string            `json:"kind"`
	RecordRef       string            `json:"record_ref"`
	PlannedAuditRef string            `json:"planned_audit_ref"`
	DefinitionRef   WorkDefinitionRef `json:"definition_ref"`
	PolicyRef       CapturePolicyRef  `json:"policy_ref"`
	UnitRef         string            `json:"unit_ref"`
	StoreBinding    string            `json:"store_binding"`
	Namespace       string            `json:"namespace"`
	RootBinding     string            `json:"configured_root_binding"`
	Writer          string            `json:"implementation_bundle"`
	ObservedAbsent  []string          `json:"observed_absent_before"`
	State           string            `json:"state"`
	Authority       string            `json:"authority"`
	CreatedAt       string            `json:"created_at"`
}
type CaptureSourceObject struct {
	Digest string   `json:"digest"`
	Bytes  int64    `json:"bytes"`
	Roles  []string `json:"roles"`
}
type CaptureSourceBracket struct {
	Phase   string `json:"phase"`
	BeginNS int64  `json:"begin_ns"`
	EndNS   int64  `json:"end_ns"`
}

// CaptureSourceIdentity records local observations only. Inner reader offsets and
// outer operation brackets have DISTINCT private origins; no conversion is exposed.
type CaptureSourceIdentity struct {
	Kind                                string                      `json:"kind"`
	RecordRef                           string                      `json:"record_ref"`
	DefinitionRef                       WorkDefinitionRef           `json:"definition_ref"`
	PolicyRef                           CapturePolicyRef            `json:"policy_ref"`
	ReservationRef                      CaptureReservationRef       `json:"reservation_ref"`
	Definition                          WorkDefinitionIdentity      `json:"definition"`
	RootBinding                         string                      `json:"configured_root_binding"`
	RootIdentity                        string                      `json:"observed_root_identity"`
	ReceiptObjects                      []DefinitionObject          `json:"receipt_objects"`
	Objects                             []CaptureSourceObject       `json:"object_inventory"`
	ObservedAbsentBefore                []string                    `json:"observed_absent_before"`
	ObservedAbsentAfter                 []string                    `json:"observed_absent_after"`
	SharedStateReadBytesAtCommitPrepare int64                       `json:"shared_state_returned_read_bytes_at_commit_prepare"`
	OuterInvocation                     string                      `json:"outer_invocation_identity"`
	OuterStats                          readgit.SourceDispatchStats `json:"outer_dispatch_stats_at_commit_prepare"`
	ClockScope                          string                      `json:"producer_clock_scope_ref"`
	Brackets                            []CaptureSourceBracket      `json:"completed_producer_call_brackets"`
	CommitPrepareNS                     int64                       `json:"commit_prepare_begin_ns"`
	State                               string                      `json:"state"`
	Basis                               string                      `json:"basis"`
	Authority                           string                      `json:"authority"`
	Writer                              string                      `json:"implementation_bundle"`
	CreatedAt                           string                      `json:"created_at"`
}
type CaptureSourceInputs struct {
	identity CaptureSourceIdentity
	objects  map[string][]byte
}

func (h *CaptureSourceInputs) Identity() CaptureSourceIdentity {
	if h == nil {
		return CaptureSourceIdentity{}
	}
	b, _ := json.Marshal(h.identity)
	var v CaptureSourceIdentity
	_ = json.Unmarshal(b, &v)
	return v
}
func (h *CaptureSourceInputs) Object(d string) ([]byte, bool) {
	if h == nil {
		return nil, false
	}
	b, ok := h.objects[d]
	return append([]byte(nil), b...), ok
}

func captureDecode(raw []byte, v any) error {
	if definitionDecode(raw, v) != nil {
		return ErrCaptureSource
	}
	b, e := hint.Canonical(v)
	if e != nil || !bytes.Equal(b, raw) {
		return ErrCaptureSource
	}
	return nil
}
func captureWrite(ctx context.Context, s *store.Store, dir string, limit int64, v any, ref string) (CaptureSourceRef, error) {
	raw, e := hint.Canonical(v)
	if e != nil || int64(len(raw)) > limit {
		return CaptureSourceRef{}, captureProblem(ctx, "record_limit")
	}
	if e = s.WriteStateImmutableContext(ctx, raw, limit, "state", dir, ref+".json"); e != nil {
		return CaptureSourceRef{}, captureProblem(ctx, "immutable_commit_refused")
	}
	return CaptureSourceRef{ref, library.Digest(raw)}, nil
}
func captureRead(ctx context.Context, s *store.Store, dir string, limit int64, ref CaptureSourceRef, v any) error {
	if !batchID.MatchString(ref.RecordRef) || !digestPattern.MatchString(ref.RecordDigest) {
		return ErrCaptureSource
	}
	raw, e := s.ReadStateBounded(ctx, limit, "state", dir, ref.RecordRef+".json")
	if e != nil || library.Digest(raw) != ref.RecordDigest || captureDecode(raw, v) != nil {
		return ErrCaptureSource
	}
	return nil
}
func capturePolicyID(s *store.Store, p capturePolicy) string {
	p.RecordRef = ""
	p.CreatedAt = ""
	d, _ := hint.Digest(p)
	return hint.EvalRef(s.Key(), capturePolicyVersion, p.DefinitionRef.RecordRef, d)
}
func captureOperationID(s *store.Store, kind string, definition WorkDefinitionRef, policy CapturePolicyRef) string {
	return hint.EvalRef(s.Key(), kind, definition.RecordRef, policy.RecordRef)
}

// LoadCaptureSource is exact factual reload only. It never mints an Acceptance,
// checks current source/history or grants replay, start, catalogue or origin authority.
func LoadCaptureSource(parent context.Context, s *store.Store, ref CaptureSourceRef) (*CaptureSourceInputs, error) {
	if parent == nil || s == nil {
		return nil, ErrCaptureSource
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return loadCaptureSource(ctx, s, ref)
}
func loadCaptureSource(ctx context.Context, s *store.Store, ref CaptureSourceRef) (*CaptureSourceInputs, error) {
	var a CaptureSourceIdentity
	if captureRead(ctx, s, library.CaptureSourceStateDir, captureRecordMax, ref, &a) != nil {
		return nil, captureProblem(ctx, "audit_unavailable")
	}
	var p capturePolicy
	var r captureReservation
	if captureRead(ctx, s, library.CapturePolicyStateDir, captureSmallMax, a.PolicyRef, &p) != nil || captureRead(ctx, s, library.CaptureReservationStateDir, captureSmallMax, a.ReservationRef, &r) != nil {
		return nil, captureProblem(ctx, "dependencies_unavailable")
	}
	def, _, e := loadWorkDefinitionResolved(ctx, s, a.DefinitionRef)
	if e != nil {
		return nil, captureProblem(ctx, "definition_unavailable")
	}
	v := def.Identity()
	lim, e := trees.ResolveStrictCaptureLimits(p.Limits)
	if e != nil || lim != p.Limits || p.Kind != capturePolicyVersion || p.RecordRef != a.PolicyRef.RecordRef || p.RecordRef != capturePolicyID(s, p) || p.DefinitionRef != a.DefinitionRef || !reflect.DeepEqual(p.Definition, v) || p.Writer != captureSourceIdentity() || p.Authority != "unknown" || p.Basis != "local_selected_source_policy_only" || p.Selector != "git-selected-capture-source-v1" || p.Scope != "git_selected_artifact_v1" || p.Reader != trees.SourceViewVersion || p.Repeat != "two_pass_exact_local_observation" || !digestPattern.MatchString(p.Invocation) || !batchID.MatchString(p.RootBinding) || !digestPattern.MatchString(p.ConfigObject.Digest) || p.ConfigObject.Bytes < 1 || p.ConfigObject.Bytes > captureSmallMax {
		return nil, captureProblem(ctx, "policy_binding")
	}
	if r.Kind != captureReservationVersion || r.RecordRef != a.ReservationRef.RecordRef || r.RecordRef != captureOperationID(s, captureReservationVersion, a.DefinitionRef, a.PolicyRef) || r.PlannedAuditRef != ref.RecordRef || r.DefinitionRef != a.DefinitionRef || r.PolicyRef != a.PolicyRef || r.UnitRef != v.UnitRef || r.StoreBinding != v.StoreBinding || r.Namespace != v.Namespace || r.RootBinding != p.RootBinding || r.Writer != p.Writer || r.State != "reserved" || r.Authority != "unknown" || r.CreatedAt != p.CreatedAt {
		return nil, captureProblem(ctx, "reservation_binding")
	}
	if a.Kind != captureVersion(v) || a.RecordRef != ref.RecordRef || a.RecordRef != captureOperationID(s, captureVersion(v), a.DefinitionRef, a.PolicyRef) || !reflect.DeepEqual(a.Definition, v) || a.RootBinding != p.RootBinding || a.Writer != p.Writer || a.OuterInvocation != p.Invocation || a.Authority != "unknown" || a.State != "observed_selected_source_consistent" || a.Basis != "repeated_local_selected_artifact_observation" || a.ClockScope != a.RecordRef || a.CreatedAt != p.CreatedAt || !reflect.DeepEqual(a.ObservedAbsentBefore, r.ObservedAbsent) || !reflect.DeepEqual(a.ObservedAbsentBefore, a.ObservedAbsentAfter) || !captureAbsentValid(a.ObservedAbsentBefore) || a.SharedStateReadBytesAtCommitPrepare < 0 || a.SharedStateReadBytesAtCommitPrepare > definitionStateMax || len(a.ReceiptObjects) != 4 {
		return nil, captureProblem(ctx, "audit_binding")
	}
	if t, e := time.Parse(time.RFC3339Nano, a.CreatedAt); e != nil || t.IsZero() {
		return nil, captureProblem(ctx, "time_invalid")
	}
	if !captureBracketsValid(a.Brackets, a.CommitPrepareNS) || a.OuterStats.Dispatches < 0 || a.OuterStats.Dispatches > readgit.SourceDispatchLimit || a.OuterStats.ExecutableReads < a.OuterStats.Dispatches+1 || a.OuterStats.ExecutableReads > readgit.SourceDispatchLimit+1 || a.OuterStats.ExecutableBytes < 0 || a.OuterStats.StdoutBytes < 0 || a.OuterStats.StderrBytes < 0 {
		return nil, captureProblem(ctx, "clock_or_stats")
	}
	l := library.Open(s, 0)
	objects := map[string][]byte{}
	wireRaw := make([][]byte, 4)
	for i, o := range a.ReceiptObjects {
		if o.Bytes < 1 || o.Bytes > captureRecordMax || !digestPattern.MatchString(o.Digest) {
			return nil, ErrCaptureSource
		}
		b, e := l.ResolveBoundedRead(ctx, "object", o.Digest, o.Bytes)
		if e != nil || int64(len(b)) != o.Bytes {
			return nil, captureProblem(ctx, "receipt_object")
		}
		wireRaw[i] = b
		objects[o.Digest] = b
	}
	pair, e := trees.DecodeSelectedSourcePair(p.Limits, trees.SelectedSourceWire{SelectedA: wireRaw[0], SelectedB: wireRaw[1], SourceA: wireRaw[2], SourceB: wireRaw[3]})
	if e != nil {
		return nil, captureProblem(ctx, "receipt_invalid")
	}
	sa, _, sx, sy := pair.Receipts()
	if sa.RootIdentity != a.RootIdentity || sx.InvocationIdentity != p.Invocation || sy.InvocationIdentity != p.Invocation {
		return nil, captureProblem(ctx, "receipt_binding")
	}
	rawObjects := map[string][]byte{}
	var used int64
	for _, o := range pair.ObjectRefs() {
		cap := o.MaxBytes
		if cap > 128<<20-used {
			cap = 128<<20 - used
		}
		if cap < 1 {
			return nil, ErrCaptureSource
		}
		b, e := l.ResolveBoundedRead(ctx, "object", o.Digest, cap)
		if e != nil {
			return nil, captureProblem(ctx, "raw_object")
		}
		used += int64(len(b))
		rawObjects[o.Digest] = b
		objects[o.Digest] = b
	}
	if _, e = pair.ValidateObjects(rawObjects); e != nil {
		return nil, captureProblem(ctx, "raw_object_invalid")
	}
	cfg, e := l.ResolveBoundedRead(ctx, "object", p.ConfigObject.Digest, p.ConfigObject.Bytes)
	if e != nil || int64(len(cfg)) != p.ConfigObject.Bytes {
		return nil, captureProblem(ctx, "config_object")
	}
	objects[p.ConfigObject.Digest] = cfg
	var cfgValue store.Config
	if json.Unmarshal(cfg, &cfgValue) != nil || cfgValue.ProjectNamespace != v.Namespace {
		return nil, captureProblem(ctx, "config_object")
	}
	root, e := captureConfiguredRoot(s, cfgValue, v.PlanBinding.RepoID)
	if e != nil || captureRootBinding(s, v, root) != p.RootBinding {
		return nil, captureProblem(ctx, "root_binding")
	}
	inventory, e := captureInventory(pair, a.ReceiptObjects, p.ConfigObject, objects)
	if e != nil || !reflect.DeepEqual(inventory, a.Objects) {
		return nil, captureProblem(ctx, "object_inventory")
	}
	if ctx.Err() != nil {
		return nil, captureProblem(ctx, "deadline")
	}
	if e = compareBaselineSource(ctx, s, v, pair, objects); e != nil {
		return nil, e
	}
	return &CaptureSourceInputs{a, objects}, nil
}
func captureInventory(pair *trees.DecodedSelectedSourcePair, receipts []DefinitionObject, config DefinitionObject, objects map[string][]byte) ([]CaptureSourceObject, error) {
	roles := map[string][]string{}
	add := func(d, role string) {
		for _, old := range roles[d] {
			if old == role {
				return
			}
		}
		roles[d] = append(roles[d], role)
	}
	a, _, x, _ := pair.Receipts()
	add(a.TreeDigest, "selected_tree")
	add(x.ConfigObject, "git_config_query")
	add(x.StateObject, "git_source_state")
	for _, f := range a.Selected {
		if f.State == trees.SelectedRegularPresent {
			add(f.ObjectDigest, "selected_content")
		}
	}
	for _, f := range x.Facts {
		if f.State == "present_regular" {
			add(f.ObjectDigest, "source_metadata")
		}
	}
	for i, o := range receipts {
		add(o.Digest, []string{"selected_receipt_a", "selected_receipt_b", "source_receipt_a", "source_receipt_b"}[i])
	}
	add(config.Digest, "store_config")
	if len(roles) > 22059 || len(roles) != len(objects) {
		return nil, ErrCaptureSource
	}
	out := []CaptureSourceObject{}
	for d, r := range roles {
		b, ok := objects[d]
		if !ok || library.Digest(b) != d {
			return nil, ErrCaptureSource
		}
		sort.Strings(r)
		out = append(out, CaptureSourceObject{d, int64(len(b)), r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

// CREATE refuses any received matching operation, including incomplete allocation.
func captureAbsentOperation(r *definitionStateRead, definition WorkDefinitionRef) error {
	for _, dir := range []string{library.CapturePolicyStateDir, library.CaptureReservationStateDir, library.CaptureSourceStateDir} {
		entries, e := r.dir("state", dir)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			return ErrCaptureSource
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !batchID.MatchString(stringsTrimJSON(entry.Name())) {
				return ErrCaptureSource
			}
			limit := captureRecordMax
			if dir != library.CaptureSourceStateDir {
				limit = captureSmallMax
			}
			raw, e := r.read(limit, "state", dir, entry.Name())
			if e != nil {
				return e
			}
			var def WorkDefinitionRef
			switch dir {
			case library.CapturePolicyStateDir:
				var v capturePolicy
				if captureDecode(raw, &v) != nil {
					return ErrCaptureSource
				}
				def = v.DefinitionRef
			case library.CaptureReservationStateDir:
				var v captureReservation
				if captureDecode(raw, &v) != nil {
					return ErrCaptureSource
				}
				def = v.DefinitionRef
			default:
				var v CaptureSourceIdentity
				if captureDecode(raw, &v) != nil {
					return ErrCaptureSource
				}
				def = v.DefinitionRef
			}
			if !batchID.MatchString(def.RecordRef) || !digestPattern.MatchString(def.RecordDigest) || def.RecordRef == definition.RecordRef {
				return ErrCaptureSource
			}
		}
	}
	return nil
}
func stringsTrimJSON(s string) string {
	if len(s) < 6 || s[len(s)-5:] != ".json" {
		return ""
	}
	return s[:len(s)-5]
}

func captureAbsentValid(names []string) bool {
	if names == nil || len(names) > 6 {
		return false
	}
	allowed := map[string]bool{"work_directory": true, "membership_directory": true, "activity_sources": true, "activity_spool": true, "activity_batches": true, "controlled_run_index": true}
	previous := ""
	for _, name := range names {
		if !allowed[name] || name <= previous {
			return false
		}
		previous = name
	}
	return true
}
