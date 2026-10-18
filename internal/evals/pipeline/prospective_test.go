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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// openCodePins are the SHA-512 values of the OpenCode 2.0.22 npm tarballs that the
// prospective check accepts (from the npm registry's dist.integrity).
var openCodePins = map[string]string{
	"darwin-arm64": "34d8355420935927cb94d2a945be1103a204ec7ebb6412c16267c85a33f70b147d34fb5d44e40508bf25a4f7fe4f3d908dee016f6eec40e2d66a7632353ffd75",
	"linux-arm64":  "b7fcaebbd0ea77f338e1d147f796e986eee290ee85013310b9593ff754019c2333b0ff3356b5d4ef30310f5c152e9618503a88a271268bcd17113945ab1edc87",
}

// prospectiveRequest is the frozen user request of the check.
const prospectiveRequest = "Total ignores the discount. Fix Total so it subtracts the discount from the price. " +
	"Add a Go test named TestDiscount in discount_test.go that checks Total(10, 3) == 7, then run go test ./... and make sure it passes."

// hostLog is a forward proxy that records every requested host and refuses it.
type hostLog struct {
	mu    sync.Mutex
	hosts []string
}

func (h *hostLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if r.Method != http.MethodConnect && r.URL.Host != "" {
		host = r.URL.Host
	}
	h.mu.Lock()
	h.hosts = append(h.hosts, r.Method+" "+host)
	h.mu.Unlock()
	http.Error(w, "refused by the HINT check", http.StatusForbidden)
}

// openCodeCmd runs the pinned OpenCode binary for the pipeline's session import with the
// session's isolated environment.
type openCodeCmd struct {
	bin string
	env []string
}

func (o openCodeCmd) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name != "opencode" {
		return nil, fmt.Errorf("%s is not available in this check", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, o.bin, args...)
	c.Dir, c.Env = dir, o.env
	return c.Output()
}

// TestLiveProspectiveCapture is the E15 prospective check (protocol:
// E15-prospective-protocol, private): one real OpenCode 2.0.22 session with the owner's
// LAN model under a 40-request proxy limit, 900 s and loopback-only egress; then the HINT
// pipeline imports the session and snapshots, proposes the episode and packages the eval;
// the test acts as the user for the requirement mapping (no model in the policy); real
// Harbor qualifies. Set HINT_LIVE_PROSPECTIVE=1, HINT_OPENCODE_TGZ (the npm tarball),
// HINT_LIVE_MODEL_URL, HINT_LIVE_MODEL, HINT_LOCAL_MODEL_KEY, HINT_HARBOR, HINT_LIVE_OUT.
func TestLiveProspectiveCapture(t *testing.T) {
	if os.Getenv("HINT_LIVE_PROSPECTIVE") == "" {
		t.Skip("set HINT_LIVE_PROSPECTIVE=1 (live, bounded)")
	}
	out := os.Getenv("HINT_LIVE_OUT")
	os.MkdirAll(out, 0o700)
	save := func(name string, data []byte) { os.WriteFile(filepath.Join(out, name), data, 0o600) }
	summary := map[string]any{"host": runtime.GOOS + "-" + runtime.GOARCH}
	defer func() {
		b, _ := json.MarshalIndent(summary, "", "  ")
		save("summary.json", b)
		t.Logf("summary: %s", b)
	}()

	// the pinned binary
	target := runtime.GOOS + "-" + runtime.GOARCH
	tgz := os.Getenv("HINT_OPENCODE_TGZ")
	raw, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(raw)
	if hex.EncodeToString(sum[:]) != openCodePins[target] {
		t.Fatalf("the OpenCode tarball does not match the 2.0.22 pin for %s", target)
	}
	tools := t.TempDir()
	if out, err := exec.Command("tar", "-xzf", tgz, "-C", tools, "package/bin/opencode").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	bin := filepath.Join(tools, "package", "bin", "opencode")
	summary["agent"] = "opencode 2.0.22 (" + target + ", sha512 pinned)"

	// the project: the frozen synthetic task, HINT initialized, adapter installed, evals on
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	for p, c := range map[string]string{"go.mod": evaltest.GoMod, "cart.go": evaltest.GoBug, "cart_test.go": evaltest.GoOld} {
		testkit.WriteFile(t, dir, p, c)
	}
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "base")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "harbor"} // no model: no direct model call
	s.Cfg.Evals = &p
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	// the OpenCode adapter, installed by the real CLI; the plugin starts HINT_BIN
	hintBin := filepath.Join(tools, "git-hint")
	if out, err := exec.Command("go", "build", "-o", hintBin, "../../../cmd/git-hint").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	inst := exec.Command(hintBin, "adapter", "install", "opencode")
	inst.Dir = dir
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}

	// the counting proxy (40 requests, retries included) and the host log
	lv, err := runner.StartLive(runner.LiveConfig{BaseURL: os.Getenv("HINT_LIVE_MODEL_URL"), Model: os.Getenv("HINT_LIVE_MODEL"), KeyEnv: "HINT_LOCAL_MODEL_KEY", AgentVersion: "2.0.22"})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	lv.ContainerHost = "127.0.0.1"
	counter := lv.NewTrial(40)
	logger := &hostLog{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, logger)
	defer ln.Close()

	// the isolated OpenCode environment
	home := t.TempDir()
	xdg := func(n string) string { d := filepath.Join(home, n); os.MkdirAll(d, 0o700); return d }
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "XDG_DATA_HOME=" + xdg("data"), "XDG_CONFIG_HOME=" + xdg("config"),
		"XDG_STATE_HOME=" + xdg("state"), "XDG_CACHE_HOME=" + xdg("cache"), "OPENCODE_DISABLE_MODELS_FETCH=true", "OPENCODE_DISABLE_AUTOUPDATE=true",
		"HTTP_PROXY=http://" + ln.Addr().String(), "HTTPS_PROXY=http://" + ln.Addr().String(), "http_proxy=http://" + ln.Addr().String(),
		"https_proxy=http://" + ln.Addr().String(), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1", "HINT_BIN=" + hintBin, "HINT_KEY_DIR=" + os.Getenv("HINT_KEY_DIR")}
	model := os.Getenv("HINT_LIVE_MODEL")
	cfg := fmt.Sprintf(`{"provider":{"hint":{"npm":"@ai-sdk/openai-compatible","name":"hint","options":{"baseURL":%q,"apiKey":"hint-proxy"},"models":{%q:{"name":%q}}}},"autoupdate":false,"share":"disabled"}`,
		lv.BaseURL(counter), model, model)
	os.MkdirAll(filepath.Join(home, "config", "opencode"), 0o700)
	os.WriteFile(filepath.Join(home, "config", "opencode", "opencode.json"), []byte(cfg), 0o600)

	// the session: one attempt, 900 s, loopback-only egress
	args := []string{"run", "--standalone", "--format", "json", "--auto", "-m", "hint/" + model, "--", prospectiveRequest}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		profile := `(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))`
		cmd = exec.CommandContext(ctx, "sandbox-exec", append([]string{"-p", profile, bin}, args...)...)
		summary["egress"] = "sandbox-exec: network-outbound only to localhost"
	case "linux":
		// the control host restricts the uid of the session to loopback (iptables owner match)
		cmd = exec.CommandContext(ctx, "setpriv", append([]string{"--reuid=" + os.Getenv("HINT_SESSION_UID"), "--regid=" + os.Getenv("HINT_SESSION_UID"), "--clear-groups", bin}, args...)...)
		summary["egress"] = "iptables owner match: uid " + os.Getenv("HINT_SESSION_UID") + " only on lo"
	}
	if runtime.GOOS == "linux" {
		// the session user owns the project and its home
		for _, d := range []string{dir, home, tools} {
			exec.Command("chown", "-R", os.Getenv("HINT_SESSION_UID")+":"+os.Getenv("HINT_SESSION_UID"), d).Run()
		}
	}
	cmd.Dir, cmd.Env = dir, env
	start := time.Now()
	events, runErr := cmd.CombinedOutput()
	wall := time.Since(start)
	save("opencode-events.jsonl", events)
	summary["session"] = map[string]any{"exit_error": fmt.Sprint(runErr), "wall_seconds": wall.Round(time.Second).Seconds(), "timed_out": ctx.Err() != nil,
		"model_requests": counter.Requests(), "proxy_refusals": counter.Retries(), "limit_hit": counter.LimitHit()}
	logger.mu.Lock()
	hosts := append([]string{}, logger.hosts...)
	logger.mu.Unlock()
	summary["proxied_host_attempts"] = hosts
	var fetchLines []string
	filepath.Walk(filepath.Join(home, "data", "opencode", "log"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		f, _ := os.Open(p)
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if l := sc.Text(); strings.Contains(l, "http") && (strings.Contains(l, "ERROR") || strings.Contains(strings.ToLower(l), "fetch")) {
				fetchLines = append(fetchLines, l)
			}
		}
		return nil
	})
	summary["opencode_log_fetch_lines"] = fetchLines
	if counter.Requests() == 0 {
		t.Fatalf("the session made no model request: %v", runErr)
	}

	// the HINT pipeline, as after collect (the episode closes after the idle time)
	idle := time.Now().Add(IdleCloseForCheck)
	r := Run(context.Background(), s, Env{Home: home, DataHome: filepath.Join(home, "data"), Cmd: openCodeCmd{bin: bin, env: env}, Now: func() time.Time { return idle }})
	l := library.Open(s, 0)
	evalsAfter, _ := l.Manifests("eval")
	episodesAfter, _ := l.Manifests("episode")
	summary["pipeline"] = map[string]any{"imports": r.Imports, "episodes": r.Episodes, "packages": r.Packages, "evals": evalsAfter, "episode_records": episodesAfter}
	for _, n := range episodesAfter {
		var ep map[string]any
		l.Manifest("episode", n, &ep)
		b, _ := json.MarshalIndent(map[string]any{"outcomes": ep["outcomes"], "reasons": ep["reasons"], "family": ep["family"], "repositories": ep["repositories"], "attribution": ep["attribution"]}, "", "  ")
		save("episode-"+n+".json", b)
	}
	if len(evalsAfter) != 1 {
		summary["result"] = "no eval packaged"
		t.Logf("no eval packaged; refusals %v", r.Packages)
		return
	}

	// the user confirms the requirement mapping (the test acts as the user)
	f, err := MapDraft(l, evalsAfter[0])
	if err != nil {
		t.Fatal(err)
	}
	c1 := build.C1Tests(mustRec(t, l, evalsAfter[0]))
	summary["hidden_tests"] = c1
	f.Requirements = []build.Link{{Source: "R1", Quote: "Fix Total so it subtracts the discount from the price", Tests: c1}}
	f.Context = []string{"Total ignores the discount.", "Add a Go test named TestDiscount in discount_test.go that checks Total(10, 3) == 7, then run go test ./... and make sure it passes."}
	b, _ := json.Marshal(f)
	next, unmapped, err := ApplyMapFile(context.Background(), l, f.Eval, b, "", idle)
	summary["user_mapping"] = map[string]any{"by": "the test, acting as the user", "error": fmt.Sprint(err), "version": next, "unmapped": unmapped}
	if err != nil {
		summary["result"] = "user mapping refused"
		return
	}

	// qualification through real Harbor
	h, err := runner.FindHarbor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q := Qualify(context.Background(), s, h, idle)
	names, _ := l.Manifests("eval")
	final := mustRec(t, l, names[len(names)-1])
	checks := map[string]any{}
	for _, c := range final["qualification"].(map[string]any)["checks"].([]any) {
		checks[c.(map[string]any)["name"].(string)] = c.(map[string]any)["status"]
	}
	summary["qualification"] = map[string]any{"report": q, "state": final["state"], "reasons": final["reasons"], "checks": checks}
	summary["result"] = final["state"]
	sort.Strings(names)
}

// IdleCloseForCheck is how far past the session the pipeline looks (the episode closes
// after the idle time).
var IdleCloseForCheck = 31 * time.Minute

func mustRec(t *testing.T, l *library.Library, name string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := l.Manifest("eval", name, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}
