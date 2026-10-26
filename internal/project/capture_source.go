package project

// CREATE-only local selected-source observation. No replay or durable authority.
import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/readgit"
	"github.com/neprel/git-hint/internal/store"
)

var ErrCaptureSource = errors.New("selected source capture refused")

//go:embed selected_result_inputs.go selected_result_policy.go capture_source.go capture_source_records.go capture_source_pair.go baseline_collect.go baseline_records.go prework_acceptance.go
var captureSources embed.FS

func captureSourceIdentity() string {
	return captureSourceIdentityFrom(captureSources, githint.CaptureSourceAssets)
}
func captureSourceIdentityFrom(local, dependencies fs.FS) string {
	values := []string{CaptureSourceVersion, runtime.Version(), runtime.GOOS, runtime.GOARCH, definitionSourceIdentity(), qualify.QualifiedInputSourceIdentity(), store.BoundedStateSourceIdentity(), preworkAcceptanceVersion}
	for _, source := range []fs.FS{local, dependencies} {
		names := []string{}
		e := fs.WalkDir(source, ".", func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !d.IsDir() {
				names = append(names, path)
			}
			return nil
		})
		if e != nil {
			return ""
		}
		sort.Strings(names)
		for _, name := range names {
			b, e := fs.ReadFile(source, name)
			if e != nil {
				return ""
			}
			values = append(values, name, library.Digest(b))
		}
	}
	d, e := hint.Digest(values)
	if e != nil {
		return ""
	}
	return d
}
func captureProblem(ctx context.Context, code string) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(ErrCaptureSource, ctx.Err())
	}
	return fmt.Errorf("%w: %s", ErrCaptureSource, code)
}

type CaptureSourceRequest struct {
	Label      string
	Definition WorkDefinitionRef
	Limits     trees.Limits
}
type CaptureSourceAttempt struct {
	source     *CaptureSourceRef
	acceptance *CaptureSourceAcceptance
}

func (a *CaptureSourceAttempt) Source() *CaptureSourceRef {
	if a == nil || a.source == nil {
		return nil
	}
	v := *a.source
	return &v
}
func (a *CaptureSourceAttempt) Acceptance() *CaptureSourceAcceptance {
	if a == nil || a.acceptance == nil || !time.Now().Before(a.acceptance.end) {
		return nil
	}
	v := *a.acceptance
	return &v
}

// CaptureSourceAcceptance is process-local and purpose-bound. It supplies no origin
// or observer authority, and expires at the ORIGINAL producer endpoint.
type CaptureSourceAcceptance struct {
	checkpoint                            preworkAcceptance
	source                                CaptureSourceRef
	definition                            WorkDefinitionRef
	policy                                CapturePolicyRef
	reservation                           CaptureReservationRef
	storeBinding, unit, namespace, writer string
	end                                   time.Time
}

func (CaptureSourceAcceptance) MarshalJSON() ([]byte, error) { return nil, ErrCaptureSource }
func (*CaptureSourceAcceptance) UnmarshalJSON([]byte) error  { return ErrCaptureSource }
func captureAcceptanceMatches(a *CaptureSourceAcceptance, ref CaptureSourceRef, v CaptureSourceIdentity) bool {
	return a != nil && matchPreworkAcceptance(a.checkpoint, preworkSelectedSource) && time.Now().Before(a.end) && a.source == ref && a.definition == v.DefinitionRef && a.policy == v.PolicyRef && a.reservation == v.ReservationRef && a.storeBinding == v.Definition.StoreBinding && a.unit == v.Definition.UnitRef && a.namespace == v.Definition.Namespace && a.writer == captureSourceIdentity()
}

// Private negative-test observation only. It cannot return capture data or success.
type captureSourceObserverKey struct{}
type captureSourceObserver interface{ captureSourceBoundary(string) }

func captureObserve(ctx context.Context, phase string) {
	if v, ok := ctx.Value(captureSourceObserverKey{}).(captureSourceObserver); ok {
		v.captureSourceBoundary(phase)
	}
}

func captureConfiguredRoot(s *store.Store, cfg store.Config, repoID string) (string, error) {
	root := ""
	for _, r := range cfg.Repos {
		if s.RepoID(r) == repoID {
			if root != "" {
				return "", ErrCaptureSource
			}
			root = s.RepoRoot(r)
		}
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrCaptureSource
	}
	return root, nil
}
func captureRootBinding(s *store.Store, v WorkDefinitionIdentity, root string) string {
	return hint.EvalRef(s.Key(), "project-capture-root-v1", v.UnitRef, v.PlanBinding.RepoID, root)
}

// One stored-byte/current-pointer guard. Later coordinators can reuse this exact
// local guard; it does NOT prove physical selected-tree/structural snapshot equality.
func captureDefinitionCurrent(r *definitionStateRead, v WorkDefinitionIdentity, config DefinitionObject) error {
	cfg, e := r.read(captureSmallMax, "state", "project.json")
	if e != nil || library.Digest(cfg) != config.Digest || int64(len(cfg)) != config.Bytes {
		return ErrCaptureSource
	}
	var c store.Config
	if json.Unmarshal(cfg, &c) != nil || !reflect.DeepEqual(c, r.s.Cfg) {
		return ErrCaptureSource
	}
	b, e := r.read(definitionMetadataMax, "state", "exports", "state.json")
	if e != nil || library.Digest(b) != v.ExportStateObject {
		return ErrCaptureSource
	}
	for _, in := range v.SnapshotInputs {
		b, e := r.read(definitionMetadataMax, "state", "snapshots", in.RepoID, "latest.json")
		if e != nil || library.Digest(b) != in.LatestRefObject {
			return ErrCaptureSource
		}
		var latest latestRef
		if definitionDecode(b, &latest) != nil || !batchID.MatchString(latest.SnapshotID) {
			return ErrCaptureSource
		}
		b, e = r.read(definitionMetadataMax, "state", "snapshots", in.RepoID, latest.SnapshotID+".json")
		if e != nil || library.Digest(b) != in.SnapshotObject {
			return ErrCaptureSource
		}
	}
	return nil
}

var capturePhases = []string{"definition_initial", "history_initial", "policy_commit", "reservation_commit", "view_open", "capture_a", "capture_b", "view_close", "objects_and_receipts", "definition_final", "history_final"}

func captureBracketsValid(brackets []CaptureSourceBracket, end int64) bool {
	if len(brackets) != len(capturePhases) || end < 0 || end >= int64(30*time.Second) {
		return false
	}
	last := int64(0)
	for i, b := range brackets {
		if b.Phase != capturePhases[i] || b.BeginNS < last || b.EndNS < b.BeginNS || b.EndNS > end {
			return false
		}
		last = b.EndNS
	}
	return true
}

// CaptureWorkDefinitionSource allocates one policy/reservation/audit, never resumes
// received bytes. All work, reload, release and final checks share the entry+30s bound.
func CaptureWorkDefinitionSource(parent context.Context, s *store.Store, req CaptureSourceRequest, now time.Time) (attempt *CaptureSourceAttempt, err error) {
	anchor := time.Now()
	end := anchor.Add(30 * time.Second)
	if parent == nil || s == nil || now.IsZero() || !ValidTaskID(req.Label) {
		return nil, ErrCaptureSource
	}
	if d, ok := parent.Deadline(); ok && d.Before(end) {
		end = d
	}
	ctx, cancel := context.WithDeadline(parent, end)
	defer cancel()
	limits, e := trees.ResolveStrictCaptureLimits(req.Limits)
	if e != nil || req.Limits.MaxFileBytes > limits.MaxFileBytes || req.Limits.MaxTotalBytes > limits.MaxTotalBytes || req.Limits.MaxFiles > limits.MaxFiles {
		return nil, captureProblem(ctx, "limits")
	}
	invocation, e := readgit.FreezeSourceInvocation(ctx, os.Environ())
	if e != nil {
		return nil, captureProblem(ctx, "invocation")
	}
	ctx = readgit.WithSourceInvocation(ctx, invocation)
	lock, e := s.TryLockStateContext(ctx)
	if e != nil {
		return nil, errors.Join(ErrCaptureSource, e)
	}
	defer func() {
		if lock != nil {
			if e := lock.UnlockChecked(); e != nil {
				attempt = nil
				err = errors.Join(err, ErrCaptureSource, e)
			}
		}
	}()
	brackets := []CaptureSourceBracket{}
	phase := func(name string, fn func() error) error {
		captureObserve(ctx, name+"_begin")
		begin := time.Since(anchor).Nanoseconds()
		e := fn()
		finish := time.Since(anchor).Nanoseconds()
		if e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		brackets = append(brackets, CaptureSourceBracket{name, begin, finish})
		captureObserve(ctx, name+"_end")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	r := &definitionStateRead{ctx: ctx, s: s}
	var v WorkDefinitionIdentity
	var root string
	var config DefinitionObject
	var configBytes []byte
	if e = phase("definition_initial", func() error {
		def, _, e := loadWorkDefinitionResolved(ctx, s, req.Definition)
		if e != nil {
			return e
		}
		v = def.Identity()
		if hint.EvalRef(s.Key(), "work-unit", req.Label) != v.UnitRef {
			return ErrCaptureSource
		}
		configBytes, e = r.read(captureSmallMax, "state", "project.json")
		if e != nil {
			return e
		}
		config = DefinitionObject{library.Digest(configBytes), int64(len(configBytes))}
		if e = captureDefinitionCurrent(r, v, config); e != nil {
			return e
		}
		root, e = captureConfiguredRoot(s, s.Cfg, v.PlanBinding.RepoID)
		if e != nil {
			return e
		}
		canonical, e := filepath.EvalSymlinks(root)
		if e != nil || canonical != root {
			return ErrCaptureSource
		}
		return captureAbsentOperation(r, req.Definition)
	}); e != nil {
		return nil, captureProblem(ctx, "definition_initial")
	}
	var before, after []string
	if e = phase("history_initial", func() error { var e error; before, e = definitionHistory(r, req.Label, v.UnitRef); return e }); e != nil {
		return nil, captureProblem(ctx, "history_initial")
	}
	p := capturePolicy{Kind: capturePolicyVersion, DefinitionRef: req.Definition, Definition: v, RootBinding: captureRootBinding(s, v, root), ConfigObject: config, Limits: limits, Selector: "git-selected-capture-source-v1", Scope: "git_selected_artifact_v1", Reader: trees.SourceViewVersion, Repeat: "two_pass_exact_local_observation", Invocation: invocation.Identity(), Writer: captureSourceIdentity(), Basis: "local_selected_source_policy_only", Authority: "unknown", CreatedAt: now.UTC().Format(time.RFC3339Nano)}
	if p.Writer == "" {
		return nil, ErrCaptureSource
	}
	p.RecordRef = capturePolicyID(s, p)
	var pr CapturePolicyRef
	if e = phase("policy_commit", func() error {
		// One existing retention barrier covers even empty/orphan directories BEFORE objects.
		for _, dir := range []string{library.CapturePolicyStateDir, library.CaptureReservationStateDir, library.CaptureSourceStateDir} {
			if e := s.EnsureStateDirContext(ctx, "state", dir); e != nil {
				return e
			}
		}
		var e error
		pr, e = captureWrite(ctx, s, library.CapturePolicyStateDir, captureSmallMax, p, p.RecordRef)
		return e
	}); e != nil {
		return nil, captureProblem(ctx, "policy_commit")
	}
	auditID := captureOperationID(s, captureVersion(v), req.Definition, pr)
	reservation := captureReservation{Kind: captureReservationVersion, RecordRef: captureOperationID(s, captureReservationVersion, req.Definition, pr), PlannedAuditRef: auditID, DefinitionRef: req.Definition, PolicyRef: pr, UnitRef: v.UnitRef, StoreBinding: v.StoreBinding, Namespace: v.Namespace, RootBinding: p.RootBinding, Writer: p.Writer, ObservedAbsent: before, State: "reserved", Authority: "unknown", CreatedAt: p.CreatedAt}
	var rr CaptureReservationRef
	if e = phase("reservation_commit", func() error {
		var e error
		rr, e = captureWrite(ctx, s, library.CaptureReservationStateDir, captureSmallMax, reservation, reservation.RecordRef)
		return e
	}); e != nil {
		return nil, captureProblem(ctx, "reservation_commit")
	}
	l := library.Open(s, 0)
	paired, e := captureSourcePair(ctx, l, root, limits, invocation, phase)
	if e != nil {
		return nil, e
	}
	objects, receipts := paired.objects, paired.receipts
	d, e := l.PutObjectContext(ctx, configBytes)
	if e != nil || d != config.Digest {
		return nil, ErrCaptureSource
	}
	objects[d] = configBytes
	wire := trees.SelectedSourceWire{SelectedA: objects[receipts[0].Digest], SelectedB: objects[receipts[1].Digest], SourceA: objects[receipts[2].Digest], SourceB: objects[receipts[3].Digest]}
	pair, e := trees.DecodeSelectedSourcePair(limits, wire)
	if e != nil {
		return nil, e
	}
	inventory, e := captureInventory(pair, receipts, config, objects)
	if e != nil {
		return nil, e
	}
	if e = compareBaselineSource(ctx, s, v, pair, objects); e != nil {
		return nil, e
	}
	if e = phase("definition_final", func() error {
		def, _, e := loadWorkDefinitionResolved(ctx, s, req.Definition)
		if e != nil || !reflect.DeepEqual(def.Identity(), v) {
			return ErrCaptureSource
		}
		return captureDefinitionCurrent(r, v, config)
	}); e != nil {
		return nil, captureProblem(ctx, "definition_final")
	}
	if e = phase("history_final", func() error {
		var e error
		after, e = definitionHistory(r, req.Label, v.UnitRef)
		if e != nil || !reflect.DeepEqual(before, after) {
			return ErrCaptureSource
		}
		return nil
	}); e != nil {
		return nil, captureProblem(ctx, "history_final")
	}
	audit := CaptureSourceIdentity{Kind: captureVersion(v), RecordRef: auditID, DefinitionRef: req.Definition, PolicyRef: pr, ReservationRef: rr, Definition: v, RootBinding: p.RootBinding, RootIdentity: paired.selected.RootIdentity, ReceiptObjects: receipts, Objects: inventory, ObservedAbsentBefore: before, ObservedAbsentAfter: after, SharedStateReadBytesAtCommitPrepare: r.used, OuterInvocation: invocation.Identity(), OuterStats: invocation.Stats(), ClockScope: auditID, Brackets: brackets, CommitPrepareNS: time.Since(anchor).Nanoseconds(), State: "observed_selected_source_consistent", Basis: "repeated_local_selected_artifact_observation", Authority: "unknown", Writer: p.Writer, CreatedAt: p.CreatedAt}
	captureObserve(ctx, "audit_commit_begin")
	ref, e := captureWrite(ctx, s, library.CaptureSourceStateDir, captureRecordMax, audit, auditID)
	if e != nil {
		return nil, captureProblem(ctx, "audit_commit")
	}
	captureObserve(ctx, "audit_commit_end")
	loaded, e := loadCaptureSource(ctx, s, ref)
	if e != nil || !reflect.DeepEqual(loaded.Identity(), audit) {
		return nil, captureProblem(ctx, "final_reload")
	}
	captureObserve(ctx, "final_reload_end")
	// These final checks are NOT represented as completed audit spans/counters.
	if e = captureDefinitionCurrent(r, v, config); e != nil {
		return nil, captureProblem(ctx, "final_definition")
	}
	finalAbsent, e := definitionHistory(r, req.Label, v.UnitRef)
	if e != nil || !reflect.DeepEqual(finalAbsent, before) {
		return nil, captureProblem(ctx, "final_history")
	}
	captureObserve(ctx, "unlock_begin")
	if e = lock.UnlockChecked(); e != nil {
		lock = nil
		return nil, captureProblem(ctx, "unlock_failed")
	}
	lock = nil
	captureObserve(ctx, "unlock_end")
	if ctx.Err() != nil || !time.Now().Before(end) {
		return nil, captureProblem(ctx, "late_finalization")
	}
	witness := &CaptureSourceAcceptance{checkpoint: mintPreworkAcceptance(preworkSelectedSource), source: ref, definition: req.Definition, policy: pr, reservation: rr, storeBinding: v.StoreBinding, unit: v.UnitRef, namespace: v.Namespace, writer: p.Writer, end: end}
	return &CaptureSourceAttempt{source: &ref, acceptance: witness}, nil
}

// Declared local family list for structural source-identity guards.
func captureSourceFamily(name string) bool {
	return (strings.HasPrefix(name, "selected_result_") || strings.HasPrefix(name, "baseline_") || strings.HasPrefix(name, "capture_source") || strings.HasPrefix(name, "prework_acceptance")) && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}
