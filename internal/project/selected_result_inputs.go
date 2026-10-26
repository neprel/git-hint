package project

// LOCAL physical/structural correspondence. The full candidate TAR, selected
// tree object and canonical structural snapshot occupy three distinct domains.
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/readgit"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

var ErrSelectedResult = errors.New("selected_result_inputs_unavailable")

type SelectedResultRequest struct {
	RepoID     string
	Definition WorkDefinitionRef
	Baseline   BaselineObservationRef
	Source     CaptureSourceRef
	Result     *runner.TaskResultArtifact
	Limits     trees.Limits
}
type SelectedResultIdentity struct {
	RepoID, Namespace, State, Authority, Writer string
	Definition                                  WorkDefinitionRef
	Baseline                                    BaselineBinding
	Source                                      CaptureSourceRef
	Result                                      runner.TaskResultIdentity
	Selected                                    trees.SelectedReceipt
	SourceView                                  trees.SourceViewReceipt
	Receipts                                    []DefinitionObject
	Compared                                    []runner.PrelaunchEntry
	Absent                                      []string
	Relation                                    NativeArtifactRelation
}
type SelectedResultInputs struct {
	identity  SelectedResultIdentity
	objects   map[string][]byte
	inventory runner.PrelaunchInventory
	unmapped  runner.PrelaunchInventory
	local     *snapshot.Local
}

func (*SelectedResultInputs) MarshalJSON() ([]byte, error) { return nil, ErrSelectedResult }
func (v *SelectedResultInputs) Identity() SelectedResultIdentity {
	if v == nil {
		return SelectedResultIdentity{State: "unknown", Authority: "unknown"}
	}
	b, _ := json.Marshal(v.identity)
	var out SelectedResultIdentity
	_ = json.Unmarshal(b, &out)
	return out
}
func (v *SelectedResultInputs) Object(d string) ([]byte, bool) {
	if v == nil {
		return nil, false
	}
	b, ok := v.objects[d]
	return append([]byte(nil), b...), ok
}
func (v *SelectedResultInputs) Inventory() runner.PrelaunchInventory {
	if v == nil {
		return runner.PrelaunchInventory{}
	}
	return v.inventory.Copy()
}
func (v *SelectedResultInputs) UnmappedInventory() runner.PrelaunchInventory {
	if v == nil {
		return runner.PrelaunchInventory{}
	}
	return v.unmapped.Copy()
}
func (v *SelectedResultInputs) Snapshot() *snapshot.Local {
	if v == nil || v.local == nil {
		return nil
	}
	b, _ := json.Marshal(v.local)
	var out snapshot.Local
	_ = json.Unmarshal(b, &out)
	restoreLocalFields(&out)
	return &out
}

// ResolveSelectedResultInputs writes only retained library objects behind the
// existing retention barrier. It publishes no current snapshot or trust record.
func ResolveSelectedResultInputs(parent context.Context, s *store.Store, req SelectedResultRequest) (out *SelectedResultInputs, err error) {
	if parent == nil || s == nil || parent.Err() != nil || !req.Result.Ready() {
		return nil, ErrSelectedResult
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	limits, e := trees.ResolveStrictCaptureLimits(req.Limits)
	if e != nil || req.Limits.MaxFileBytes > limits.MaxFileBytes || req.Limits.MaxTotalBytes > limits.MaxTotalBytes || req.Limits.MaxFiles > limits.MaxFiles {
		return nil, ErrSelectedResult
	}
	if _, e = captureConfiguredRoot(s, s.Cfg, req.RepoID); e != nil {
		return nil, ErrSelectedResult
	}
	invocation, e := readgit.FreezeSourceInvocation(ctx, os.Environ())
	if e != nil {
		return nil, e
	}
	ctx = readgit.WithSourceInvocation(ctx, invocation)
	lock, e := s.TryLockStateContext(ctx)
	if e != nil {
		return nil, e
	}
	defer func() {
		if lock != nil {
			if ce := lock.UnlockChecked(); ce != nil {
				out = nil
				err = ErrSelectedResult
			}
		}
		if !referenceCurrent(ctx) {
			out = nil
			err = ErrSelectedResult
		}
	}()
	cfgRaw, e := s.ReadStateBounded(ctx, captureSmallMax, "state", "project.json")
	var cfg store.Config
	if e != nil || definitionDecode(cfgRaw, &cfg) != nil || !reflect.DeepEqual(cfg, s.Cfg) {
		return nil, ErrSelectedResult
	}
	if _, e = captureConfiguredRoot(s, cfg, req.RepoID); e != nil {
		return nil, ErrSelectedResult
	}
	def, e := LoadWorkDefinitionInputs(ctx, s, req.Definition)
	if e != nil {
		return nil, e
	}
	base, e := LoadBaselineObservation(ctx, s, req.Baseline)
	if e != nil {
		return nil, e
	}
	source, e := LoadCaptureSource(ctx, s, req.Source)
	if e != nil {
		return nil, e
	}
	d, b, src := def.Identity(), base.Binding(), source.Identity()
	if d.Kind != WorkDefinitionInputsV2 || d.Baseline == nil || *d.Baseline != b || b.RepoID != req.RepoID || src.Kind != CaptureSourceV2 || src.DefinitionRef != req.Definition || !reflect.DeepEqual(src.Definition, d) || d.Namespace != s.Cfg.ProjectNamespace {
		return nil, ErrSelectedResult
	}
	rid := req.Result.Identity()
	qi := d.QualifiedInputIdentity
	if rid.Inputs.SpecDigest != qi.SpecDigest || rid.Inputs.InputsDigest != qi.InputsDigest || rid.Inputs.DeclaredImage != qi.Image || rid.Inputs.SourceInputs["base_archive"] != qi.NegativeControlArchive || rid.Inputs.SourceInputs["verifier"] != qi.VerifierArchive || rid.Inputs.SourceInputs["reference"] != qi.ReferencePatchArchive || rid.Inputs.SourceInputs["setup"] != qi.SetupDigest || qi.Workdir != "/app" {
		return nil, ErrSelectedResult
	}
	// The artifact profile replays the loaded closure's retained global exclude.
	replay, e := nativeReplay(source)
	if e != nil {
		return nil, e
	}
	if e = s.EnsureStateDirContext(ctx, "state", library.CaptureSourceStateDir); e != nil {
		return nil, e
	}
	dir, e := os.MkdirTemp("", "hint-selected-result-")
	if e != nil {
		return nil, e
	}
	defer func() {
		if ce := os.RemoveAll(dir); ce != nil || !referenceCurrent(ctx) {
			out = nil
			err = ErrSelectedResult
		}
	}()
	canonical, ce := filepath.EvalSymlinks(dir)
	if ce != nil {
		return nil, ErrSelectedResult
	}
	dir = canonical
	inv, e := req.Result.Materialize(ctx, dir)
	if e != nil {
		return nil, e
	}
	phase := func(_ string, fn func() error) error {
		if !referenceCurrent(ctx) {
			return ErrSelectedResult
		}
		e := fn()
		if e != nil {
			return e
		}
		return ctx.Err()
	}
	l := library.Open(s, 0)
	pair, e := captureSourcePairProfile(ctx, l, dir, limits, invocation, replay, phase)
	if e != nil {
		return nil, e
	}
	compared, absent, unmapped, e := runner.MatchSelectedInventory(ctx, pair.selected, inv, false)
	if e != nil {
		return nil, e
	}
	relation, e := nativeArtifactRelation(source, replay, pair.source, pair.objects, inv)
	if e != nil {
		return nil, e
	}
	decoded, e := baselinePair(limits, pair.receipts, pair.objects)
	if e != nil {
		return nil, e
	}
	graph, g, e := materializeBaseline(ctx, s, req.RepoID, decoded, pair.objects)
	if e != nil {
		return nil, e
	}
	local, _, e := snapshot.Build(nil, graph, req.RepoID, s.Key(), g, time.Now())
	if e != nil {
		return nil, e
	}
	if e = lock.UnlockChecked(); e != nil {
		return nil, e
	}
	lock = nil
	if !referenceCurrent(ctx) {
		return nil, ErrSelectedResult
	}
	return &SelectedResultInputs{SelectedResultIdentity{req.RepoID, s.Cfg.ProjectNamespace, "terminal_selected_correspondence_observed", "unknown", captureSourceIdentity(), req.Definition, b, req.Source, rid, pair.selected, pair.source, append([]DefinitionObject(nil), pair.receipts...), compared, absent, relation}, pair.objects, inv.Copy(), unmapped.Copy(), local}, nil
}
