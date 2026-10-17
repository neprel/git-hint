package suites

import (
	"fmt"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
)

func episode(id string, links ...string) map[string]any {
	src := library.Digest([]byte("s"))
	var ls []any
	for _, x := range links {
		ls = append(ls, map[string]any{"kind": "depends_on", "episode_id": x, "status": "inferred"})
	}
	if ls == nil {
		ls = []any{}
	}
	return map[string]any{"record": "episode", "manifest_version": 1, "episode_id": id, "created_at": "2026-10-17T00:00:00Z", "rules_version": "0.1.0",
		"sources":      []any{map[string]any{"agent": "opencode", "parser_version": "0.1.0", "source": src, "session": "s", "role": "primary"}},
		"boundary":     map[string]any{"basis": "inferred", "start": map[string]any{"source": src, "record": "u"}, "rule": "r", "support": "high"},
		"requests":     []any{map[string]any{"at": map[string]any{"source": src, "record": "u"}}},
		"repositories": []any{map[string]any{"repo": "shop", "base_evidence": "unavailable"}}, "links": ls, "attribution": "exclusive", "reasons": []any{}}
}

func TestFamiliesAndPartitions(t *testing.T) {
	l := evaltest.Library(t)
	for _, e := range []map[string]any{episode("ep-000000000000000a"), episode("ep-000000000000000b", "ep-000000000000000a"), episode("ep-000000000000000c")} {
		if _, err := l.PutManifest("episode", e); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Families(l)
	if err != nil {
		t.Fatal(err)
	}
	if f["ep-000000000000000a"] != f["ep-000000000000000b"] || f["ep-000000000000000a"] == f["ep-000000000000000c"] {
		t.Fatalf("families %v", f)
	}
	if Partition(f["ep-000000000000000a"]) != Partition(f["ep-000000000000000b"]) {
		t.Fatal("related tasks in different partitions")
	}
	held := 0
	for i := 0; i < 1000; i++ {
		if Partition(fmt.Sprintf("tf-%016x", i)) == "held_out" {
			held++
		}
	}
	if held < 230 || held > 370 {
		t.Fatalf("held-out share %d/1000", held)
	}
}

func TestFreezeTakesValidatedVersionsOnly(t *testing.T) {
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	if _, err := l.PutManifest("eval", rec); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := Freeze(l, nil, time.Now()); err != ErrEmpty || len(skipped) != 1 {
		t.Fatalf("packaged eval entered a suite: %v %v", err, skipped)
	}
	v2 := map[string]any{}
	for k, v := range rec {
		v2[k] = v
	}
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	if _, err := l.PutManifest("eval", v2); err != nil {
		t.Fatal(err)
	}
	id, _, err := Freeze(l, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	l.Manifest("suite", id, &s)
	m := s["members"].([]any)[0].(map[string]any)
	if m["eval"] != fmt.Sprintf("%s@2", rec["eval_id"]) {
		t.Fatalf("member %v", m)
	}
	if again, _, _ := Freeze(l, nil, time.Now().Add(time.Hour)); again != id {
		t.Fatal("the same set gave another suite")
	}
	// a later version does not change the frozen suite
	v3 := map[string]any{}
	for k, v := range v2 {
		v3[k] = v
	}
	v3["version"], v3["previous"] = 3, fmt.Sprintf("%s@2", rec["eval_id"])
	l.PutManifest("eval", v3)
	var still map[string]any
	l.Manifest("suite", id, &still)
	if still["members"].([]any)[0].(map[string]any)["eval"] != m["eval"] {
		t.Fatal("a frozen suite changed")
	}
}

func TestTrialRecords(t *testing.T) {
	l := evaltest.Library(t)
	ten := int64(10)
	reward := 0.0
	timeout := runner.Trial{Name: "t1", Exception: "AgentTimeoutError", Checks: map[string]string{"C1": "fail"}}
	in := TrialInput{Suite: "su-0123456789abcdef", Eval: "ev-0123456789abcdef@2", Attempt: 1, Arm: "baseline", Agent: "opencode", AgentVersion: "2.0.26",
		Model: "local", RunnerVersion: "0.23.0", Started: time.Now(), MaxRequests: 10, MaxSeconds: 600, Requests: &ten}
	name, err := RecordTrial(l, in, timeout)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	l.Manifest("trial", name, &rec)
	c := rec["counters"].(map[string]any)
	if rec["outcome"] != "timeout" || rec["budget"].(map[string]any)["stop_reason"] != "time_limit" || c["input_tokens"] != nil || c["requests"].(float64) != 10 {
		t.Fatalf("timeout trial %v", rec)
	}
	in.Attempt, in.Stop = 2, "request_limit"
	name, err = RecordTrial(l, in, runner.Trial{Name: "t2", Reward: &reward, Checks: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	l.Manifest("trial", name, &rec)
	if rec["outcome"] != "budget_exhausted" {
		t.Fatalf("request limit trial %v", rec["outcome"])
	}
	if err := evals.ValidateValue("trial", rec); err != nil {
		t.Fatal(err)
	}
}
