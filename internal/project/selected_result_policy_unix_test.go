//go:build unix

package project

import (
	"archive/tar"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// TC-2: an ACTUAL supported relation (actual retained Native SourceRef + actual
// artifact-profile capture of the same owned root) refuses when the retained
// Native executable digest and the artifact executable digest differ.
func TestNativeArtifactRelationBindsRetainedExecutable(t *testing.T) {
	f, def := captureFixture(t)
	ctx := context.Background()
	a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, def), f.now)
	if e != nil {
		t.Fatal(e)
	}
	loaded, e := LoadCaptureSource(ctx, f.s, *a.Source())
	if e != nil {
		t.Fatal(e)
	}
	replay, e := nativeReplay(loaded)
	if e != nil {
		t.Fatal(e)
	}
	limits, e := trees.ResolveStrictCaptureLimits(trees.Limits{})
	if e != nil {
		t.Fatal(e)
	}
	lock, e := f.s.TryLockStateContext(ctx)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := captureSourcePairProfile(ctx, library.Open(f.s, 0), f.s.Root, limits, nil, replay, func(_ string, fn func() error) error { return fn() })
	if ue := lock.UnlockChecked(); e != nil || ue != nil {
		t.Fatal("artifact capture", e, ue)
	}
	inv := runner.PrelaunchInventory{Entries: []runner.PrelaunchEntry{{Path: "", Type: tar.TypeDir, Mode: 0755}}}
	for _, s := range pair.selected.Selected {
		if s.State == trees.SelectedRegularPresent {
			inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: s.Path, Type: tar.TypeReg, Mode: 0644, Size: s.Size, Digest: s.ObjectDigest})
		}
	}
	rel, e := nativeArtifactRelation(loaded, replay, pair.source, pair.objects, inv)
	if e != nil {
		t.Fatal("actual supported relation refused", e)
	}
	if rel.ExecutableDigest == "" || rel.ExecutableDigest != loaded.native.ExecutableDigest || rel.ExecutableDigest != pair.source.ExecutableDigest || rel.NativeInvocation == rel.ArtifactInvocation {
		t.Fatal("executable binding", rel.ExecutableDigest)
	}
	other := "sha256:" + strings.Repeat("a", 64)
	changedArtifact := pair.source
	changedArtifact.ExecutableDigest = other
	if _, e = nativeArtifactRelation(loaded, replay, changedArtifact, pair.objects, inv); !errors.Is(e, ErrSelectedResultRelation) {
		t.Fatal("artifact executable mismatch admitted", e)
	}
	changedNative := *loaded
	changedNative.native.ExecutableDigest = other
	if _, e = nativeArtifactRelation(&changedNative, replay, pair.source, pair.objects, inv); !errors.Is(e, ErrSelectedResultRelation) {
		t.Fatal("retained Native executable mismatch admitted", e)
	}
	if loaded.native.ExecutableDigest == other {
		t.Fatal("retained closure aliased")
	}
}
