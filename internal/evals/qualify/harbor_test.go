package qualify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/runner"
)

// TestHarborQualification qualifies the synthetic Go bug fix through the real Harbor CLI
// with no model (nop, oracle, probe). Set HINT_HARBOR to run it; it needs Docker.
// HINT_HARBOR_JOBS keeps the work directory.
func TestHarborQualification(t *testing.T) {
	if os.Getenv("HINT_HARBOR") == "" {
		t.Skip("set HINT_HARBOR to the harbor executable")
	}
	ctx := context.Background()
	h, err := runner.FindHarbor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	work := t.TempDir()
	if keep := os.Getenv("HINT_HARBOR_JOBS"); keep != "" {
		work = filepath.Join(keep, "qualify-"+time.Now().UTC().Format("20060102T150405"))
		os.MkdirAll(work, 0o755)
	}
	start := time.Now()
	res, err := Run(ctx, l, h, rec, Options{Work: work, Settings: runner.Settings{AgentSeconds: 300, VerifierSeconds: 300, BuildSeconds: 900, CPUs: 2, MemoryMB: 2048, Pids: 512}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("harbor %s: state %s reasons %v trials %d in %s", h.Version, res.State, res.Reasons, res.Trials, time.Since(start).Round(time.Second))
	for _, c := range res.Checks {
		t.Logf("  %-18s %-14s runs %d", c.Name, c.Status, c.Runs)
	}
	if res.State != "validated" {
		t.Fatalf("state %s", res.State)
	}
}
