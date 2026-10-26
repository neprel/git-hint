package project

// Baseline records are local source/content observations, never normal HINT
// payloads or observer authority. Loading data cannot recover process acceptance.
import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

const BaselineObservationVersion = "project-verified-baseline-v1"
const baselineSelection = "git_selected_regular_source_v1"

var ErrBaselineObservation = errors.New("baseline observation refused")

// Declared source/dependency family, not a complete binary attestation.
//
//go:embed selected_result_inputs.go baseline_collect.go baseline_records.go state.go collect.go capture_source.go capture_source_records.go capture_source_pair.go prework_acceptance.go
var baselineSources embed.FS

func baselineSourceIdentity() string {
	return captureSourceIdentityFrom(baselineSources, githint.CaptureSourceAssets)
}

type BaselineObservationRef struct {
	RecordRef    string `json:"record_ref"`
	RecordDigest string `json:"record_digest"`
}
type BaselinePhysicalBinding struct {
	TreeDigest      string `json:"tree_digest"`
	SelectionDigest string `json:"selection_digest"`
	StageDigest     string `json:"stage_digest"`
	SourceDigest    string `json:"source_digest"`
	GitStateObject  string `json:"git_state_object"`
	Selection       string `json:"selection"`
}
type BaselineBinding struct {
	Observation    BaselineObservationRef  `json:"observation"`
	RepoID         string                  `json:"repo_id"`
	SnapshotID     string                  `json:"snapshot_id"`
	SnapshotDigest string                  `json:"snapshot_digest"`
	Physical       BaselinePhysicalBinding `json:"physical"`
}
type BaselineObservationIdentity struct {
	Kind            string                  `json:"kind"`
	RecordRef       string                  `json:"record_ref"`
	Namespace       string                  `json:"namespace"`
	StoreBinding    string                  `json:"store_binding"`
	RepoID          string                  `json:"repo_id"`
	RootBinding     string                  `json:"root_binding"`
	RootIdentity    string                  `json:"root_identity"`
	ConfigObject    DefinitionObject        `json:"config_object"`
	Limits          trees.Limits            `json:"limits"`
	Receipts        []DefinitionObject      `json:"receipts"`
	Objects         []CaptureSourceObject   `json:"objects"`
	Physical        BaselinePhysicalBinding `json:"physical"`
	SnapshotID      string                  `json:"snapshot_id"`
	SnapshotDigest  string                  `json:"snapshot_digest"`
	SnapshotObject  DefinitionObject        `json:"snapshot_object"`
	ParentObject    *DefinitionObject       `json:"parent_object"`
	PreviousPointer DefinitionObject        `json:"previous_pointer"`
	ObservedAt      string                  `json:"observed_at"`
	DerivedAt       string                  `json:"derived_at"`
	BeforePublishNS int64                   `json:"before_publish_ns"`
	Writer          string                  `json:"writer"`
	Reader          string                  `json:"reader"`
	Scanner         string                  `json:"scanner"`
	State           string                  `json:"state"`
	Authority       string                  `json:"authority"`
}
type BaselineInputs struct {
	ref      BaselineObservationRef
	identity BaselineObservationIdentity
	objects  map[string][]byte
	local    snapshot.Local
	pair     *trees.DecodedSelectedSourcePair
}

func (h *BaselineInputs) Identity() BaselineObservationIdentity {
	if h == nil {
		return BaselineObservationIdentity{}
	}
	b, _ := json.Marshal(h.identity)
	var c BaselineObservationIdentity
	_ = json.Unmarshal(b, &c)
	return c
}
func (h *BaselineInputs) Object(d string) ([]byte, bool) {
	if h == nil {
		return nil, false
	}
	b, ok := h.objects[d]
	return append([]byte(nil), b...), ok
}
func (h *BaselineInputs) Snapshot() *snapshot.Local {
	if h == nil {
		return nil
	}
	b, _ := json.Marshal(h.local)
	var c snapshot.Local
	_ = json.Unmarshal(b, &c)
	restoreLocalFields(&c)
	return &c
}
func (h *BaselineInputs) Binding() BaselineBinding {
	if h == nil {
		return BaselineBinding{}
	}
	return BaselineBinding{h.ref, h.identity.RepoID, h.identity.SnapshotID, h.identity.SnapshotDigest, h.identity.Physical}
}
func baselinePhysical(a trees.SelectedReceipt, x trees.SourceViewReceipt) BaselinePhysicalBinding {
	return BaselinePhysicalBinding{a.TreeDigest, a.SelectedInventoryDigest, a.StageInventoryDigest, x.SourceDigest, x.StateObject, baselineSelection}
}
func baselineRoot(s *store.Store, binding, repo, root string) string {
	return hint.EvalRef(s.Key(), BaselineObservationVersion, "root", binding, repo, root)
}
func baselinePair(limits trees.Limits, receipts []DefinitionObject, objects map[string][]byte) (*trees.DecodedSelectedSourcePair, error) {
	if len(receipts) != 4 {
		return nil, ErrBaselineObservation
	}
	for _, o := range receipts {
		b, ok := objects[o.Digest]
		if !ok || int64(len(b)) != o.Bytes || library.Digest(b) != o.Digest {
			return nil, ErrBaselineObservation
		}
	}
	return trees.DecodeSelectedSourcePair(limits, trees.SelectedSourceWire{SelectedA: objects[receipts[0].Digest], SelectedB: objects[receipts[1].Digest], SourceA: objects[receipts[2].Digest], SourceB: objects[receipts[3].Digest]})
}

// materializeBaseline never examines the live repository or follows links. The
// caller owns the temporary directory and supplies the already validated closure.
func materializeBaseline(ctx context.Context, s *store.Store, repo string, pair *trees.DecodedSelectedSourcePair, objects map[string][]byte) (*scan.Result, snapshot.GitState, error) {
	a, _, x, _ := pair.Receipts()
	if e := s.EnsureStateDirContext(ctx, "cache"); e != nil {
		return nil, snapshot.GitState{}, e
	}
	dir, e := os.MkdirTemp(s.Path("cache"), "baseline-")
	if e != nil {
		return nil, snapshot.GitState{}, e
	}
	defer os.RemoveAll(dir)
	files := []string{}
	for _, f := range a.Selected {
		if e = ctx.Err(); e != nil {
			return nil, snapshot.GitState{}, e
		}
		if f.State != trees.SelectedRegularPresent {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return nil, snapshot.GitState{}, e
		}
		mode := os.FileMode(0600)
		if f.Mode == "100755" {
			mode = 0700
		}
		if e = os.WriteFile(p, objects[f.ObjectDigest], mode); e != nil {
			return nil, snapshot.GitState{}, e
		}
		files = append(files, f.Path)
	}
	if len(files) == 0 {
		return nil, snapshot.GitState{}, ErrBaselineObservation
	}
	// StateObject was checked by the sole pair validator; no ambient Git lookup.
	var state struct {
		Head   string
		Branch string
		Facts  []trees.SourceFact
	}
	if definitionDecode(objects[x.StateObject], &state) != nil {
		return nil, snapshot.GitState{}, ErrBaselineObservation
	}
	res, e := scan.Scan(scan.Options{Ctx: ctx, Root: dir, RepoID: repo, Key: s.Key(), Files: files})
	if e != nil {
		return nil, snapshot.GitState{}, e
	}
	hashes := map[string]string{}
	for _, ent := range res.Entities {
		if ent.Kind == "file" {
			hashes[ent.Path] = ent.Hash
		}
	}
	if len(hashes) != len(files) {
		return nil, snapshot.GitState{}, ErrBaselineObservation
	}
	for _, f := range a.Selected {
		if f.State == trees.SelectedRegularPresent && hashes[f.Path] != f.ObjectDigest[7:] {
			return nil, snapshot.GitState{}, ErrBaselineObservation
		}
	}
	return res, snapshot.GitState{Head: state.Head, State: "unknown"}, ctx.Err()
}
func LoadBaselineObservation(parent context.Context, s *store.Store, ref BaselineObservationRef) (*BaselineInputs, error) {
	if parent == nil || s == nil {
		return nil, ErrBaselineObservation
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return loadBaselineObservation(ctx, s, ref)
}
func loadBaselineObservation(ctx context.Context, s *store.Store, ref BaselineObservationRef) (*BaselineInputs, error) {
	if !batchID.MatchString(ref.RecordRef) || !digestPattern.MatchString(ref.RecordDigest) {
		return nil, ErrBaselineObservation
	}
	raw, e := s.ReadStateBounded(ctx, captureRecordMax, "state", library.BaselineObservationStateDir, ref.RecordRef+".json")
	if e != nil || library.Digest(raw) != ref.RecordDigest {
		return nil, ErrBaselineObservation
	}
	var v BaselineObservationIdentity
	if captureDecode(raw, &v) != nil {
		return nil, ErrBaselineObservation
	}
	binding, e := definitionStoreBinding(ctx, s)
	root, re := captureConfiguredRoot(s, s.Cfg, v.RepoID)
	lim, le := trees.ResolveStrictCaptureLimits(v.Limits)
	if e != nil || re != nil || le != nil || lim != v.Limits || v.Kind != BaselineObservationVersion || v.RecordRef != ref.RecordRef || v.Namespace != s.Cfg.ProjectNamespace || v.StoreBinding != binding || v.RootBinding != baselineRoot(s, binding, v.RepoID, root) || v.Writer != baselineSourceIdentity() || v.Scanner != scan.Version || v.Reader != trees.SourceViewVersion || v.State != "prepared_baseline_publication_unknown" || v.Authority != "unknown" || v.BeforePublishNS < 0 || v.BeforePublishNS >= int64(30*time.Second) {
		return nil, ErrBaselineObservation
	}
	for _, t := range []string{v.ObservedAt, v.DerivedAt} {
		if at, e := time.Parse(time.RFC3339Nano, t); e != nil || at.IsZero() {
			return nil, ErrBaselineObservation
		}
	}
	l := library.Open(s, 0)
	objects := map[string][]byte{}
	var used int64
	read := func(o DefinitionObject) error {
		if !digestPattern.MatchString(o.Digest) || o.Bytes < 1 {
			return ErrBaselineObservation
		}
		if cached, ok := objects[o.Digest]; ok {
			if int64(len(cached)) != o.Bytes {
				return ErrBaselineObservation
			}
			return nil
		}
		if o.Bytes > 128<<20-used {
			return ErrBaselineObservation
		}
		b, e := l.ResolveBoundedRead(ctx, "object", o.Digest, o.Bytes)
		if e != nil || int64(len(b)) != o.Bytes {
			return ErrBaselineObservation
		}
		used += int64(len(b))
		objects[o.Digest] = b
		return nil
	}
	for _, o := range v.Receipts {
		if o.Bytes > captureRecordMax || read(o) != nil {
			return nil, ErrBaselineObservation
		}
	}
	pair, e := baselinePair(v.Limits, v.Receipts, objects)
	if e != nil {
		return nil, e
	}
	closure := map[string][]byte{}
	for _, o := range pair.ObjectRefs() {
		b, e := l.ResolveBoundedRead(ctx, "object", o.Digest, min(o.MaxBytes, 128<<20-used))
		if e != nil {
			return nil, ErrBaselineObservation
		}
		used += int64(len(b))
		objects[o.Digest] = b
		closure[o.Digest] = b
	}
	if _, e = pair.ValidateObjects(closure); e != nil {
		return nil, e
	}
	a, _, x, _ := pair.Receipts()
	if v.RootIdentity != a.RootIdentity || v.Physical != baselinePhysical(a, x) {
		return nil, ErrBaselineObservation
	}
	if read(v.ConfigObject) != nil || v.ConfigObject.Bytes > captureSmallMax {
		return nil, ErrBaselineObservation
	}
	invObjects := map[string][]byte{}
	for d, b := range objects {
		invObjects[d] = b
	}
	inv, e := captureInventory(pair, v.Receipts, v.ConfigObject, invObjects)
	if e != nil || !reflect.DeepEqual(inv, v.Objects) {
		return nil, ErrBaselineObservation
	}
	var cfg store.Config
	if definitionDecode(objects[v.ConfigObject.Digest], &cfg) != nil || cfg.ProjectNamespace != v.Namespace {
		return nil, ErrBaselineObservation
	}
	cr, e := captureConfiguredRoot(s, cfg, v.RepoID)
	if e != nil || cr != root {
		return nil, ErrBaselineObservation
	}
	if v.SnapshotObject.Bytes > definitionMetadataMax || read(v.SnapshotObject) != nil {
		return nil, ErrBaselineObservation
	}
	var local snapshot.Local
	if definitionDecode(objects[v.SnapshotObject.Digest], &local) != nil {
		return nil, ErrBaselineObservation
	}
	restoreLocalFields(&local)
	var prev *snapshot.Local
	if v.ParentObject != nil {
		if v.ParentObject.Bytes > definitionMetadataMax || read(*v.ParentObject) != nil {
			return nil, ErrBaselineObservation
		}
		prev = &snapshot.Local{}
		if definitionDecode(objects[v.ParentObject.Digest], prev) != nil {
			return nil, ErrBaselineObservation
		}
		restoreLocalFields(prev)
	}
	res, g, e := materializeBaseline(ctx, s, v.RepoID, pair, objects)
	if e != nil {
		return nil, e
	}
	if e = baselineGraphMatches(&local, res, g, s); e != nil {
		return nil, e
	}
	// Validate the actual recorded predecessor as data, including pointer linkage.
	if prev != nil {
		d, e := snapshot.Digest(prev.Record, prev.Entities, prev.Relations)
		if e != nil || d != prev.Record.Digest || prev.Record.RepoID != v.RepoID {
			return nil, ErrBaselineObservation
		}
		if v.PreviousPointer.Bytes < 1 || read(v.PreviousPointer) != nil {
			return nil, ErrBaselineObservation
		}
		var ptr latestRef
		if definitionDecode(objects[v.PreviousPointer.Digest], &ptr) != nil || ptr.SnapshotID != prev.Record.SnapshotID {
			return nil, ErrBaselineObservation
		}
	} else if v.PreviousPointer.Bytes != 0 || v.PreviousPointer.Digest != library.Digest(nil) {
		return nil, ErrBaselineObservation
	}
	observed, _ := time.Parse(time.RFC3339Nano, v.ObservedAt)
	derived, _ := time.Parse(time.RFC3339Nano, v.DerivedAt)
	captured, e := time.Parse(time.RFC3339Nano, local.Record.CapturedAt)
	if e != nil || derived.Before(observed) || captured.After(derived) {
		return nil, ErrBaselineObservation
	}
	if prev == nil {
		if len(local.Record.ParentSnapshotIDs) != 0 {
			return nil, ErrBaselineObservation
		}
	} else if local.Record.SnapshotID == prev.Record.SnapshotID {
		if !equalJSON(local, *prev) {
			return nil, ErrBaselineObservation
		}
	} else if len(local.Record.ParentSnapshotIDs) != 1 || local.Record.ParentSnapshotIDs[0] != prev.Record.SnapshotID {
		return nil, ErrBaselineObservation
	}
	if local.Record.RepoID != v.RepoID || local.Record.SnapshotID != v.SnapshotID || local.Record.Digest != v.SnapshotDigest {
		return nil, ErrBaselineObservation
	}
	return &BaselineInputs{ref, v, objects, local, pair}, ctx.Err()
}

func captureVersion(v WorkDefinitionIdentity) string {
	if v.Kind == WorkDefinitionInputsV2 {
		return CaptureSourceV2
	}
	return CaptureSourceVersion
}

// Source v2 compares independently captured facts. Structural equality never
// substitutes for the physical path/presence/mode/size/content commitment.
func compareBaselineSource(ctx context.Context, s *store.Store, v WorkDefinitionIdentity, pair *trees.DecodedSelectedSourcePair, objects map[string][]byte) error {
	if v.Kind != WorkDefinitionInputsV2 {
		if v.Baseline != nil {
			return ErrBaselineObservation
		}
		return nil
	}
	if v.Baseline == nil {
		return ErrBaselineObservation
	}
	b, e := loadBaselineObservation(ctx, s, v.Baseline.Observation)
	if e != nil {
		return e
	}
	if b.Binding() != *v.Baseline {
		return ErrBaselineObservation
	}
	a, _, x, _ := pair.Receipts()
	ba, _, bx, _ := b.pair.Receipts()
	if b.identity.Physical != baselinePhysical(a, x) || a.RootIdentity != ba.RootIdentity || !reflect.DeepEqual(a.Selected, ba.Selected) || !reflect.DeepEqual(a.Stages, ba.Stages) || x.ConfigObject != bx.ConfigObject || !reflect.DeepEqual(x.Facts, bx.Facts) {
		return ErrBaselineObservation
	}
	res, g, e := materializeBaseline(ctx, s, b.identity.RepoID, pair, objects)
	if e != nil {
		return e
	}
	return baselineGraphMatches(&b.local, res, g, s)
}

// Rebuild the current graph through snapshot.Build rather than trusting a saved
// StateKey (Build's unchanged fast path returns prev). Only the actual recorded
// parent ID is needed by Build; no historical physical state is inferred here.
func baselineGraphMatches(local *snapshot.Local, res *scan.Result, g snapshot.GitState, s *store.Store) error {
	if len(local.Record.ParentSnapshotIDs) > 1 {
		return ErrBaselineObservation
	}
	var predecessor *snapshot.Local
	if len(local.Record.ParentSnapshotIDs) == 1 {
		id := local.Record.ParentSnapshotIDs[0]
		if !batchID.MatchString(id) || id == local.Record.SnapshotID {
			return ErrBaselineObservation
		}
		predecessor = &snapshot.Local{Record: snapshot.Record{SnapshotID: id}}
	}
	at, e := time.Parse(time.RFC3339Nano, local.Record.CapturedAt)
	if e != nil {
		return ErrBaselineObservation
	}
	fresh, _, e := snapshot.Build(predecessor, res, local.Record.RepoID, s.Key(), g, at)
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(local)
	if _, ok := sameRevision(raw, fresh); !ok {
		return ErrBaselineObservation
	}
	return nil
}
