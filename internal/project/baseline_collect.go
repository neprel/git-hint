package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/readgit"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

type BaselineCollectRequest struct {
	RepoID string
	Limits trees.Limits
}
type BaselineCollection struct {
	ref        BaselineObservationRef
	inputs     *BaselineInputs
	acceptance *BaselineAcceptance
}

func (c *BaselineCollection) Ref() BaselineObservationRef {
	if c == nil {
		return BaselineObservationRef{}
	}
	return c.ref
}
func (c *BaselineCollection) Identity() BaselineObservationIdentity {
	if c == nil {
		return BaselineObservationIdentity{}
	}
	return c.inputs.Identity()
}
func (c *BaselineCollection) Snapshot() *snapshot.Local {
	if c == nil {
		return nil
	}
	return c.inputs.Snapshot()
}
func (c *BaselineCollection) Acceptance() *BaselineAcceptance {
	if c == nil || c.acceptance == nil || !time.Now().Before(c.acceptance.end) {
		return nil
	}
	v := *c.acceptance
	return &v
}

// BaselineAcceptance is an on-time process checkpoint, not source authority.
// Restart or factual reload cannot recover it. No exported generic mint exists.
type BaselineAcceptance struct {
	checkpoint    preworkAcceptance
	ref           BaselineObservationRef
	store, writer string
	end           time.Time
}

func (BaselineAcceptance) MarshalJSON() ([]byte, error) { return nil, ErrBaselineObservation }
func (*BaselineAcceptance) UnmarshalJSON([]byte) error  { return ErrBaselineObservation }
func baselineAcceptanceMatches(a *BaselineAcceptance, ref BaselineObservationRef, v BaselineObservationIdentity) bool {
	raw, e := hint.Canonical(v)
	return e == nil && v.RecordRef == ref.RecordRef && library.Digest(raw) == ref.RecordDigest && a != nil && matchPreworkAcceptance(a.checkpoint, preworkBaseline) && a.ref == ref && a.store == v.StoreBinding && a.writer == v.Writer && a.writer == baselineSourceIdentity() && time.Now().Before(a.end)
}

type baselineObserveKey struct{}

func baselineObserve(ctx context.Context, phase string) {
	if fn, ok := ctx.Value(baselineObserveKey{}).(func(string)); ok {
		fn(phase)
	}
}
func baselinePointer(ctx context.Context, s *store.Store, repo string) ([]byte, error) {
	b, e := s.ReadStateBounded(ctx, 1024, "state", "snapshots", repo, "latest.json")
	if errors.Is(e, fs.ErrNotExist) {
		return nil, nil
	}
	return b, e
}

// CollectVerifiedBaseline performs a NEW actual current collection. It does not
// recertify the physical state of any older snapshot. All work shares one entry
// endpoint; local filesystem calls remain cooperative, with late success refused.
func CollectVerifiedBaseline(parent context.Context, s *store.Store, req BaselineCollectRequest) (result *BaselineCollection, err error) {
	anchor := time.Now()
	end := anchor.Add(30 * time.Second)
	if parent == nil || s == nil || !batchID.MatchString(req.RepoID) {
		return nil, ErrBaselineObservation
	}
	if d, ok := parent.Deadline(); ok && d.Before(end) {
		end = d
	}
	ctx, cancel := context.WithDeadline(parent, end)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	limits, e := trees.ResolveStrictCaptureLimits(req.Limits)
	if e != nil || req.Limits.MaxFileBytes > limits.MaxFileBytes || req.Limits.MaxTotalBytes > limits.MaxTotalBytes || req.Limits.MaxFiles > limits.MaxFiles {
		return nil, ErrBaselineObservation
	}
	root, e := captureConfiguredRoot(s, s.Cfg, req.RepoID)
	if e != nil {
		return nil, ErrBaselineObservation
	}
	canonical, e := filepath.EvalSymlinks(root)
	if e != nil || canonical != root {
		return nil, ErrBaselineObservation
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
			if e := lock.UnlockChecked(); e != nil {
				result = nil
				err = errors.Join(err, ErrBaselineObservation, e)
			}
		}
	}()
	// Reserve the existing common retention barrier BEFORE any library object.
	if e = s.EnsureStateDirContext(ctx, "state", library.BaselineObservationStateDir); e != nil {
		return nil, e
	}
	writer := baselineSourceIdentity()
	l := library.Open(s, 0)
	binding, e := definitionStoreBinding(ctx, s)
	if e != nil {
		return nil, e
	}
	cfg, e := s.ReadStateBounded(ctx, captureSmallMax, "state", "project.json")
	if e != nil {
		return nil, e
	}
	var cfgValue store.Config
	if definitionDecode(cfg, &cfgValue) != nil || !reflect.DeepEqual(cfgValue, s.Cfg) {
		return nil, ErrBaselineObservation
	}
	previous, e := baselinePointer(ctx, s, req.RepoID)
	if e != nil {
		return nil, e
	}
	var prev *snapshot.Local
	var parentObject *DefinitionObject
	if len(previous) > 0 {
		var p latestRef
		if definitionDecode(previous, &p) != nil || !batchID.MatchString(p.SnapshotID) {
			return nil, ErrBaselineObservation
		}
		raw, e := s.ReadStateBounded(ctx, definitionMetadataMax, "state", "snapshots", req.RepoID, p.SnapshotID+".json")
		if e != nil {
			return nil, e
		}
		prev = &snapshot.Local{}
		if definitionDecode(raw, prev) != nil || prev.Record.RepoID != req.RepoID || prev.Record.SnapshotID != p.SnapshotID {
			return nil, ErrBaselineObservation
		}
		d, e := snapshot.Digest(prev.Record, prev.Entities, prev.Relations)
		if e != nil || d != prev.Record.Digest {
			return nil, ErrBaselineObservation
		}
		restoreLocalFields(prev)
		dg, e := l.PutObjectContext(ctx, raw)
		if e != nil {
			return nil, e
		}
		parentObject = &DefinitionObject{dg, int64(len(raw))}
		if _, e = l.PutObjectContext(ctx, previous); e != nil {
			return nil, e
		}
	}
	phase := func(name string, fn func() error) error {
		baselineObserve(ctx, name+"_begin")
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := fn(); e != nil {
			return e
		}
		baselineObserve(ctx, name+"_end")
		return ctx.Err()
	}
	pair, e := captureSourcePair(ctx, l, root, limits, invocation, phase)
	if e != nil {
		return nil, e
	}
	configDigest, e := l.PutObjectContext(ctx, cfg)
	if e != nil {
		return nil, e
	}
	config := DefinitionObject{configDigest, int64(len(cfg))}
	pair.objects[configDigest] = cfg
	decoded, e := baselinePair(limits, pair.receipts, pair.objects)
	if e != nil {
		return nil, e
	}
	inv, e := captureInventory(decoded, pair.receipts, config, pair.objects)
	if e != nil {
		return nil, e
	}
	res, g, e := materializeBaseline(ctx, s, req.RepoID, decoded, pair.objects)
	if e != nil {
		return nil, e
	}
	derived := time.Now()
	cur, _, e := snapshot.Build(prev, res, req.RepoID, s.Key(), g, derived)
	if e != nil {
		return nil, e
	}
	if e = baselineGraphMatches(cur, res, g, s); e != nil {
		return nil, e
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	id := hint.EvalRef(s.Key(), BaselineObservationVersion, binding, req.RepoID, hex.EncodeToString(nonce))
	var ref BaselineObservationRef
	// Shared immutable writer can adopt an earlier identical revision; bind exactly
	// its returned record BEFORE the latest pointer is moved, never overwrite it.
	cur, e = saveSnapshotContext(ctx, s, cur, func(actual *snapshot.Local) error {
		raw, e := json.Marshal(actual)
		if e != nil || len(raw) > int(definitionMetadataMax) {
			return ErrBaselineObservation
		}
		dg, e := l.PutObjectContext(ctx, raw)
		if e != nil {
			return e
		}
		identity := BaselineObservationIdentity{Kind: BaselineObservationVersion, RecordRef: id, Namespace: s.Cfg.ProjectNamespace, StoreBinding: binding, RepoID: req.RepoID, RootBinding: baselineRoot(s, binding, req.RepoID, root), RootIdentity: pair.selected.RootIdentity, ConfigObject: config, Limits: limits, Receipts: pair.receipts, Objects: inv, Physical: baselinePhysical(pair.selected, pair.source), SnapshotID: actual.Record.SnapshotID, SnapshotDigest: actual.Record.Digest, SnapshotObject: DefinitionObject{dg, int64(len(raw))}, ParentObject: parentObject, PreviousPointer: DefinitionObject{library.Digest(previous), int64(len(previous))}, ObservedAt: anchor.UTC().Format(time.RFC3339Nano), DerivedAt: derived.UTC().Format(time.RFC3339Nano), BeforePublishNS: time.Since(anchor).Nanoseconds(), Writer: writer, Scanner: scan.Version, Reader: trees.SourceViewVersion, State: "prepared_baseline_publication_unknown", Authority: "unknown"}
		baselineObserve(ctx, "observation_commit_begin")
		r, e := captureWrite(ctx, s, library.BaselineObservationStateDir, captureRecordMax, identity, id)
		if e != nil {
			return e
		}
		ref = BaselineObservationRef(r)
		baselineObserve(ctx, "observation_commit_end")
		latest, e := baselinePointer(ctx, s, req.RepoID)
		if e != nil || !bytes.Equal(latest, previous) {
			return ErrSnapshotConflict
		}
		cfgNow, e := s.ReadStateBounded(ctx, captureSmallMax, "state", "project.json")
		if e != nil || !bytes.Equal(cfgNow, cfg) {
			return ErrBaselineObservation
		}
		baselineObserve(ctx, "pointer_publish_begin")
		return ctx.Err()
	})
	if e != nil {
		return nil, e
	}
	baselineObserve(ctx, "pointer_publish_end")
	loaded, e := loadBaselineObservation(ctx, s, ref)
	if e != nil {
		return nil, e
	}
	latest, e := baselinePointer(ctx, s, req.RepoID)
	if e != nil {
		return nil, e
	}
	var ptr latestRef
	if definitionDecode(latest, &ptr) != nil || ptr.SnapshotID != cur.Record.SnapshotID {
		return nil, ErrBaselineObservation
	}
	cfgFinal, e := s.ReadStateBounded(ctx, captureSmallMax, "state", "project.json")
	if e != nil || !bytes.Equal(cfgFinal, cfg) {
		return nil, ErrBaselineObservation
	}
	baselineObserve(ctx, "unlock_begin")
	if e = lock.UnlockChecked(); e != nil {
		lock = nil
		return nil, e
	}
	lock = nil
	baselineObserve(ctx, "unlock_end")
	if ctx.Err() != nil || !time.Now().Before(end) {
		return nil, ErrBaselineObservation
	}
	acceptance := &BaselineAcceptance{mintPreworkAcceptance(preworkBaseline), ref, binding, writer, end}
	return &BaselineCollection{ref, loaded, acceptance}, nil
}
