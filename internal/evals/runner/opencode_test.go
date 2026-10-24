package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/client"
)

type tgzEntry struct {
	h    tar.Header
	body string
}

func makeTgz(t *testing.T, entries ...tgzEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := e.h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func dirE(n string) tgzEntry { return tgzEntry{h: tar.Header{Name: n, Typeflag: tar.TypeDir}} }
func fileE(n, body string) tgzEntry {
	return tgzEntry{h: tar.Header{Name: n, Typeflag: tar.TypeReg}, body: body}
}

func TestExtractOpenCode(t *testing.T) {
	good := makeTgz(t, dirE("package/"), fileE("package/package.json", "{}"), dirE("package/bin/"), fileE("package/bin/opencode", "BIN"))
	if b, err := extractOpenCode(good); err != nil || string(b) != "BIN" {
		t.Fatalf("good: %q %v", b, err)
	}
	for name, tgz := range map[string][]byte{
		"missing":     makeTgz(t, dirE("package/"), fileE("package/README", "x")),
		"duplicate":   makeTgz(t, fileE("package/bin/opencode", "A"), fileE("package/bin/opencode", "B")),
		"symlink":     makeTgz(t, tgzEntry{h: tar.Header{Name: "package/bin/opencode", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}}),
		"bin as link": makeTgz(t, tgzEntry{h: tar.Header{Name: "package/bin", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin"}}, fileE("package/bin/opencode", "A")),
		"traversal":   makeTgz(t, fileE("package/../../etc/x", "x"), fileE("package/bin/opencode", "A")),
		"absolute":    makeTgz(t, fileE("/package/bin/opencode", "A")),
		"dot path":    makeTgz(t, fileE("package/bin/./opencode", "A")),
		"directory":   makeTgz(t, dirE("package/bin/opencode/")),
		"not gzip":    []byte("plain"),
	} {
		if _, err := extractOpenCode(tgz); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	old := maxOpenCodeBinary
	maxOpenCodeBinary = 2
	defer func() { maxOpenCodeBinary = old }()
	if _, err := extractOpenCode(good); err == nil {
		t.Error("an oversized binary is accepted")
	}
}

// testPin registers a pin for a synthetic version and removes it after the test.
func testPin(t *testing.T, version, target string, tgz []byte) {
	t.Helper()
	OpenCodePins[version] = map[string]string{target: sha512Hex(tgz)}
	t.Cleanup(func() { delete(OpenCodePins, version) })
}

func TestOpenCodeArchiveSources(t *testing.T) {
	tgz := makeTgz(t, fileE("package/bin/opencode", "BIN"))
	testPin(t, "0.0.0-test", "linux-arm64", tgz)
	var hits atomic.Int64
	mode := "ok"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/@opencode/cli-linux-arm64/-/cli-linux-arm64-0.0.0-test.tgz" {
			w.WriteHeader(404)
			return
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "wrong":
			w.Write(append([]byte{}, tgz[:len(tgz)-1]...))
		default:
			w.Write(tgz)
		}
	}))
	defer reg.Close()
	ctx := context.Background()
	cache := t.TempDir()
	src := OpenCodeSource{Version: "0.0.0-test", CacheDir: cache, Registry: reg.URL}
	if b, how, err := src.archive(ctx, "linux-arm64"); err != nil || how != "registry" || !bytes.Equal(b, tgz) {
		t.Fatalf("download: %s %v", how, err)
	}
	reg.Close() // the cache needs no network
	if _, how, err := src.archive(ctx, "linux-arm64"); err != nil || how != "cache" {
		t.Fatalf("cache: %s %v", how, err)
	}
	// a corrupted cache entry is not used; the offline file of the same pin replaces it
	entry := filepath.Join(cache, sha512Hex(tgz)+".tgz")
	os.WriteFile(entry, []byte("corrupt"), 0o600)
	file := filepath.Join(t.TempDir(), "cli.tgz")
	os.WriteFile(file, tgz, 0o600)
	off := OpenCodeSource{Version: "0.0.0-test", CacheDir: cache, TgzFile: file}
	if _, how, err := off.archive(ctx, "linux-arm64"); err != nil || how != "file" {
		t.Fatalf("offline file: %s %v", how, err)
	}
	if b, _ := os.ReadFile(entry); !bytes.Equal(b, tgz) {
		t.Fatal("the cache entry was not replaced by the pinned archive")
	}
	os.WriteFile(file, []byte("other"), 0o600)
	if _, _, err := (OpenCodeSource{Version: "0.0.0-test", CacheDir: t.TempDir(), TgzFile: file}).archive(ctx, "linux-arm64"); err == nil {
		t.Error("an offline file that misses the pin is accepted")
	}
	for name, s := range map[string]OpenCodeSource{"unknown version": {Version: "9.9.9", CacheDir: t.TempDir()}} {
		if _, _, err := s.archive(ctx, "linux-arm64"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := src.archive(ctx, "linux-x64"); err == nil {
		t.Error("an unpinned target is accepted")
	}
	for _, m := range []string{"redirect", "wrong"} {
		reg2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == "redirect" {
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			}
			w.Write(tgz[:len(tgz)-1])
		}))
		if _, _, err := (OpenCodeSource{Version: "0.0.0-test", CacheDir: t.TempDir(), Registry: reg2.URL}).archive(ctx, "linux-arm64"); err == nil {
			t.Errorf("%s: accepted", m)
		}
		reg2.Close()
	}
	old := maxOpenCodeTgz
	maxOpenCodeTgz = 10
	defer func() { maxOpenCodeTgz = old }()
	reg3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) }))
	defer reg3.Close()
	if _, _, err := (OpenCodeSource{Version: "0.0.0-test", CacheDir: t.TempDir(), Registry: reg3.URL}).archive(ctx, "linux-arm64"); err == nil {
		t.Error("an oversized download is accepted")
	}
	_ = mode
}

func TestOpenCodeFilesAndArgv(t *testing.T) {
	c := &Counter{token: "tok"}
	b, err := openCodeFiles([]byte("BIN"), c, "m-1.5")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	seen := map[string]*tar.Header{}
	var cfg []byte
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		seen[h.Name] = h
		if strings.HasSuffix(h.Name, "opencode.json") {
			cfg = make([]byte, h.Size)
			tr.Read(cfg)
		}
	}
	bin := seen["opt/hint/bin/opencode"]
	if bin == nil || bin.Uid != 0 || bin.Mode != 0o755 || seen["tmp/.config/opencode/opencode.json"].Uid != 65534 {
		t.Fatalf("headers %v", seen)
	}
	var m map[string]any
	json.Unmarshal(cfg, &m)
	opts := m["provider"].(map[string]any)["hint"].(map[string]any)["options"].(map[string]any)
	if opts["baseURL"] != "http://gate:8080/t/tok" || opts["apiKey"] != "hint-proxy" || m["autoupdate"] != false || m["share"] != "disabled" {
		t.Fatalf("config %s", cfg)
	}
	if _, err := openCodeFiles(nil, c, "m; rm -rf /"); err == nil {
		t.Fatal("an unsafe model name is accepted")
	}
	argv := openCodeArgv("m", "do it; $(x)")
	if argv[0] != "/opt/hint/bin/opencode" || argv[len(argv)-1] != "do it; $(x)" || argv[len(argv)-2] != "--" {
		t.Fatalf("argv %v", argv)
	}
}

// The target probe on the engine: glibc needs positive evidence; musl is told apart; an image
// without ldd is refused.
func TestOpenCodeTargetOnTheEngine(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	ctx := context.Background()
	arch := map[string]string{"aarch64": "arm64", "x86_64": "x64"}
	info, _ := d.cli.Info(ctx, client.InfoOptions{})
	want := "linux-" + arch[info.Info.Architecture]
	ub, err := d.resolveImage(ctx, baseImage)
	if err != nil {
		t.Fatal(err)
	}
	if got, rec, err := d.openCodeTarget(ctx, ub.ID); err != nil || got != want || rec["libc"] != "glibc" {
		t.Fatalf("ubuntu: %s %v %v", got, rec, err)
	}
	if al, err := d.resolveImage(ctx, "alpine:latest"); err == nil {
		if got, _, err := d.openCodeTarget(ctx, al.ID); err != nil || got != want+"-musl" {
			t.Errorf("alpine: %s %v", got, err)
		}
	}
	gate, err := d.gateImage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.openCodeTarget(ctx, gate); err == nil {
		t.Error("an image without ldd gave a target")
	}
}

// fakeOpenCode stands in for the OpenCode binary: it reads its config, sends one request
// through the gate, and reports its argv, the binary's owner and mode, and whether it could
// change the binary.
const fakeOpenCode = `#!/bin/bash
cfg=$HOME/.config/opencode/opencode.json
url=$(grep -o '"baseURL":"[^"]*"' "$cfg" | cut -d'"' -f4)
path=${url#http://gate:8080}
body='{"model":"m","messages":[{"role":"user","content":"hi"}]}'
exec 3<>/dev/tcp/gate/8080
printf 'POST %s/chat/completions HTTP/1.0\r\nHost: gate\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' "$path" ${#body} "$body" >&3
cat <&3 > /app/answer.txt
printf '%s\n' "$@" > /app/argv.txt
stat -c '%U:%a' /opt/hint/bin/opencode > /app/binstat.txt
(echo x >> /opt/hint/bin/opencode) 2>/dev/null && echo WRITABLE >> /app/binstat.txt
grep -c HINTCANARY-REAL-KEY "$cfg" >> /app/binstat.txt
echo '{"type":"step_finish","part":{}}'
echo 'stderr line' >&2
exit ${FAKE_EXIT:-0}
`

func openCodeRun(t *testing.T, d *Docker, script string) (Trial, trialResult, string, *Counter) {
	t.Helper()
	info, _ := d.cli.Info(context.Background(), client.InfoOptions{})
	target := "linux-" + map[string]string{"aarch64": "arm64", "x86_64": "x64"}[info.Info.Architecture]
	tgz := makeTgz(t, dirE("package/"), dirE("package/bin/"), fileE("package/bin/opencode", script))
	testPin(t, "0.0.0-test", target, tgz)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) }))
	t.Cleanup(reg.Close)
	lv, _ := fakeModel(t)
	c := lv.NewTrial(10)
	task := liveTask(t, 120)
	os.WriteFile(filepath.Join(task, "instruction.md"), []byte("<!-- canary HINTCANARY -->\nUpper-case the input.\n"), 0o644)
	jobs := t.TempDir()
	ts, err := d.Run(context.Background(), task, RunOptions{Agent: "opencode", Attempts: 1, JobsDir: jobs, JobName: "oc", Live: lv, Counter: c,
		OpenCode: &OpenCodeSource{Version: "0.0.0-test", CacheDir: t.TempDir(), Registry: reg.URL}, ExpectTarget: target})
	if err != nil || len(ts) != 1 {
		t.Fatalf("%v %v", ts, err)
	}
	return ts[0], readResult(t, jobs, "oc"), filepath.Join(jobs, "oc", "oc__1"), c
}

func TestDockerOpenCodeInstalledAndRun(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	tr, res, dir, c := openCodeRun(t, d, fakeOpenCode)
	if tr.Outcome() != "passed" {
		t.Fatalf("%+v %v", tr, res.Container["error"])
	}
	files := map[string]string{}
	f, _ := os.Open(filepath.Join(dir, "agent", "final.tar"))
	defer f.Close()
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err != nil {
			break
		}
		b := make([]byte, h.Size)
		r.Read(b)
		files[h.Name] = string(b)
	}
	if files["app/binstat.txt"] != "root:755\n0\n" {
		t.Fatalf("binary owner/mode/writability or key in config: %q", files["app/binstat.txt"])
	}
	if !strings.HasPrefix(files["app/argv.txt"], "run\n--standalone\n--format\njson\n") || !strings.HasSuffix(files["app/argv.txt"], "--\nUpper-case the input.\n\n") {
		t.Fatalf("argv %q", files["app/argv.txt"])
	}
	ab, _ := res.Container["agent_binary"].(map[string]any)
	if ab["source"] != "registry" || !strings.HasPrefix(ab["binary_sha256"].(string), "sha256:") || ab["libc"] != "glibc" || ab["version"] != "0.0.0-test" ||
		!strings.HasPrefix(fmt.Sprint(ab["execution_digest"]), "sha256:") {
		t.Fatalf("agent binary %v", ab)
	}
	out, _ := res.Container["agent_output"].(map[string]any)
	so, _ := os.ReadFile(filepath.Join(dir, "agent", "opencode.txt"))
	se, _ := os.ReadFile(filepath.Join(dir, "agent", "opencode.stderr.txt"))
	if out["stdout"] != "complete" || out["stderr"] != "complete" || !strings.Contains(string(so), "step_finish") || string(se) != "stderr line\n" {
		t.Fatalf("output %v %q %q", out, so, se)
	}
	if rep := c.Report(); rep == nil || rep.Bypass != BypassEnforced || rep.Counts[EntryComplete] != 1 {
		t.Fatalf("ledger %+v", rep)
	}
	noQ4Leftovers(t, d)
}

// An agent that cannot work (a provider it cannot load, here a nonzero exit) never passes.
func TestDockerOpenCodeFailureNeverPasses(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	tr, _, _, _ := openCodeRun(t, d, "#!/bin/sh\necho 'provider @ai-sdk/openai-compatible not found' >&2\nexit 1\n")
	if tr.Outcome() == "passed" || tr.Exception != "NonZeroAgentExitCodeError" {
		t.Fatalf("%+v", tr)
	}
}

// The launch digest does not depend on the attempt's token and changes with the instruction,
// the model and the binary.
func TestOpenCodeExecutionDigest(t *testing.T) {
	p := map[string]any{"version": "2.0.26", "binary_sha256": "sha256:a", "target": "linux-arm64"}
	a := openCodeExecutionDigest("m", "do", p)
	if a != openCodeExecutionDigest("m", "do", p) {
		t.Fatal("not stable")
	}
	q := map[string]any{"version": "2.0.26", "binary_sha256": "sha256:b", "target": "linux-arm64"}
	for _, other := range []string{openCodeExecutionDigest("m", "other", p), openCodeExecutionDigest("n", "do", p), openCodeExecutionDigest("m", "do", q)} {
		if other == a {
			t.Fatal("a launch change kept the digest")
		}
	}
}

// (A) is the same for the same launch and target, and differs with the target, its libc, its
// pin, the model or the version; an unpinned target has none.
func TestDockerLaunchDigest(t *testing.T) {
	a, err := DockerLaunchDigest("2.0.26", "m", "linux-arm64")
	if err != nil || !strings.HasPrefix(a, "sha256:") {
		t.Fatal(a, err)
	}
	if b, _ := DockerLaunchDigest("2.0.26", "m", "linux-arm64"); b != a {
		t.Fatal("not stable")
	}
	for name, args := range map[string][3]string{
		"x64":     {"2.0.26", "m", "linux-x64"},
		"musl":    {"2.0.26", "m", "linux-arm64-musl"},
		"model":   {"2.0.26", "n", "linux-arm64"},
		"version": {"2.0.22", "m", "linux-arm64"},
	} {
		if d, err := DockerLaunchDigest(args[0], args[1], args[2]); err != nil || d == a {
			t.Errorf("%s: %s %v", name, d, err)
		}
	}
	old := OpenCodePins["2.0.26"]["linux-arm64"]
	OpenCodePins["2.0.26"]["linux-arm64"] = strings.Repeat("0", 128)
	changed, _ := DockerLaunchDigest("2.0.26", "m", "linux-arm64")
	OpenCodePins["2.0.26"]["linux-arm64"] = old
	if changed == a {
		t.Error("a pin change kept the configuration")
	}
	if _, err := DockerLaunchDigest("2.0.26", "m", "linux-riscv64"); err == nil {
		t.Error("an unpinned target has a configuration")
	}
}

// An attempt whose resolved target is not the run's frozen target is refused before any
// network or agent exists (no model call).
func TestDockerOpenCodeTargetMismatchRefused(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	tgz := makeTgz(t, fileE("package/bin/opencode", fakeOpenCode))
	testPin(t, "0.0.0-test", "linux-x64", tgz)
	OpenCodePins["0.0.0-test"]["linux-arm64"] = sha512Hex(tgz)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) }))
	defer reg.Close()
	lv, calls := fakeModel(t)
	c := lv.NewTrial(10)
	task := liveTask(t, 120)
	os.WriteFile(filepath.Join(task, "instruction.md"), []byte("x\n"), 0o644)
	info, _ := d.cli.Info(context.Background(), client.InfoOptions{})
	other := map[string]string{"aarch64": "linux-x64", "x86_64": "linux-arm64"}[info.Info.Architecture]
	ts, err := d.Run(context.Background(), task, RunOptions{Agent: "opencode", Attempts: 1, JobsDir: t.TempDir(), JobName: "mm", Live: lv, Counter: c,
		OpenCode: &OpenCodeSource{Version: "0.0.0-test", CacheDir: t.TempDir(), Registry: reg.URL}, ExpectTarget: other})
	if err != nil || len(ts) != 1 || ts[0].Outcome() == "passed" || calls.Load() != 0 {
		t.Fatalf("%+v %v calls %d", ts, err, calls.Load())
	}
	noQ4Leftovers(t, d)
}

// The fixed session title is part of the launch: the argv carries it once before the
// instruction, and the launch digests differ from the digests recorded without it.
func TestOpenCodeTitleChangesTheLaunch(t *testing.T) {
	argv := openCodeArgv("m", "--title x")
	opts := argv[:slices.Index(argv, "--")] // the options before the instruction
	i := slices.Index(opts, "--title")
	if i < 0 || opts[i+1] != OpenCodeTitle || slices.Index(opts[i+1:], "--title") >= 0 || argv[len(argv)-1] != "--title x" {
		t.Fatalf("%q", argv)
	}
	before := map[string]string{ // without --title, at public 0773fec
		"linux-arm64": "sha256:4cbdd5c4c2b983dc602474c54b83ed835b01f6d32029bfc724c1ef80663df09f",
		"linux-x64":   "sha256:001f40643e2ba32765ef7c4ab6536a7b356c671d6ed3bcacd0eef63aaeeff3cc",
	}
	for target, old := range before {
		if d, err := DockerLaunchDigest("2.0.26", "ai01", target); err != nil || d == old {
			t.Errorf("%s: the launch identity did not change (%s %v)", target, d, err)
		}
	}
}
