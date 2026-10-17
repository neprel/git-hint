package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
)

// TestHarborNopAndOracle runs the exported Go bug fix through the real Harbor CLI with no
// model: the base must fail, the reference must pass. Set HINT_HARBOR to run it (it needs
// Docker and builds images). HINT_HARBOR_JOBS keeps the job directories for inspection.
func TestHarborNopAndOracle(t *testing.T) {
	if os.Getenv("HINT_HARBOR") == "" {
		t.Skip("set HINT_HARBOR to the harbor executable")
	}
	ctx := context.Background()
	h, err := FindHarbor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	task := filepath.Join(t.TempDir(), "task")
	if _, err := Export(l, rec, task, Settings{AgentSeconds: 300, VerifierSeconds: 300, BuildSeconds: 900, CPUs: 2, MemoryMB: 2048, Pids: 512}); err != nil {
		t.Fatal(err)
	}
	jobs := os.Getenv("HINT_HARBOR_JOBS")
	if jobs == "" {
		jobs = t.TempDir()
	}
	stamp := time.Now().UTC().Format("20060102T150405")
	nop, err := h.Run(ctx, task, RunOptions{Agent: "nop", JobsDir: jobs, JobName: "nop-" + stamp, Timeout: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(nop) != 1 || nop[0].Outcome() != "failed" || nop[0].Checks["C1"] != "fail" || nop[0].Checks["C2"] != "pass" {
		t.Fatalf("nop: %+v", nop)
	}
	or, err := h.Run(ctx, task, RunOptions{Agent: "oracle", Attempts: 2, JobsDir: jobs, JobName: "oracle-" + stamp, Timeout: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(or) != 2 {
		t.Fatalf("oracle trials: %d", len(or))
	}
	for _, tr := range or {
		if tr.Outcome() != "passed" || tr.Checks["C1"] != "pass" || tr.Checks["C2"] != "pass" {
			t.Fatalf("oracle: %+v", tr)
		}
	}
	t.Logf("harbor %s: nop %s, oracle %s/%s", h.Version, nop[0].Outcome(), or[0].Outcome(), or[1].Outcome())
}
