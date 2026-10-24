package pipeline

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// Limits of the live context channel check (one session per platform): model requests
// including retries (counted at the proxy, refused beyond), failed model answers before the
// gate refuses every further request (one retry), and wall time (the agent is killed).
const (
	ChannelRequests = 8
	ChannelFailures = 2 // the first failure may be retried once; the second stops the session
	ChannelTime     = 5 * time.Minute
)

// openCode26Pins are the SHA-512 values of the OpenCode 2.0.26 npm tarballs of the channel
// check (2.0.26 gives MCP tools natively with codemode off; 2.0.22 only through Code Mode).
var openCode26Pins = map[string]string{
	"darwin-arm64": "5e496575e3ba796806f2f90f34b71d9413c49b563efedca1117199bd76e4f2ecada4f3fb484555d91f6867df0d31248f1fd90100effe5e8e2c29fbc3e821c0b3",
	"linux-arm64":  "206df5e88f3056a9dda21cc502858d787f3778c03cb0a40546d31eba4d967e0e0353585a092833009cf7fe06f2b63c21e9b5dd591ed5dc5092adf8e8ada28383",
}

// channelCanary is the frozen source name of the check; it must never reach the spool or an
// export.
const channelCanary = "HintChannelCanaryRate"

// failGate forwards model requests and refuses all of them after ChannelFailures failed
// answers (non-2xx), so that at most one retry happens within the request limit.
type failGate struct {
	rp       *httputil.ReverseProxy
	failures atomic.Int64
	refused  atomic.Int64
	mu       sync.Mutex
	paths    []string
	statuses []int // status of every forwarded model answer
}

func (g *failGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.Method+" "+r.URL.Path[strings.LastIndexByte(r.URL.Path, '/'):])
	g.mu.Unlock()
	if g.failures.Load() >= ChannelFailures {
		g.refused.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	g.rp.ServeHTTP(w, r)
}

// TestLiveContextChannel is the bounded check of the context channel with a real agent: one
// OpenCode 2.0.26 session (pinned npm tarball) with the real installed HINT plugin and a real
// `git-hint mcp` process, on the owner's LAN model through HINT's counting proxy, with
// loopback-only egress. The agent is asked to call githint_hint_context once. The test then
// checks the real hook records, the two stages of one call, the policy identity and a v0.2
// export without canaries. Set HINT_LIVE_CHANNEL=1, HINT_OPENCODE_TGZ, HINT_LIVE_MODEL_URL,
// HINT_LIVE_MODEL, HINT_LOCAL_MODEL_KEY and HINT_LIVE_OUT. No hook event is synthesized.
func TestLiveContextChannel(t *testing.T) {
	if os.Getenv("HINT_LIVE_CHANNEL") == "" {
		t.Skip("set HINT_LIVE_CHANNEL=1 (live, bounded)")
	}
	out := os.Getenv("HINT_LIVE_OUT")
	os.MkdirAll(out, 0o700)
	save := func(name string, data []byte) { os.WriteFile(filepath.Join(out, name), data, 0o600) }
	summary := map[string]any{"host": runtime.GOOS + "-" + runtime.GOARCH,
		"limits": map[string]any{"model_requests_including_retries": ChannelRequests, "failed_answers_before_stop": ChannelFailures, "wall_seconds": ChannelTime.Seconds(), "attempts": 1}}
	checks := map[string]any{}
	summary["checks"] = checks
	defer func() {
		b, _ := json.MarshalIndent(summary, "", "  ")
		save("summary.json", b)
		t.Logf("summary: %s", b)
	}()

	target := runtime.GOOS + "-" + runtime.GOARCH
	raw, err := os.ReadFile(os.Getenv("HINT_OPENCODE_TGZ"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(raw)
	if hex.EncodeToString(sum[:]) != openCode26Pins[target] {
		t.Fatalf("the OpenCode tarball does not match the 2.0.26 pin for %s", target)
	}
	tools := t.TempDir()
	if o, err := exec.Command("tar", "-xzf", os.Getenv("HINT_OPENCODE_TGZ"), "-C", tools, "package/bin/opencode").CombinedOutput(); err != nil {
		t.Fatal(string(o))
	}
	bin := filepath.Join(tools, "package", "bin", "opencode")
	summary["agent"] = "opencode 2.0.26 (" + target + ", npm tarball, sha512 pinned)"
	hintBin := filepath.Join(tools, "git-hint")
	if o, err := exec.Command("go", "build", "-o", hintBin, "../../../cmd/git-hint").CombinedOutput(); err != nil {
		t.Fatal(string(o))
	}
	if rev, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		summary["git_hint_commit"] = strings.TrimSpace(string(rev))
	}

	// the synthetic project: frozen canary source, HINT initialized with a known context
	// policy, the real plugin installed, the git-hint MCP server configured for OpenCode
	keys := t.TempDir()
	t.Setenv("HINT_KEY_DIR", keys)
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "go.mod", "module example.invalid/channel\n\ngo 1.22\n")
	testkit.WriteFile(t, dir, "pay/rate.go", "package pay\n\n// "+channelCanary+" gives a rate. HINTCANARY_CHANNEL_COMMENT\nfunc "+channelCanary+"(a, b int) int { return a + b }\n")
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "base")
	hintRun := func(args ...string) {
		c := exec.Command(hintBin, args...)
		c.Dir = dir
		if o, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git-hint %v: %s", args, o)
		}
	}
	hintRun("init", "--context-limits", "depth=1,items=10")
	hintRun("collect", "--full")
	hintRun("adapter", "install", "opencode")
	mcpCfg, _ := json.Marshal(map[string]any{"mcp": map[string]any{"servers": map[string]any{"githint": map[string]any{"type": "local", "codemode": false, "command": []string{hintBin, "mcp"}}}}})
	testkit.WriteFile(t, dir, "opencode.json", string(mcpCfg))
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantPolicy := store.ContextPolicyRef(s.Key(), store.ContextLimits{Depth: 1, MaxEntities: 10, MaxBytes: store.ContextRanges["max_bytes"][0], MaxMillis: store.ContextRanges["max_ms"][0]})

	// the model endpoint must accept a TCP connection before the session starts (no model
	// request); an unreachable endpoint ends the check without using the session attempt
	mu, err := url.Parse(os.Getenv("HINT_LIVE_MODEL_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", mu.Host, 5*time.Second); err != nil {
		summary["result"] = "not started: the model endpoint is not reachable"
		t.Fatalf("the model endpoint is not reachable: %T", err)
	} else {
		c.Close()
	}

	// the counting proxy, the failure gate and the host log
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: os.Getenv("HINT_LIVE_MODEL_URL"), Model: os.Getenv("HINT_LIVE_MODEL"), KeyEnv: "HINT_LOCAL_MODEL_KEY", AgentVersion: "2.0.26"})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	lv.ContainerHost = "127.0.0.1"
	counter := lv.NewTrial(ChannelRequests)
	up, _ := url.Parse(lv.BaseURL(counter))
	gate := &failGate{}
	gate.rp = &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(up)
		r.Out.URL.Path, r.Out.URL.RawPath = up.Path+strings.TrimPrefix(r.In.URL.Path, "/gate"), ""
	}, ModifyResponse: func(resp *http.Response) error {
		gate.mu.Lock()
		gate.statuses = append(gate.statuses, resp.StatusCode)
		gate.mu.Unlock()
		if resp.StatusCode/100 != 2 {
			gate.failures.Add(1)
		}
		return nil
	}}
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(gl, gate)
	defer gl.Close()
	logger := &hostLog{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, logger)
	defer ln.Close()

	home := t.TempDir()
	xdg := func(n string) string { d := filepath.Join(home, n); os.MkdirAll(d, 0o700); return d }
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "XDG_DATA_HOME=" + xdg("data"), "XDG_CONFIG_HOME=" + xdg("config"),
		"XDG_STATE_HOME=" + xdg("state"), "XDG_CACHE_HOME=" + xdg("cache"), "OPENCODE_DISABLE_MODELS_FETCH=true", "OPENCODE_DISABLE_AUTOUPDATE=true",
		"HTTP_PROXY=http://" + ln.Addr().String(), "HTTPS_PROXY=http://" + ln.Addr().String(), "http_proxy=http://" + ln.Addr().String(),
		"https_proxy=http://" + ln.Addr().String(), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1", "HINT_BIN=" + hintBin, "HINT_KEY_DIR=" + keys}
	model := os.Getenv("HINT_LIVE_MODEL")
	cfg := fmt.Sprintf(`{"provider":{"hint":{"npm":"@ai-sdk/openai-compatible","name":"hint","options":{"baseURL":%q,"apiKey":"hint-proxy"},"models":{%q:{"name":%q}}}},"autoupdate":false,"share":"disabled"}`,
		"http://"+gl.Addr().String()+"/gate", model, model)
	os.MkdirAll(filepath.Join(home, "config", "opencode"), 0o700)
	os.WriteFile(filepath.Join(home, "config", "opencode", "opencode.json"), []byte(cfg), 0o600)

	// OpenCode connects MCP servers asynchronously: the first model request can lack the HINT
	// tools, so the agent lists the files first (one native call)
	prompt := "First list the files with the glob tool, pattern pay/*.go. Then call the tool githint_hint_context exactly once with the argument seed set to " +
		channelCanary + ". Then answer with the first line of the source in its result. Do not call any other tool."
	args := []string{"run", "--standalone", "--format", "json", "--auto", "-m", "hint/" + model, "--", prompt}
	ctx, cancel := context.WithTimeout(context.Background(), ChannelTime)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		profile := `(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))`
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", profile, bin}, args...)...)
		summary["egress"] = "sandbox-exec: network-outbound only to localhost (OpenCode, the plugin's child and the MCP server inherit it)"
	case "linux":
		uid := os.Getenv("HINT_SESSION_UID")
		cmd = exec.CommandContext(ctx, "setpriv", append([]string{"--reuid=" + uid, "--regid=" + uid, "--clear-groups", bin}, args...)...)
		summary["egress"] = "iptables owner match: uid " + uid + " only on lo"
		for _, d := range []string{dir, home, tools, keys} {
			if o, err := exec.Command("chown", "-R", uid+":"+uid, d).CombinedOutput(); err != nil {
				t.Fatalf("chown %s: %s", d, o)
			}
			os.Chmod(filepath.Dir(d), 0o711)
		}
	}
	cmd.Dir, cmd.Env = dir, env
	start := time.Now()
	events, runErr := cmd.CombinedOutput()
	wall := time.Since(start)
	save("opencode-events.jsonl", events)
	logger.mu.Lock()
	hosts := append([]string{}, logger.hosts...)
	logger.mu.Unlock()
	gate.mu.Lock()
	paths := append([]string{}, gate.paths...)
	statuses := append([]int{}, gate.statuses...)
	gate.mu.Unlock()
	summary["session"] = map[string]any{"exit_error": fmt.Sprint(runErr), "wall_seconds": wall.Round(time.Second).Seconds(), "timed_out": ctx.Err() != nil,
		"model_requests": counter.Requests(), "requests_refused_by_limit": counter.LimitRefusals(), "limit_hit": counter.LimitHit(),
		"failed_model_answers": gate.failures.Load(), "requests_refused_after_failures": gate.refused.Load(), "model_answer_statuses": statuses}
	summary["endpoints"] = map[string]any{"model_gate_requests (method, last path segment)": paths, "proxied_host_attempts": hosts,
		"note": "the egress rule allows loopback only; traffic that bypasses both the proxy and the gate is not observed"}
	summary["tokens"] = tokensOf(events)

	// what the hook delivered and the adapter recorded (no synthesized event)
	spool := readJSONLines(t, filepath.Join(dir, ".hint", "state", "activity", "spool.jsonl"))
	var ctxObs []map[string]any
	for _, o := range spool {
		if _, ok := o["context"]; ok {
			ctxObs = append(ctxObs, o)
		}
	}
	stats, _ := os.ReadFile(filepath.Join(dir, ".hint", "state", "activity", "adapter-stats.json"))
	checks["hook_records"] = len(spool)
	checks["adapter_stats"] = json.RawMessage(stats)
	checks["context_observations"] = len(ctxObs)
	if len(ctxObs) != 2 {
		summary["result"] = "channel failure: no two context stages recorded"
		t.Fatalf("context observations: %d (spool %d records)", len(ctxObs), len(spool))
	}
	stages := map[string]map[string]any{}
	calls := map[any]bool{}
	for _, o := range ctxObs {
		c := o["context"].(map[string]any)
		stages[c["stage"].(string)] = c
		calls[o["correlation"].(map[string]any)["tool_call_id"]] = true
	}
	d, r := stages["delivered_context"], stages["raw_tool_output"]
	checks["one_call_two_stages"] = len(calls) == 1 && d != nil && r != nil
	checks["delivered_bytes"], checks["raw_bytes"] = d["bytes"], r["bytes"]
	checks["policy_identity_matches"] = d["policy_state"] == "enabled" && d["policy_ref"] == wantPolicy
	checks["result"] = d["result"]
	// the original call ID of OpenCode, pseudonymized as the adapter does
	sources := map[string]map[string]any{}
	b, _ := os.ReadFile(filepath.Join(dir, ".hint", "state", "activity", "sources.json"))
	json.Unmarshal(b, &sources)
	callID, sessionID := toolCall(events, "githint_hint_context")
	matched := false
	for sid := range sources {
		if hint.DeriveID(s.Key(), "call", sid, sessionID, callID) == ctxObs[0]["correlation"].(map[string]any)["tool_call_id"] {
			matched = true
		}
	}
	checks["original_call_id_matches"] = matched
	// the agent received useful text: source and refs in the tool result OpenCode recorded
	checks["agent_text_has_source"] = strings.Contains(string(events), "func "+channelCanary+"(a, b int) int")
	checks["agent_text_has_refs"] = strings.Contains(string(events), "hc1.")

	// a v0.2 export without canaries
	exp := t.TempDir()
	hintRun("export", "--out", exp)
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(exp, "*.hint.json"))
	v02 := len(files) > 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all.Write(b)
		var doc map[string]any
		json.Unmarshal(b, &doc)
		v02 = v02 && doc["hint_version"] == "0.2.0"
	}
	checks["export_v02"] = v02
	leaks := []string{}
	for _, c := range []string{channelCanary, "HINTCANARY_", "pay/rate.go", "rate.go", "first line of the source", "func ", "hc1."} {
		if strings.Contains(all.String(), c) {
			leaks = append(leaks, c)
		}
	}
	checks["export_canary_leaks"] = leaks
	ok := checks["one_call_two_stages"] == true && checks["policy_identity_matches"] == true && matched && v02 && len(leaks) == 0 &&
		checks["agent_text_has_source"] == true && checks["agent_text_has_refs"] == true
	if !ok {
		summary["result"] = "channel failure"
		t.Fatalf("checks: %v", checks)
	}
	summary["result"] = "channel verified"
}

func readJSONLines(t *testing.T, p string) []map[string]any {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// toolCall finds the call ID and session ID of the first call of a tool in OpenCode's JSON
// event stream.
func toolCall(events []byte, tool string) (callID, sessionID string) {
	sc := bufio.NewScanner(strings.NewReader(string(events)))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		part, _ := e["part"].(map[string]any)
		if part != nil && part["tool"] == tool {
			c, _ := part["id"].(string) // the tool call ID (the hook's event ID)
			if c == "" {
				c, _ = part["callID"].(string)
			}
			sid, _ := part["sessionID"].(string)
			if sid == "" {
				sid, _ = e["sessionID"].(string)
			}
			return c, sid
		}
	}
	return "", ""
}

// tokensOf sums the token counts that OpenCode reports in its step events; unknown when it
// reports none.
func tokensOf(events []byte) any {
	in, outT, seen := 0.0, 0.0, false
	sc := bufio.NewScanner(strings.NewReader(string(events)))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		part, _ := e["part"].(map[string]any)
		tk, _ := part["tokens"].(map[string]any)
		if tk == nil {
			continue
		}
		if v, ok := tk["input"].(float64); ok {
			in += v
			seen = true
		}
		if v, ok := tk["output"].(float64); ok {
			outT += v
		}
	}
	if !seen {
		return "unknown"
	}
	return map[string]any{"input": in, "output": outT, "source": "OpenCode step events"}
}
