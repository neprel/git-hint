package pipeline

import (
	"context"
	"errors"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/store"
)

// ObservedPrelaunch owns one actual prelaunch graph. It conveys no source authority
// or baseline mapping. The legacy graph population includes captured Git metadata.
type ObservedPrelaunch struct {
	result    *observedStartResult
	inventory runner.PrelaunchInventory
}

func (v *ObservedPrelaunch) Graph() *scan.Result {
	if v == nil || v.result == nil {
		return nil
	}
	return v.result.graph.Copy()
}
func (v *ObservedPrelaunch) Inventory() runner.PrelaunchInventory {
	if v == nil || v.result == nil {
		return runner.PrelaunchInventory{}
	}
	return v.inventory.Copy()
}
func (v *ObservedPrelaunch) Capture() runner.PrelaunchTreeCapture {
	if v == nil || v.result == nil {
		return runner.PrelaunchTreeCapture{}
	}
	return v.result.capture
}
func (*ObservedPrelaunch) MarshalJSON() ([]byte, error) {
	return nil, errors.New("prelaunch view is local only")
}
func prepareObservedPrelaunch(ctx context.Context, s *store.Store, rec map[string]any, a *runner.PrelaunchArtifact) (*ObservedPrelaunch, error) {
	var inv runner.PrelaunchInventory
	r, e := buildObservedStart(ctx, s, rec, a.Metadata().Capture, func(ctx context.Context, dst string) ([]string, error) {
		var err error
		inv, err = a.Materialize(ctx, dst)
		return inv.Files(), err
	})
	if e != nil {
		return nil, e
	}
	return &ObservedPrelaunch{result: r, inventory: inv}, nil
}
