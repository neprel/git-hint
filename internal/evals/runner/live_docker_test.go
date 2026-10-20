package runner

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// The live-attempt tests use a fake model on the host (no model, no LAN): it answers every
// chat request with a marker and OpenAI-style usage.
const realKey = "HINTCANARY-REAL-KEY-never-in-a-container"

func fakeModel(t *testing.T) (*Live, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+realKey {
			w.WriteHeader(401)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"HINT-MODEL-ANSWER"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HINT_TEST_LIVE_KEY", realKey)
	lv, err := StartLive(LiveConfig{BaseURL: srv.URL + "/v1", Model: "m", KeyEnv: "HINT_TEST_LIVE_KEY", AgentVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lv.Close() })
	return lv, &n
}

// liveAgent is the test agent: one chat request through the gate, then its environment.
const liveAgent = `path=${OPENAI_BASE_URL#http://gate:8080}
body='{"model":"m","messages":[{"role":"user","content":"hi"}]}'
exec 3<>/dev/tcp/gate/8080
printf 'POST %s/chat/completions HTTP/1.0\r\nHost: gate\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' "$path" ${#body} "$body" >&3
cat <&3 > /app/answer.txt
env > /app/env.txt
`

// liveTask is a task whose verifier passes when the agent's answer holds the model's marker.
func liveTask(t *testing.T, agentSeconds int) string {
	t.Helper()
	dir := syntheticTask(t, solveFixed, agentSeconds)
	os.WriteFile(filepath.Join(dir, "tests", "test.sh"), []byte(`#!/bin/sh
mkdir -p /logs/verifier
echo "NET $(ls /sys/class/net | tr '\n' ' ')"
if grep -q HINT-MODEL-ANSWER /app/answer.txt 2>/dev/null; then echo "CHECK C1 PASS"; echo 1 > /logs/verifier/reward.txt; else echo "CHECK C1 FAIL"; echo 0 > /logs/verifier/reward.txt; fi
`), 0o755)
	return dir
}

func noQ4Leftovers(t *testing.T, d *Docker) {
	t.Helper()
	nets, err := d.cli.NetworkList(context.Background(), client.NetworkListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets.Items {
		if n.Labels["hint"] == "q4" {
			t.Errorf("network %s left behind", n.Name)
		}
	}
}

func readResult(t *testing.T, jobs, job string) trialResult {
	t.Helper()
	var r trialResult
	b, _ := os.ReadFile(filepath.Join(jobs, job, job+"__1", "result.json"))
	json.Unmarshal(b, &r)
	return r
}

// A live attempt through the gate: the agent (the task's own agent image, agent policy)
// reaches the fake model only through the gate; the ledger is sealed with enforced egress
// and complete usage; the container never saw the real key; the verifier has no network;
// nothing is left behind.
func TestDockerLiveThroughGate(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	lv, calls := fakeModel(t)
	c := lv.NewTrial(10)
	jobs := t.TempDir()
	ts, err := d.Run(context.Background(), liveTask(t, 120), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: jobs, JobName: "live",
		Live: lv, Counter: c, agentCmd: []string{"bash", "-c", liveAgent}})
	if err != nil || len(ts) != 1 || ts[0].Outcome() != "passed" {
		t.Fatalf("%+v %v %s", ts, err, readResult(t, jobs, "live").Container["error"])
	}
	rep := c.Report()
	if rep == nil || rep.Bypass != BypassEnforced || rep.Scope != ScopeIsolatedContainer || rep.Counts[EntryComplete] != 1 || rep.ModelCheck != "verified" || calls.Load() != 1 {
		t.Fatalf("ledger %+v, model calls %d", rep, calls.Load())
	}
	if l, ok := readResult(t, jobs, "live").Container["ledger"].(map[string]any); !ok || l["bypass"] != BypassEnforced {
		t.Fatalf("ledger in the result: %v", l)
	}
	f, _ := os.Open(filepath.Join(jobs, "live", "live__1", "agent", "final.tar"))
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "app/env.txt" {
			b, _ := io.ReadAll(tr)
			if strings.Contains(string(b), realKey) || !strings.Contains(string(b), "OPENAI_BASE_URL=http://gate:8080/t/") {
				t.Fatalf("agent environment: %s", b)
			}
		}
	}
	out, _ := os.ReadFile(filepath.Join(jobs, "live", "live__1", "verifier", "test-stdout.txt"))
	if !strings.HasPrefix(string(out), "NET lo") {
		t.Fatalf("verifier network: %q", out)
	}
	noQ4Leftovers(t, d)
}

// Without a passed probe nothing is launched: the attempt is refused, the ledger is sealed
// empty with unverified egress, the model is never called.
func TestDockerLiveRefusedWithoutProbe(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	d.probeFn = func(context.Context) GateProbe { return GateProbe{Reason: "synthetic probe failure"} }
	lv, calls := fakeModel(t)
	c := lv.NewTrial(10)
	jobs := t.TempDir()
	ts, err := d.Run(context.Background(), liveTask(t, 120), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: jobs, JobName: "refused",
		Live: lv, Counter: c, agentCmd: []string{"bash", "-c", liveAgent}})
	if err != nil || len(ts) != 1 || ts[0].Exception != ErrGateRefused || ts[0].Outcome() == "passed" {
		t.Fatalf("%+v %v", ts, err)
	}
	if rep := c.Report(); rep == nil || rep.Bypass != BypassUnverified || rep.Counts[EntryComplete] != 0 || calls.Load() != 0 {
		t.Fatalf("ledger %+v calls %d", rep, calls.Load())
	}
	noQ4Leftovers(t, d)
}

// A hung agent times out and a cancelled run stops: both seal the ledger and leave no
// network, gate or agent container behind.
func TestDockerLiveTimeoutAndCancellation(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	lv, _ := fakeModel(t)
	jobs := t.TempDir()
	c := lv.NewTrial(10)
	ts, err := d.Run(context.Background(), liveTask(t, 3), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: jobs, JobName: "slow",
		Live: lv, Counter: c, agentCmd: []string{"sleep", "60"}})
	if err != nil || len(ts) != 1 || ts[0].Outcome() != "timeout" || c.Report() == nil {
		t.Fatalf("timeout: %+v %v", ts, err)
	}
	noQ4Leftovers(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	c2 := lv.NewTrial(10)
	go func() { time.Sleep(20 * time.Second); cancel() }()
	ts, _ = d.Run(ctx, liveTask(t, 120), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: jobs, JobName: "cancel",
		Live: lv, Counter: c2, agentCmd: []string{"sleep", "120"}})
	if len(ts) == 1 && ts[0].Outcome() == "passed" {
		t.Fatalf("a cancelled attempt passed: %+v", ts)
	}
	if c2.Report() == nil {
		t.Fatal("the cancelled attempt's ledger is not sealed")
	}
	noQ4Leftovers(t, d)
}

// The proxy's limits hold through the gate: with a limit of one request, the agent's second
// request is refused and counted; the account is then incomplete.
func TestDockerLiveRequestLimit(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	lv, calls := fakeModel(t)
	c := lv.NewTrial(1)
	twice := strings.Replace(liveAgent, "env > /app/env.txt", "exec 3<>/dev/tcp/gate/8080\nprintf 'POST %s/chat/completions HTTP/1.0\\r\\nHost: gate\\r\\nContent-Type: application/json\\r\\nContent-Length: %d\\r\\n\\r\\n%s' \"$path\" ${#body} \"$body\" >&3\ncat <&3 > /app/second.txt", 1)
	ts, err := d.Run(context.Background(), liveTask(t, 120), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: t.TempDir(), JobName: "limit",
		Live: lv, Counter: c, agentCmd: []string{"bash", "-c", twice}})
	if err != nil || len(ts) != 1 {
		t.Fatal(ts, err)
	}
	rep := c.Report()
	if rep == nil || rep.Counts[EntryComplete] != 1 || rep.Counts["rejected_limit"] != 1 || rep.State == "complete" || calls.Load() != 1 {
		t.Fatalf("ledger %+v calls %d", rep, calls.Load())
	}
}

// A passed probe for another gate image is no evidence for this one: the attempt is refused
// before launch.
func TestDockerLiveProbeImageMismatch(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	d.probeFn = func(context.Context) GateProbe {
		return GateProbe{OK: true, Key: "sha256:other", Image: "sha256:" + strings.Repeat("0", 64)}
	}
	lv, calls := fakeModel(t)
	c := lv.NewTrial(10)
	ts, err := d.Run(context.Background(), liveTask(t, 120), RunOptions{Agent: "test-agent", Attempts: 1, JobsDir: t.TempDir(), JobName: "mismatch",
		Live: lv, Counter: c, agentCmd: []string{"bash", "-c", liveAgent}})
	if err != nil || len(ts) != 1 || ts[0].Exception != ErrGateRefused || calls.Load() != 0 {
		t.Fatalf("%+v %v calls %d", ts, err, calls.Load())
	}
	noQ4Leftovers(t, d)
}
