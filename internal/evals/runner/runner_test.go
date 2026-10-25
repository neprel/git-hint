package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
)

func TestExportLayoutAndIsolation(t *testing.T) {
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	dir := filepath.Join(t.TempDir(), "task")
	task, err := Export(l, rec, dir, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(b)
	}
	toml := read("task.toml")
	// the verifier runs the agent's code: no network (found on the Linux run: it had the
	// compose default network)
	if v, _, _ := strings.Cut(strings.SplitN(toml, "[verifier]", 2)[1], "[agent]"); !strings.Contains(v, `network_mode = "no-network"`) {
		t.Errorf("the verifier has network: %s", v)
	}
	for _, want := range []string{`environment_mode = "separate"`, `artifacts = ["/app"]`, `network_mode = "public"`} {
		if !strings.Contains(toml, want) {
			t.Errorf("task.toml lacks %s", want)
		}
	}
	if c := read("environment/docker-compose.yaml"); !strings.Contains(c, `network_mode: "none"`) || !strings.Contains(c, "pids_limit") || !strings.Contains(c, "no-new-privileges") {
		t.Errorf("compose %q", c)
	}
	if !strings.HasPrefix(read("instruction.md"), "<!-- canary "+task.Canary) {
		t.Error("instruction without the canary line")
	}
	// the agent build context holds the base only
	filepath.Walk(filepath.Join(dir, "environment"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "price - discount") || strings.Contains(string(b), "TestDiscount") {
				t.Errorf("result or hidden test in the agent context: %s", p)
			}
		}
		if err == nil && fi.IsDir() && fi.Name() == ".git" {
			t.Errorf("Git metadata in the agent context")
		}
		return nil
	})
	if !strings.Contains(read("tests/hidden/discount_test.go"), "TestDiscount") || !strings.Contains(read("solution/files/cart.go"), "price - discount") {
		t.Error("verifier or solution content missing")
	}
	if !strings.Contains(read("tests/Dockerfile"), "COPY . /tests") || read("tests/deps/go.mod") == "" {
		t.Error("verifier image recipe incomplete")
	}
	// The live runner uses uid 65534. Git checks ownership of both the worktree
	// and .git; overlaying candidate files alone leaves image-created .git root-owned.
	environment := read("environment/Dockerfile")
	if !strings.Contains(environment, "commit -qm init && chown -hR 65534:65534 /app\n") {
		t.Error("the completed agent repository is not assigned to the live agent")
	}
	if strings.Contains(environment, "safe.directory") {
		t.Error("repository ownership must not be bypassed with Git trust settings")
	}
	// the export is a deterministic function of the manifest
	d1, _ := DirDigest(dir)
	dir2 := filepath.Join(t.TempDir(), "task")
	if _, err := Export(l, rec, dir2, Settings{}); err != nil {
		t.Fatal(err)
	}
	if d2, _ := DirDigest(dir2); d1 != d2 {
		t.Fatal("two exports of one eval version differ")
	}
	if _, err := Export(l, rec, dir, Settings{}); err == nil {
		t.Fatal("export into a non-empty directory")
	}
}

func TestReadJob(t *testing.T) {
	job := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(job, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	write("t1/result.json", `{"trial_name":"t1","verifier_result":{"rewards":{"reward":1.0}},"agent_result":{"n_input_tokens":10,"n_output_tokens":2,"n_cache_tokens":null},
	 "started_at":"2026-10-17T12:00:00Z","finished_at":"2026-10-17T12:01:30Z","agent_execution":{"started_at":"2026-10-17T12:00:10Z","finished_at":"2026-10-17T12:01:00Z"}}`)
	write("t1/verifier/test-stdout.txt", "noise\nCHECK C1 PASS\nCHECK C2 PASS\n")
	write("t2/result.json", `{"trial_name":"t2","verifier_result":null,"exception_info":{"exception_type":"AgentTimeoutError"}}`)
	write("t3/result.json", `{"trial_name":"t3","verifier_result":{"rewards":{"reward":0.0}}}`)
	write("t3/verifier/test-stdout.txt", "CHECK C1 FAIL\nCHECK C2 PASS\nCHECK C1 PASS\n")
	write("t4/result.json", `{"trial_name":"t4","verifier_result":{"rewards":{"reward":1.0}}}`)
	write("t4/verifier/test-stdout.txt", "TEST C1 a_test.go::TestA PASS\nTEST C1 a_test.go::TestB FAIL\nCHECK C1 PASS\nCHECK C2 PASS\n")
	write("t5/result.json", `{"trial_name":"t5","verifier_result":{"rewards":{"reward":1.0}}}`)
	write("t5/verifier/test-stdout.txt", "TEST C1 a_test.go::TestA PASS\nTEST C1 a_test.go::TestA PASS\nCHECK C1 PASS\nCHECK C2 PASS\n")
	ts, err := ReadJob(job)
	if err != nil || len(ts) != 5 {
		t.Fatalf("%v %d", err, len(ts))
	}
	if ts[0].Outcome() != "passed" || ts[0].Checks["C1"] != "pass" || *ts[0].WallSeconds != 90 || *ts[0].InputTokens != 10 || ts[0].CacheTokens != nil {
		t.Fatalf("t1 %+v", ts[0])
	}
	if ts[1].Outcome() != "timeout" || ts[2].Outcome() != "verifier_error" || !ts[2].Tampered {
		t.Fatalf("t2 %s t3 %s %v", ts[1].Outcome(), ts[2].Outcome(), ts[2].Checks)
	}
	// G10-2: per-test results are read; a passing check with a failed test, or a repeated
	// test line, is not a pass
	if ts[3].Tests["C1 a_test.go::TestB"] != "fail" || ts[3].Outcome() != "verifier_error" {
		t.Fatalf("t4 %s %v", ts[3].Outcome(), ts[3].Tests)
	}
	if !ts[4].Tampered || ts[4].Outcome() != "verifier_error" {
		t.Fatalf("t5 %s", ts[4].Outcome())
	}
}
