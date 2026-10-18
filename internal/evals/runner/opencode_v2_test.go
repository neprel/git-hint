package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A pinned 2.x version runs through HINT's adapter class: Harbor gets the import path and
// a PYTHONPATH that holds the adapter; 1.x keeps Harbor's own OpenCode agent.
func TestOpenCodeV2UsesTheAdapter(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	fake := filepath.Join(dir, "harbor")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+rec+".args\necho \"$PYTHONPATH\" > "+rec+".path\nls \"$PYTHONPATH\" > "+rec+".files 2>/dev/null\nexit 1\n"), 0o755)
	h := &Harbor{Bin: fake}
	for _, c := range []struct {
		version, agent string
		adapter        bool
	}{{"2.0.26", "hint_opencode_v2:OpenCodeV2", true}, {OpenCodeVersion, "opencode", false}} {
		os.Remove(rec + ".files")
		h.Run(context.Background(), t.TempDir(), RunOptions{Agent: "opencode", JobsDir: t.TempDir(), JobName: "j", AgentArgs: []string{"version=" + c.version}})
		args, _ := os.ReadFile(rec + ".args")
		files, _ := os.ReadFile(rec + ".files")
		if !strings.Contains(string(args), "-a "+c.agent+" ") || strings.Contains(string(files), "hint_opencode_v2.py") != c.adapter {
			t.Fatalf("%s: args %q files %q", c.version, args, files)
		}
	}
}

// The adapter imports under the pinned Harbor and builds the v2 commands. With
// HINT_DOCKER=1 the install command also runs in the Go task image: the pinned tarball
// installs and prints its version, and a wrong hash stops the install. Network access is
// needed for the npm download; no model and no agent session run.
func TestOpenCodeV2AdapterUnderHarbor(t *testing.T) {
	bin := os.Getenv("HINT_HARBOR")
	if bin == "" {
		t.Skip("set HINT_HARBOR to the harbor executable")
	}
	py := filepath.Join(filepath.Dir(bin), "python")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hint_opencode_v2.py"), openCodeV2Adapter, 0o600)
	script := `import hint_opencode_v2 as m
assert issubclass(m.OpenCodeV2, m.OpenCode)
print(m.install_command("2.0.26"))
print("RUN " + m.run_command("openai/ai01", "fix it", False, ""))`
	cmd := exec.Command(py, "-c", script)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	install, run, _ := strings.Cut(string(out), "\nRUN ")
	for _, want := range []string{"run --standalone --format json --thinking --auto", "-m hint/ai01 -- 'fix it'", "/logs/agent/opencode.txt", "@ai-sdk/openai-compatible", "$OPENAI_BASE_URL"} {
		if !strings.Contains(run, want) {
			t.Fatalf("run command %q lacks %q", run, want)
		}
	}
	if strings.Contains(run, "dangerously-skip-permissions") || !strings.Contains(install, "sha512sum") || !strings.Contains(install, "206df5e88f3056a9") {
		t.Fatalf("install %q run %q", install, run)
	}
	if os.Getenv("HINT_DOCKER") == "" {
		return
	}
	image := "golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452"
	got, err := exec.Command("docker", "run", "--rm", image, "sh", "-c", strings.TrimSpace(install)).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "opencode v2.0.26") {
		t.Fatalf("install: %v %s", err, got)
	}
	bad := strings.Replace(install, "206df5e88f3056a9", "006df5e88f3056a9", 1)
	bad = strings.Replace(bad, "508036ff5224f076", "008036ff5224f076", 1)
	got, err = exec.Command("docker", "run", "--rm", image, "sh", "-c", strings.TrimSpace(bad)).CombinedOutput()
	if err == nil || !strings.Contains(string(got), "hash mismatch") {
		t.Fatalf("a wrong hash installed: %v %s", err, got)
	}
}
