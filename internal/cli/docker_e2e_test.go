package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

// 07B-J §23: with the policy runner docker, an unavailable engine is an error before any
// model or runner step, and Harbor is never invoked (no fallback).
func TestDockerRunnerNoFallback(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker", "--runner", "docker"))
	marker := filepath.Join(t.TempDir(), "harbor-called")
	fake := filepath.Join(t.TempDir(), "harbor")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+marker+"\necho harbor 0.23.0\n"), 0o755)
	t.Setenv("HINT_HARBOR", fake)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	if r := runEval(t, shop, &spawned, "eval", "run", "--agent", "oracle"); r.code == 0 || !strings.Contains(r.stderr, "Docker") {
		t.Fatalf("%+v", r)
	}
	if r := runEval(t, shop, &spawned, "eval", "__qualify"); r.code == 0 {
		t.Fatalf("worker: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Harbor was invoked")
	}
	if r := runEval(t, shop, &spawned, "eval", "on", "--runner", "podman"); r.code == 0 {
		t.Fatal("an unknown runner was accepted")
	}
}

func dockerE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1 to run the container runner end to end")
	}
	d, err := runner.FindDocker(context.Background())
	if err != nil {
		t.Skip(err)
	}
	d.Close()
}

// The clean-install offline path with the built-in runner: a declared B1 eval is qualified by
// the worker and validated, then runs with oracle (passed) and nop (failed); trials name the
// container runner; the report checks.
func TestDockerRunnerCLIOfflinePaths(t *testing.T) {
	dockerE2E(t)
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker")) // the default runner
	id := declaredMappedEval(t, shop, &spawned)
	r := must(t, runEval(t, shop, &spawned, "eval", "__qualify"))
	if !strings.Contains(r.stdout, "validated:1") {
		show := runEval(t, shop, &spawned, "eval", "show", id)
		t.Fatalf("worker: %s\n%s", r.stdout, show.stdout)
	}
	if out := must(t, runEval(t, shop, &spawned, "eval", "run", "--agent", "oracle", id)).stdout; !strings.Contains(out, "passed:1") {
		t.Fatalf("oracle: %s", out)
	}
	if out := must(t, runEval(t, shop, &spawned, "eval", "run", "--agent", "nop", id)).stdout; !strings.Contains(out, "failed:1") {
		t.Fatalf("nop: %s", out)
	}
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	names, _ := l.Manifests("trial")
	for _, n := range names {
		var tr map[string]any
		l.Manifest("trial", n, &tr)
		if tr["runner"].(map[string]any)["name"] != "hint-docker" || tr["result_trust"] != "process_separated" {
			t.Fatalf("trial %v %v", tr["runner"], tr["result_trust"])
		}
	}
	file := filepath.Join(t.TempDir(), "report.json")
	if r := runEval(t, shop, &spawned, "eval", "report", "-o", file); r.code != 0 {
		t.Fatalf("report: %s %s", r.stdout, r.stderr)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `"hint-docker"`) {
		t.Fatalf("report %s", b)
	}
}

// declaredMappedEval declares a B1 eval in the fixture repository and links its request.
func declaredMappedEval(t *testing.T, shop string, spawned *[]string) string {
	t.Helper()
	// the reference changes only tool.sh: every near miss (the empty change) must fail
	os.WriteFile(filepath.Join(shop, "go.mod"), []byte("module shop\n\ngo 1.26\n"), 0o644)
	os.WriteFile(filepath.Join(shop, "tool.sh"), []byte("cat\n"), 0o644)
	testkit.Git(t, shop, "add", "go.mod", "tool.sh")
	testkit.Git(t, shop, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(testkit.Git(t, shop, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(shop, "tool.sh"), []byte("tr a-z A-Z\n"), 0o644)
	testkit.Git(t, shop, "add", "tool.sh")
	testkit.Git(t, shop, "commit", "-q", "-m", "reference")
	refRev := strings.TrimSpace(testkit.Git(t, shop, "rev-parse", "HEAD"))
	dir := t.TempDir()
	req := filepath.Join(dir, "request.txt")
	os.WriteFile(req, []byte("tool.sh must print its input in upper case.\n"), 0o644)
	in := filepath.Join(dir, "c.in")
	os.WriteFile(in, []byte("hello\n"), 0o644)
	must(t, runEval(t, shop, spawned, "eval", "declare-io", "--base", base, "--reference", refRev, "--entry", `["sh","tool.sh"]`, "--case", "c="+in, "--request", req))
	s, _ := store.Open(shop)
	l := library.Open(s, 0)
	id := ""
	for _, e := range latestEvals(l) {
		var rec map[string]any
		if l.Manifest("eval", e.Ref, &rec) == nil {
			if v, _ := rec["verifier"].(map[string]any); v["kind"] == "cli-file-v1" {
				id = rec["eval_id"].(string)
			}
		}
	}
	out := must(t, runEval(t, shop, spawned, "eval", "map", id)).stdout
	var f pipeline.MapFile
	json.Unmarshal([]byte(out), &f)
	f.Requirements[0].Quote, f.Requirements[0].Tests = "tool.sh must print its input in upper case", []string{"cases/c::output"}
	mf := filepath.Join(t.TempDir(), "map.json")
	mb, _ := json.Marshal(f)
	os.WriteFile(mf, mb, 0o600)
	must(t, runEval(t, shop, spawned, "eval", "map", id, "--apply", mf))
	return id
}

// The controlled path with a fake model: OpenCode comes from --opencode-tgz (a synthetic
// archive pinned for this test), runs through the gate, fixes the tool; the ledger is
// handed over complete; requests are counted; the report checks.
func TestDockerRunnerCLIControlledFakeModel(t *testing.T) {
	dockerE2E(t)
	shop, _ := setup(t)
	var spawned []string
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker", "--runner", "docker"))
	id := declaredMappedEval(t, shop, &spawned)
	must(t, runEval(t, shop, &spawned, "eval", "__qualify"))
	// the fake model and the policy's local model
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer srv.Close()
	t.Setenv("HINT_TEST_LIVE_KEY", "HINTCANARY-REAL-KEY")
	s, _ := store.Open(shop)
	cfg := *s.Cfg.Evals
	cfg.Model = &evalsModel{Use: "lan", BaseURL: srv.URL + "/v1", Model: "m", KeyEnv: "HINT_TEST_LIVE_KEY"}
	s.Cfg.Evals = &cfg
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	// a synthetic OpenCode for the engine's target, pinned for this test only
	d, _ := runner.FindDocker(context.Background())
	target := map[string]string{"aarch64": "linux-arm64", "x86_64": "linux-x64"}[dockerArch(t, d)]
	d.Close()
	script := `#!/bin/bash
cfg=$HOME/.config/opencode/opencode.json
url=$(grep -o '"baseURL":"[^"]*"' "$cfg" | cut -d'"' -f4)
path=${url#http://gate:8080}
body='{"model":"m","messages":[{"role":"user","content":"hi"}]}'
exec 3<>/dev/tcp/gate/8080
printf 'POST %s/chat/completions HTTP/1.0\r\nHost: gate\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' "$path" ${#body} "$body" >&3
cat <&3 > /dev/null
printf 'tr a-z A-Z\n' > /app/tool.sh
echo '{"type":"step_finish"}'
`
	tgz := synthTgz(t, script)
	old := runner.OpenCodePins[runner.DockerOpenCodeVersion][target]
	runner.OpenCodePins[runner.DockerOpenCodeVersion][target] = sha512hex(tgz)
	defer func() { runner.OpenCodePins[runner.DockerOpenCodeVersion][target] = old }()
	file := filepath.Join(t.TempDir(), "cli.tgz")
	os.WriteFile(file, tgz, 0o600)
	r := must(t, runEval(t, shop, &spawned, "eval", "run", "--agent", "opencode", "--opencode-tgz", file, id))
	if !strings.Contains(r.stdout, "passed:1") || !strings.Contains(r.stdout, "model requests 1") {
		t.Fatalf("run: %s", r.stdout)
	}
	obs, _, err := project.ReadSpool(s)
	if err != nil {
		t.Fatal(err)
	}
	ledgers := 0
	for _, o := range obs {
		if o.ModelLedger != nil {
			ledgers++
			if o.ModelLedger.State != "complete" || o.ModelLedger.Bypass != "enforced" {
				t.Fatalf("ledger %+v", o.ModelLedger)
			}
		}
	}
	if ledgers != 1 {
		t.Fatalf("ledger records %d", ledgers)
	}
	out := filepath.Join(t.TempDir(), "report.json")
	must(t, runEval(t, shop, &spawned, "eval", "report", "-o", out))
}

func synthTgz(t *testing.T, script string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "package/bin/opencode", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.Close()
	zw.Close()
	return b.Bytes()
}

type evalsModel = evals.Model

func sha512hex(b []byte) string { s := sha512.Sum512(b); return hex.EncodeToString(s[:]) }

func dockerArch(t *testing.T, d *runner.Docker) string {
	t.Helper()
	a, err := d.Architecture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// harborMarker installs a fake Harbor (PATH and the former HINT_HARBOR) that records any
// call, and an unreachable engine.
func harborMarker(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "harbor-called")
	fake := filepath.Join(t.TempDir(), "harbor")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+marker+"\necho harbor 0.23.0\n"), 0o755)
	t.Setenv("HINT_HARBOR", fake)
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	return marker
}

// The default runner is the built-in one: a new policy records docker; a legacy policy
// without the field uses docker; Harbor is never called (eval run and the worker); an
// unknown runner is refused; an explicit legacy harbor choice is refused, not rewritten.
func TestDefaultRunnerIsBuiltIn(t *testing.T) {
	shop, _ := setup(t)
	var spawned []string
	marker := harborMarker(t)
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	s, _ := store.Open(shop)
	if s.Cfg.Evals.Run == nil || s.Cfg.Evals.Run.Runner != "docker" {
		t.Fatalf("new policy runner %+v", s.Cfg.Evals.Run)
	}
	check := func(name string) {
		t.Helper()
		if r := runEval(t, shop, &spawned, "eval", "run", "--agent", "oracle"); r.code == 0 || !strings.Contains(r.stderr, "Docker") {
			t.Fatalf("%s: eval run %+v", name, r)
		}
		if r := runEval(t, shop, &spawned, "eval", "__qualify"); r.code == 0 {
			t.Fatalf("%s: worker %+v", name, r)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: Harbor was called", name)
		}
	}
	check("new policy")
	set := func(runner string) {
		s, _ := store.Open(shop)
		s.Cfg.Evals.Run.Runner = runner
		if err := s.SaveConfig(); err != nil {
			t.Fatal(err)
		}
	}
	set("") // a legacy policy without the field
	check("missing field")
	set("podman")
	if r := runEval(t, shop, &spawned, "eval", "run", "--agent", "oracle"); r.code == 0 {
		t.Fatalf("unknown runner: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("unknown runner: Harbor was called")
	}
	// an explicit legacy harbor policy is refused with the migration command: eval run, the
	// worker and eval on --runner harbor change nothing, spawn nothing and never call Harbor;
	// eval on --run docker does not rewrite the explicit choice
	set("harbor")
	before, n := hintTree(t, shop), len(spawned)
	for _, args := range [][]string{{"eval", "run", "--agent", "oracle"}, {"eval", "run", "--agent", "opencode"}, {"eval", "__qualify"}} {
		r := runEval(t, shop, &spawned, args...)
		if r.code == 0 || !strings.Contains(r.stderr, "git hint eval on --runner docker") {
			t.Fatalf("explicit harbor %v: %+v", args, r)
		}
	}
	if r := runEval(t, shop, &spawned, "eval", "on", "--runner", "harbor"); r.code != 2 || !strings.Contains(r.stderr, "git hint eval on --runner docker") {
		t.Fatalf("eval on --runner harbor: %+v", r)
	}
	if hintTree(t, shop) != before || len(spawned) != n {
		t.Fatal("a refused harbor policy changed the project or spawned a process")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("explicit harbor: Harbor was called")
	}
	must(t, runEval(t, shop, &spawned, "eval", "on", "--run", "docker"))
	if s, _ := store.Open(shop); s.Cfg.Evals.Run.Runner != "harbor" {
		t.Fatalf("the explicit choice was rewritten: %+v", s.Cfg.Evals.Run)
	}
	must(t, runEval(t, shop, &spawned, "eval", "on", "--runner", "docker")) // the migration
	if s, _ := store.Open(shop); s.Cfg.Evals.Run.Runner != "docker" {
		t.Fatalf("migration: %+v", s.Cfg.Evals.Run)
	}
	check("migrated")
}

// hintTree is a digest of every file below the project's .hint directory (path, mode, bytes).
func hintTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(filepath.Join(root, ".hint"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %v\n", p, info.Mode())
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
