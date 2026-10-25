package pipeline

import (
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/runner"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrelaunchViewSharedCoreCopiesAndReusesGraph(t *testing.T) {
	s, l := behaviorProject(t)
	names, _ := l.Manifests("eval")
	var rec map[string]any
	if e := l.Manifest("eval", names[len(names)-1], &rec); e != nil {
		t.Fatal(e)
	}
	artifact := filepath.Join(t.TempDir(), "capture.tar")
	digest, e := writeObservedArchive(artifact)
	if e != nil {
		t.Fatal(e)
	}
	capture := runner.PrelaunchTreeCapture{State: "complete", Digest: digest, Clock: "ledger_anchor_monotonic_ms", ExecutionDigest: "sha256:" + strings.Repeat("a", 64)}
	materializations := 0
	r, e := buildObservedStart(context.Background(), s, rec, capture, func(_ context.Context, dst string) ([]string, error) {
		materializations++
		return runner.MaterializePrelaunchTree(artifact, digest, dst)
	})
	if e != nil {
		t.Fatal(e)
	}
	defer r.close()
	v := &ObservedPrelaunch{result: r, inventory: runner.PrelaunchInventory{Digest: digest, Entries: []runner.PrelaunchEntry{{Path: "kept"}}}}
	graph := v.Graph()
	var filePath, id string
	for i, e := range graph.Entities {
		if e.Kind == "file" {
			filePath = e.Path
			id = e.ID
			graph.Entities[i].Path = "mutated"
			graph.Entities[i].Hash = "changed"
		}
	}
	if filePath == "" || materializations != 1 {
		t.Fatal("missing actual graph")
	}
	// Remove the raw audit path. The owned graph/resolver remain the single result.
	if e := os.Remove(artifact); e != nil {
		t.Fatal(e)
	}
	if got, ok := r.resolve("/app/" + filePath); !ok || got != id {
		t.Fatal("resolver changed")
	}
	if materializations != 1 || v.Graph().Entities[0].Path == "mutated" {
		t.Fatal("graph rerun or alias")
	}
	inv := v.Inventory()
	inv.Entries[0].Path = "mutated"
	if v.Inventory().Entries[0].Path != "kept" {
		t.Fatal("inventory alias")
	}
	if _, e := json.Marshal(v); e == nil {
		t.Fatal("wire view")
	}
	if !sameObservedCapture(capture, v.Capture()) {
		t.Fatal("capture changed")
	}
}
func TestPrelaunchViewPredispatchRequiresActualCapture(t *testing.T) {
	_, e := RunSuite(context.Background(), nil, nil, "", SuiteOptions{PrelaunchCheck: func(context.Context, *ObservedPrelaunch) error { return nil }}, time.Time{})
	if e == nil {
		t.Fatal("missing capture accepted")
	}
}

func TestPrelaunchPreparedResultRequiresExactCompletedEnclosure(t *testing.T) {
	capture := runner.PrelaunchTreeCapture{State: "complete", Digest: "sha256:" + strings.Repeat("a", 64), ExecutionDigest: "sha256:" + strings.Repeat("b", 64), Clock: "ledger_anchor_monotonic_ms", StartedMs: 1, FinishedMs: 2}
	retained := &observedStartResult{capture: capture}
	makeJob := func() jobResult {
		var j jobResult
		c := capture
		c.CheckState = "complete"
		c.CheckStartedMs = 2
		c.CheckFinishedMs = 3
		j.Container.PrelaunchTree = &c
		start := int64(4)
		j.Container.Ledger = &runner.LedgerReport{AgentRun: &runner.ReportAgentRun{ExecutionDigest: capture.ExecutionDigest, EnclosureStartMs: &start}}
		j.Container.AgentBinary.ExecutionDigest = capture.ExecutionDigest
		return j
	}
	if !preparedObservedStartMatches(makeJob(), retained) {
		t.Fatal("valid prestart result refused")
	}
	for name, mutate := range map[string]func(*jobResult){"missing": func(j *jobResult) { j.Container.PrelaunchTree = nil }, "unfinished": func(j *jobResult) { j.Container.PrelaunchTree.CheckState = "failed" }, "before_capture": func(j *jobResult) { j.Container.PrelaunchTree.CheckStartedMs = 1 }, "after_start": func(j *jobResult) { j.Container.PrelaunchTree.CheckFinishedMs = 5 }, "other_bytes": func(j *jobResult) { j.Container.PrelaunchTree.Digest = "sha256:" + strings.Repeat("c", 64) }, "other_execution": func(j *jobResult) { j.Container.AgentBinary.ExecutionDigest = "other" }} {
		t.Run(name, func(t *testing.T) {
			j := makeJob()
			mutate(&j)
			if preparedObservedStartMatches(j, retained) {
				t.Fatal("invalid retained binding accepted")
			}
		})
	}
	if preparedObservedStartMatches(makeJob(), nil) {
		t.Fatal("missing prestart result permitted backfill")
	}
}
