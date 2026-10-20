package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
)

// C5 minimization: the runner's local provenance (image IDs, engine identity, toolchain
// output, manifest digests) never enters the trial record or the eval report.
func TestProvenanceStaysLocal(t *testing.T) {
	const canary = "HINTCANARY_PROVENANCE_9f3"
	job := t.TempDir()
	dir := filepath.Join(job, "j__1")
	os.MkdirAll(filepath.Join(dir, "verifier"), 0o700)
	prov := map[string]any{"engine": map[string]any{"id": canary, "kernel": canary}, "toolchain": map[string]any{"output": "go version " + canary},
		"base_images":          map[string]any{"environment": []any{map[string]any{"pinned": "x@sha256:" + strings.Repeat("a", 64), "resolved": map[string]any{"id": "sha256:" + canary}}}},
		"dependency_manifests": map[string]any{"files": []any{map[string]any{"path": canary, "digest": canary}}}}
	res, _ := json.Marshal(map[string]any{"trial_name": "j__1", "verifier_result": map[string]any{"rewards": map[string]any{"reward": 1}},
		"hint_container": map[string]any{"result_trust": "same_process", "check_trust": map[string]any{"C1": "same_process", "C2": "same_process"},
			"agent_image": "sha256:" + canary, "provenance": prov}})
	os.WriteFile(filepath.Join(dir, "result.json"), res, 0o600)
	os.WriteFile(filepath.Join(dir, "verifier", "test-stdout.txt"), []byte("CHECK C1 PASS\nCHECK C2 PASS\n"), 0o600)
	ts, err := runner.ReadJob(job)
	if err != nil || len(ts) != 1 {
		t.Fatal(ts, err)
	}
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	l.PutManifest("eval", rec)
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	l.PutManifest("eval", v2)
	suite, _, err := suites.Freeze(l, nil, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	name, err := suites.RecordTrial(l, suites.TrialInput{Suite: suite, Eval: fmt.Sprintf("%s@2", rec["eval_id"]), Arm: "baseline", Agent: "oracle",
		RunnerVersion: "0.23.0", MaxSeconds: 900, Attempt: 1, Started: time.Date(2026, 10, 18, 9, 1, 0, 0, time.UTC)}, ts[0])
	if err != nil {
		t.Fatal(err)
	}
	var trial map[string]any
	l.Manifest("trial", name, &trial)
	tb, _ := json.Marshal(trial)
	b, _, err := Builder{Key: fixedKey, Namespace: fixedNS}.Build(l, []string{name}, time.Date(2026, 10, 18, 9, 41, 7, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tb), canary) || strings.Contains(string(b), canary) {
		t.Fatal("local provenance reached the trial record or the report")
	}
	if trial["result_trust"] != "same_process" {
		t.Fatalf("trust %v", trial["result_trust"])
	}
}
