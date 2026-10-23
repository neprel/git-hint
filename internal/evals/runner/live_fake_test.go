package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// C7BJ-16 regressions on a synthetic engine API (no engine, no model): the persisted trial
// result holds the ledger as read after the attempt's teardown.

type liveFakeOpts struct {
	lateDuringCapture bool          // a request reaches the proxy while the tree is captured
	evilTree          bool          // the captured tree has an escaping symlink
	blockWait         bool          // the agent never ends (wait blocks until the client gives up)
	failNetworkDelete bool          // removing a network fails
	cancelAfter       time.Duration // cancel the attempt's context
	digest            bool          // the attempt has an execution digest (agent_run is recorded)
	requestInWait     bool          // the one request arrives while the agent runs, not before the trial
	refuseProbe       bool          // the gate probe fails: nothing is launched
}

func liveFake(t *testing.T, o liveFakeOpts) (trialResult, *Counter) {
	t.Helper()
	d, s := fakeQ4Docker(t, false)
	lv, _ := fakeModel(t)
	c := lv.NewTrial(10)
	request := func() {
		r := httptest.NewRequest("POST", "http://proxy/t/"+c.token+"/chat/completions", strings.NewReader(`{"model":"m"}`))
		lv.srv.Handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	if !o.requestInWait {
		request() // one complete request during the run
	}
	waited := false
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755})
	if o.evilTree {
		tw.WriteHeader(&tar.Header{Name: "app/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	}
	tw.Close()
	injected := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/archive"):
			if r.Method == "PUT" {
				w.WriteHeader(200)
				return
			}
			if o.lateDuringCapture && !injected { // during the agent tree capture only
				injected = true
				request()
			}
			w.Header().Set("X-Docker-Container-Path-Stat", "eyJuYW1lIjoiYXBwIiwic2l6ZSI6MCwibW9kZSI6MjE0NzQ4NDE0MSwibXRpbWUiOiIyMDI2LTEwLTIwVDAwOjAwOjAwWiIsImxpbmtUYXJnZXQiOiIifQ==")
			w.Write(tree.Bytes())
			return
		case o.requestInWait && !waited && strings.HasSuffix(p, "/wait"): // the agent's wait
			waited = true
			request()
			s.serve(w, r)
			return
		case o.blockWait && strings.HasSuffix(p, "/wait"):
			<-r.Context().Done()
			return
		case o.failNetworkDelete && r.Method == "DELETE" && strings.Contains(p, "/networks/"):
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"message": "synthetic"})
			return
		}
		s.serve(w, r)
	}))
	t.Cleanup(api.Close)
	cli, err := client.New(client.WithHost(api.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	d.cli = cli
	key, err := d.probeKey(context.Background(), fakeGateID)
	if err != nil {
		t.Fatal(err)
	}
	d.probes = map[string]GateProbe{key: {OK: true, Key: key, Image: fakeGateID}}
	if o.refuseProbe {
		d.probes, d.probeFn = nil, func(context.Context) GateProbe { return GateProbe{Reason: "synthetic probe failure"} }
	}
	digest := ""
	if o.digest {
		digest = "sha256:" + strings.Repeat("c", 64)
	}
	task := t.TempDir()
	os.MkdirAll(filepath.Join(task, "environment", "repo"), 0o755)
	dir := t.TempDir()
	ctx := context.Background()
	if o.cancelAfter > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.cancelAfter)
		defer cancel()
	}
	lim := taskLimits{agent: time.Minute, verifier: time.Minute, cpus: 1, memoryMB: 128}
	if o.blockWait && o.cancelAfter == 0 {
		lim.agent = time.Second
	}
	d.trial(ctx, task, dir, "t__1", RunOptions{Agent: "test-agent", Attempts: 1, Live: lv, Counter: c, agentCmd: []string{"test"}, testDigest: digest},
		lim, images{agent: fakeGateID, verifier: fakeGateID}, nil, nil)
	var r trialResult
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil || json.Unmarshal(b, &r) != nil {
		t.Fatalf("result %s %v", b, err)
	}
	return r, c
}

func persisted(t *testing.T, r trialResult) LedgerReport {
	t.Helper()
	var l LedgerReport
	b, _ := json.Marshal(r.Container["ledger"])
	if json.Unmarshal(b, &l) != nil || l.Counts == nil {
		t.Fatalf("no ledger in the result: %v", r.Container)
	}
	return l
}

func TestLiveLedgerIncludesLateDuringCapture(t *testing.T) {
	r, c := liveFake(t, liveFakeOpts{lateDuringCapture: true})
	l, cur := persisted(t, r), c.Report()
	if l.State != cur.State || l.Revision != cur.Revision || l.Counts["late"] != 1 || l.State != "incomplete" || r.Container["ledger_final"] != true {
		t.Fatalf("persisted %+v, current %+v", l, cur)
	}
}

func TestLiveLedgerOnErrorPaths(t *testing.T) {
	for name, o := range map[string]liveFakeOpts{
		"freeze error": {evilTree: true},
		"timeout":      {blockWait: true},
		"cancel":       {blockWait: true, cancelAfter: 2 * time.Second},
	} {
		r, c := liveFake(t, o)
		l, cur := persisted(t, r), c.Report()
		if r.ExceptionInfo == nil || l.State != cur.State || l.Revision != cur.Revision || l.Counts[EntryComplete] != 1 || r.Container["ledger_final"] != true {
			t.Errorf("%s: exception %v, persisted %+v, current %+v, final %v", name, r.ExceptionInfo, l, cur, r.Container["ledger_final"])
		}
	}
}

// A teardown that does not complete is recorded: the ledger is not claimed final.
func TestLiveLedgerTeardownFailure(t *testing.T) {
	r, _ := liveFake(t, liveFakeOpts{failNetworkDelete: true})
	errs, _ := r.Container["teardown_errors"].([]any)
	if r.Container["ledger_final"] != false || len(errs) == 0 {
		t.Fatalf("final %v errors %v", r.Container["ledger_final"], errs)
	}
}

// The live phase records the agent's enclosure around the engine calls: a run whose request
// arrives while the agent runs gives an exited record that passes the public rule; a refused
// gate gives refused_before_launch without offsets; without a digest nothing is recorded.
func TestLiveAgentRunWiring(t *testing.T) {
	r, c := liveFake(t, liveFakeOpts{digest: true, requestInWait: true})
	l := persisted(t, r)
	if a := l.AgentRun; a == nil || a.Outcome != AgentExited || a.EnclosureStartMs == nil || a.EnclosureEndMs == nil || len(l.Entries) != 1 {
		t.Fatalf("agent run %+v entries %d", a, len(l.Entries))
	}
	if p := checked(t, &l); p != "" || *l.AgentRun.EnclosureEndMs > *l.SealedMs || c.Report().AgentRun.Outcome != AgentExited {
		t.Fatalf("the produced record fails the public rule: %s %+v", p, l.AgentRun)
	}
	r, _ = liveFake(t, liveFakeOpts{digest: true, requestInWait: true, refuseProbe: true})
	l = persisted(t, r)
	if a := l.AgentRun; r.ExceptionInfo == nil || a == nil || a.Outcome != AgentRefusedBeforeLaunch || a.EnclosureStartMs != nil || checked(t, &l) != "" {
		t.Fatalf("refused: exception %v agent run %+v", r.ExceptionInfo, a)
	}
	r, _ = liveFake(t, liveFakeOpts{})
	if l := persisted(t, r); l.AgentRun != nil {
		t.Fatalf("a record without a digest: %+v", l.AgentRun)
	}
}
