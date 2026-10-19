package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// workEvents are reduced plugin events of one agent run: an edit, then optionally a passing
// test (go test) after it.
func workEvents(root, session string, test bool, t0 int64) string {
	ev := func(hook, tool, call string, t int64, args, meta map[string]any) string {
		out := map[string]any{"args": args}
		if hook == "tool.execute.after" {
			out = map[string]any{"metadata": meta, "status": "completed", "output": "[redacted:len=10]"}
		}
		b, _ := json.Marshal(map[string]any{"hook": hook, "t_wall_ms": t, "input": map[string]any{"tool": tool, "sessionID": session, "callID": call}, "output": out})
		return string(b) + "\n"
	}
	ledger := filepath.ToSlash(filepath.Join(root, "canary_dir_e1d2/billing/ledger.go"))
	var b strings.Builder
	b.WriteString(ev("tool.execute.before", "edit", session+"-e", t0, map[string]any{"filePath": ledger}, nil))
	b.WriteString(ev("tool.execute.after", "edit", session+"-e", t0+10, nil, map[string]any{"files": []any{map[string]any{"filePath": ledger}}}))
	if test {
		b.WriteString(ev("tool.execute.before", "bash", session+"-t", t0+1000, map[string]any{"command": "go test ./..."}, nil))
		b.WriteString(ev("tool.execute.after", "bash", session+"-t", t0+1500, nil, map[string]any{"exit": 0}))
	}
	return b.String()
}

// member runs one agent task of a work unit.
func member(t *testing.T, dir, unit, task string, test bool, offset time.Duration) result {
	t.Helper()
	t.Setenv("HINT_WORK_UNIT", unit)
	t.Setenv("HINT_TASK_ID", task)
	defer t.Setenv("HINT_WORK_UNIT", "")
	defer t.Setenv("HINT_TASK_ID", "")
	return run(t, dir, workEvents(dir, "ses_"+task, test, time.Now().Add(offset).UnixMilli()), "adapter", "opencode", "--project", dir)
}

func workReport(t *testing.T, dir string) map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "r.json")
	must(t, run(t, dir, "", "eval", "report", "-o", out))
	b, _ := os.ReadFile(out)
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func revisions(r map[string]any) []map[string]any {
	var out []map[string]any
	for _, w := range r["work_units"].([]any) {
		out = append(out, w.(map[string]any))
	}
	return out
}

func TestWorkUnitLifecycleWithoutPlan(t *testing.T) {
	shop, _ := setup(t)
	t.Setenv("HINT_TASK_FAMILY", "")
	must(t, run(t, shop, "", "eval", "work", "start", "rework", "--family", "billing"))
	r := workReport(t, shop)
	if len(r["trials"].([]any)) != 0 || len(revisions(r)) != 1 || revisions(r)[0]["state"] != "open" {
		t.Fatalf("first report: %v", r)
	}
	must(t, member(t, shop, "rework", "task-1", false, 0))
	must(t, run(t, shop, "", "eval", "work", "close", "rework", "--outcome", "failed")) // another attempt may follow
	must(t, member(t, shop, "rework", "task-2", true, time.Second))
	if r := run(t, shop, "", "eval", "work", "close", "rework", "--outcome", "abandoned"); r.code != 0 {
		t.Fatalf("abandon: %+v", r)
	}
	revs := revisions(workReport(t, shop))
	last := revs[len(revs)-1]
	atts := last["attempts"].([]any)
	if last["state"] != "closed" || len(atts) != 2 || atts[0].(map[string]any)["outcome"] != "failed" || atts[1].(map[string]any)["outcome"] != "abandoned" {
		t.Fatalf("attempts: %v", last)
	}
	// the chain: every revision names its predecessor; a failed/abandoned attempt needs no change set
	for i, rv := range revs {
		if i > 0 && rv["predecessor"] != revs[i-1]["revision_digest"] {
			t.Fatalf("broken chain at %d", i)
		}
		for _, a := range rv["attempts"].([]any) {
			if a.(map[string]any)["application_change_set"] != nil {
				t.Fatal("an attempt carries a change set")
			}
		}
	}
	// usage is unknown (the plugin reports none), never zero
	u := atts[0].(map[string]any)["usage"].(map[string]any)
	if u["input_tokens"] != nil {
		t.Fatalf("usage: %v", u)
	}
}

func TestWorkUnitRefusals(t *testing.T) {
	shop, _ := setup(t)
	s, _ := store.Open(shop)
	must(t, run(t, shop, "", "eval", "work", "start", "a", "--family", "f"))
	must(t, run(t, shop, "", "eval", "work", "start", "b", "--family", "f"))
	if r := run(t, shop, "", "eval", "work", "start", "a", "--family", "f"); r.code == 0 {
		t.Fatal("a unit started twice")
	}
	t.Setenv("HINT_WORK_UNIT", "a")
	if r := run(t, shop, workEvents(shop, "s", false, 0), "adapter", "opencode", "--project", shop); r.code == 0 || !strings.Contains(r.stderr, "HINT_TASK_ID") {
		t.Fatalf("work unit without a task: %+v", r)
	}
	t.Setenv("HINT_WORK_UNIT", "")
	if r := member(t, shop, "unknown", "t1", false, 0); r.code == 0 || !strings.Contains(r.stderr, "nothing was written") {
		t.Fatalf("unknown unit: %+v", r)
	}
	must(t, member(t, shop, "a", "t1", false, 0))
	if r := member(t, shop, "b", "t1", false, 0); r.code == 0 || !strings.Contains(r.stderr, "another work unit") {
		t.Fatalf("a task in two units: %+v", r)
	}
	if r := run(t, shop, "", "eval", "work", "close", "a", "--outcome", "completed"); r.code == 0 || !strings.Contains(r.stderr, "no passed test") {
		t.Fatalf("completed without validation: %+v", r)
	}
	if obs, _, _ := project.ReadSpool(s); len(obs) == 0 {
		t.Fatal("the valid member run wrote nothing")
	}
}

func TestWorkUnitMultiCollectCompletionAndFrozenAfter(t *testing.T) {
	shop, _ := setup(t)
	srv := singleRepoServer(t, shop)
	defer srv.Close()
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	planID := firstPlan(t, shop)
	must(t, run(t, shop, "", "eval", "work", "start", "split", "--plan", planID, "--family", "billing"))
	must(t, member(t, shop, "split", "t1", false, 0))
	change(t, shop, "func StepOne() int { return 1 }\n") // an intermediate snapshot
	must(t, member(t, shop, "split", "t2", true, time.Second))
	change(t, shop, "func StepTwo() int { return 2 }\n")
	if r := run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split"); r.code != 2 {
		t.Fatalf("--work-unit without --after: %+v", r)
	}
	if r := run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"); r.code == 0 {
		t.Fatal("declared before the attempt was closed")
	}
	must(t, run(t, shop, "", "eval", "work", "close", "split", "--outcome", "completed"))
	if r := run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", strings.Repeat("9", 32)); r.code == 0 {
		t.Fatal("an unknown after snapshot was accepted")
	}
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	s, _ := store.Open(shop)
	latest, _ := project.LoadLatest(s, shopRepo(t, s))
	frozen := latest.Record.SnapshotID
	change(t, shop, "func Later() int { return 3 }\n") // a later collect does not move the declaration
	out := t.TempDir()
	must(t, run(t, shop, "", "export", "--out", out))
	var declared []map[string]any
	for _, c := range changeSets(t, out) {
		if c["payload"].(map[string]any)["plan_application"] != nil {
			declared = append(declared, c)
		}
	}
	if len(declared) != 1 {
		t.Fatalf("declared change sets: %d", len(declared))
	}
	p := declared[0]["payload"].(map[string]any)
	work := p["plan_application"].(map[string]any)["work"].(map[string]any)
	if p["after_snapshot"].(map[string]any)["snapshot_id"] != frozen || work["attempt_ref"] == nil || work["revision"] == nil {
		t.Fatalf("declared: %v", p)
	}
	// the base is the plan snapshot, two snapshots before the after snapshot
	if p["before_snapshot"].(map[string]any)["snapshot_id"] == latest.Record.ParentSnapshotIDs[0] {
		t.Fatal("the declared change starts at the parent, not at the unit base")
	}
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL)) // the server accepts it
	// late activity after the closure keeps the evidence and marks the unit stale
	must(t, member(t, shop, "split", "t2", false, 2*time.Second))
	revs := revisions(workReport(t, shop))
	atts := revs[len(revs)-1]["attempts"].([]any)
	if late := atts[len(atts)-1].(map[string]any)["late_tasks"]; late == nil {
		t.Fatalf("late activity not recorded: %v", revs[len(revs)-1])
	}
}

// singleRepoServer is a test server whose plans cover the shop repository only.
func singleRepoServer(t *testing.T, shop string) *testkit.Server {
	t.Helper()
	s, _ := store.Open(shop)
	id := shopRepo(t, s)
	srv := testkit.NewServer()
	srv.Mutate = func(doc map[string]any) {
		for _, p := range doc["payload"].(map[string]any)["plans"].([]any) {
			pm := p.(map[string]any)
			pm["repo_ids"] = []any{id}
			var keep []any
			for _, x := range pm["snapshots"].([]any) {
				if x.(map[string]any)["repo_id"] == id {
					keep = append(keep, x)
				}
			}
			pm["snapshots"] = keep
		}
	}
	return srv
}

func shopRepo(t *testing.T, s *store.Store) string {
	t.Helper()
	shop := filepath.Dir(s.Path())
	for _, r := range s.Cfg.Repos {
		if filepath.Base(s.RepoRoot(r)) == filepath.Base(shop) {
			return s.RepoID(r)
		}
	}
	t.Fatal("no shop repository")
	return ""
}

func firstPlan(t *testing.T, shop string) string {
	t.Helper()
	s, _ := store.Open(shop)
	saved, err := project.LatestRecommendation(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Payload struct {
			Plans []struct {
				PlanID string `json:"plan_id"`
			} `json:"plans"`
		} `json:"payload"`
	}
	b, _ := os.ReadFile(saved)
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Payload.Plans) == 0 {
		t.Fatalf("saved set: %v", err)
	}
	return doc.Payload.Plans[0].PlanID
}

// modelMember runs one agent task of a work unit whose plugin forwards model calls (OpenCode
// 2.0.26): one primary call; closed is false for a run without its closure line.
func modelMember(t *testing.T, dir, unit, task string, closed bool) result {
	t.Helper()
	t.Setenv("HINT_WORK_UNIT", unit)
	t.Setenv("HINT_TASK_ID", task)
	defer t.Setenv("HINT_WORK_UNIT", "")
	defer t.Setenv("HINT_TASK_ID", "")
	t0 := time.Now().UnixMilli()
	var b strings.Builder
	line := func(dt int64, m map[string]any) {
		m["t_wall_ms"] = t0 + dt
		j, _ := json.Marshal(m)
		b.Write(j)
		b.WriteByte('\n')
	}
	ses := "ses_" + task
	evt := func(dt int64, e map[string]any) {
		e["sessionID"] = ses
		line(dt, map[string]any{"hook": "event", "event": e})
	}
	line(0, map[string]any{"hook": "hint.attach"})
	evt(1, map[string]any{"id": task + "-1", "type": "session.created"})
	line(2, map[string]any{"hook": "model.request", "input": map[string]any{"sessionID": ses, "kind": "primary"}})
	evt(3, map[string]any{"id": task + "-2", "type": "session.step.started", "assistantMessageID": task + "-m"})
	evt(4, map[string]any{"id": task + "-3", "type": "session.step.ended", "assistantMessageID": task + "-m",
		"tokens": map[string]any{"input": 1000, "output": 50, "reasoning": 10, "cache": map[string]any{"read": 0, "write": 0}}})
	if closed {
		line(5, map[string]any{"hook": "hint.close", "close": map[string]any{"drained": true, "dropped": 0, "errors": 0}})
	}
	return run(t, dir, b.String(), "adapter", "opencode", "--project", dir, "--agent-version", "2.0.26", "--model-calls")
}

func TestAttemptUsageNeedsCompleteCoverageOfEveryMemberTask(t *testing.T) {
	shop, _ := setup(t)
	t.Setenv("HINT_TASK_FAMILY", "")
	must(t, run(t, shop, "", "eval", "work", "start", "covered", "--family", "billing"))
	must(t, modelMember(t, shop, "covered", "task-a", true))
	must(t, modelMember(t, shop, "covered", "task-b", true))
	must(t, run(t, shop, "", "eval", "work", "close", "covered", "--outcome", "failed"))
	must(t, modelMember(t, shop, "covered", "task-c", true))
	must(t, modelMember(t, shop, "covered", "task-d", false)) // no closure line: interrupted
	must(t, run(t, shop, "", "eval", "work", "close", "covered", "--outcome", "abandoned"))
	revs := revisions(workReport(t, shop))
	atts := revs[len(revs)-1]["attempts"].([]any)
	full, part := atts[0].(map[string]any), atts[1].(map[string]any)
	u := full["usage"].(map[string]any)
	// two tasks of 1000 input and 60 generated output each; cache reported as unknown zeros
	if u["input_tokens"] != 2000.0 || u["output_tokens"] != 120.0 || u["requests"] != nil || u["cache_tokens"] != nil ||
		full["usage_provenance"] != "activity_usage" || full["usage_normalization"] != "opencode-2.0.26:tokeninfo-v1" ||
		full["coverage"].(map[string]any)["tasks_with_complete_usage"] != 2.0 {
		t.Fatalf("complete attempt: %v", full)
	}
	pu := part["usage"].(map[string]any)
	if pu["input_tokens"] != nil || part["usage_provenance"] != "unknown" || part["usage_normalization"] != nil ||
		part["coverage"].(map[string]any)["tasks_with_complete_usage"] != 1.0 {
		t.Fatalf("partial attempt: never a partial sum: %v", part)
	}
}
