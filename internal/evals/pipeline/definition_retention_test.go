package pipeline

import (
	"context"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"testing"
	"time"
)

// The normal AfterCollect call arrives after CLI/MCP releases the state lock.
// This uses the existing pure command fixture; no server, agent or engine starts.
func TestNormalPipelinePrunesWithoutDefinitionReferences(t *testing.T) {
	s, env, _ := setup(t)
	p := evals.Enabled([]string{"opencode"})
	s.Cfg.Evals = &p
	l := library.Open(s, 0)
	old := evaltest.GoBugFix(t, l)
	old["created_at"] = "2000-01-01T00:00:00Z"
	name, e := l.PutManifest("eval", old)
	if e != nil {
		t.Fatal(e)
	}
	env.Now = func() time.Time { return time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) }
	got := Run(context.Background(), s, env)
	if !got.Ran || got.Pruned < 1 {
		t.Fatalf("ordinary pipeline retention skipped: %+v", got)
	}
	var after map[string]any
	if e = l.Manifest("eval", name, &after); e == nil {
		t.Fatal("old manifest survived normal pipeline prune")
	}
}
