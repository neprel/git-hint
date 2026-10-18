package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
)

// TestMeasureStorage prints the storage of the synthetic bug fix through the pipeline
// (session -> packaged -> qualified -> one trial) with the fake runner. Set
// HINT_MEASURE=1. The numbers describe this fixture only.
func TestMeasureStorage(t *testing.T) {
	if os.Getenv("HINT_MEASURE") == "" {
		t.Skip("set HINT_MEASURE=1")
	}
	s, env := e2eProject(t)
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"}
	s.Cfg.Evals = &p
	l := library.Open(s, 0)
	u0, _ := l.Usage()
	Run(context.Background(), s, env)
	u1, _ := l.Usage()
	names, _ := l.Manifests("eval")
	f, _ := MapDraft(l, names[0])
	f.Context = []string{"Total ignores the discount."}
	f.Requirements[0].Quote, f.Requirements[0].Tests = "Fix Total so it subtracts the discount from the price", []string{"discount_test.go::TestDiscount"}
	b, _ := json.Marshal(f)
	ApplyMapFile(context.Background(), l, f.Eval, b, "", env.Now())
	Qualify(context.Background(), s, &e2eRunner{agentLog: 0}, env.Now())
	u2, _ := l.Usage()
	names, _ = l.Manifests("eval")
	var rec map[string]any
	l.Manifest("eval", names[len(names)-1], &rec)
	size := func(d any) int {
		b, _ := l.GetObject(d.(string))
		return len(b)
	}
	repo := rec["repositories"].([]any)[0].(map[string]any)
	t.Logf("library: empty %d B; after import+packaging %d B; after map+qualification %d B", u0, u1, u2)
	t.Logf("eval objects: base archive %d B, verifier archive %d B, reference %d B, instruction %d B; versions %d",
		size(repo["base_archive"]), size(rec["verifier"].(map[string]any)["archive"]), size(rec["reference"]), size(rec["instruction"]), len(names))
	if rs, _ := rec["qualification"].(map[string]any)["checks"].([]any); len(rs) > 0 {
		if ev, ok := rs[0].(map[string]any)["evidence"].([]any); ok && len(ev) > 0 {
			t.Logf("qualification run archive (fake runner) %d B", size(ev[len(ev)-1]))
		}
	}
}
