package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
)

func controlledMetadata(t *testing.T, dir string) string {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{
		"image_id": "sha256:" + strings.Repeat("a", 64), "host": "synthetic-host", "container": "synthetic-container-id",
		"started_at": "2026-01-02T03:04:05Z", "restart_count": 0, "prompt_tokens_details": true, "prefix_caching": false,
		"max_model_len": 65536, "served_model": "synthetic-model", "reasoning_parser": "qwen3", "tool_call_parser": "qwen3_coder",
		"capture_target": "synthetic-host", "capture_container": "synthetic-container",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "expect.json")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func controlledArgs(file string) []string {
	return []string{"ev-synthetic@3", "--backend-expect", file, "--study-launch", "single-root-v1", "--ripgrep-binary", "offline-rg", "--opencode-tgz", "offline-opencode.tgz", "--agent", "opencode", "--attempts", "1"}
}

// Use the same option registration, flag reordering, preparation and conversion as evalRun.
func parsedControlled(t *testing.T, dir string, args []string) (*controlledEvalRun, pipeline.SuiteOptions, error) {
	t.Helper()
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "opencode", "")
	attempts := fs.Int("attempts", 1, "")
	arm := fs.String("arm", "baseline", "")
	tgz := fs.String("opencode-tgz", "", "")
	c := controlledEvalRunFlags(fs)
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return c, pipeline.SuiteOptions{}, err
	}
	c.selected(fs)
	o := pipeline.SuiteOptions{Evals: fs.Args(), Agent: *agent, Attempts: *attempts, Arm: *arm, OpenCodeTgz: *tgz}
	return c, o, c.prepare(context.Background(), dir, o, false)
}

func TestControlledEvalRunFlagsReachCommonSuiteOptions(t *testing.T) {
	dir := t.TempDir()
	file := controlledMetadata(t, dir)
	c, o, err := parsedControlled(t, dir, controlledArgs(file))
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	if err := c.apply(&o, key); err != nil {
		t.Fatal(err)
	}
	if o.StudyLaunchPolicy != runner.StudyLaunchSingleRootV1 || !o.CapturePrelaunchTree || o.RipgrepBinary != "offline-rg" || o.OpenCodeTgz != "offline-opencode.tgz" ||
		o.BackendCapture == nil || o.ExpectedBackend == nil || o.Agent != "opencode" || o.Attempts != 1 || len(o.Evals) != 1 || o.Evals[0] != "ev-synthetic@3" {
		t.Fatal("controlled flags did not reach the common suite")
	}
	if o.ExpectedBackend.HostRef != hint.DeriveID(key, "capture-host", "synthetic-host") || *o.ExpectedBackend.Flags.PrefixCaching {
		t.Fatal("actual key/false metadata not preserved")
	}
	if _, err := os.Stat(filepath.Join(dir, ".hint")); !os.IsNotExist(err) {
		t.Fatal("option preparation created a Store")
	}
}

func TestControlledEvalRunLegacyOptionsStayUnselected(t *testing.T) {
	for _, args := range [][]string{{"ev-synthetic", "--agent", "oracle", "--attempts", "3"}, {"ev-synthetic", "--opencode-tgz", "existing.tgz"}} {
		c, o, err := parsedControlled(t, t.TempDir(), args)
		if err != nil || c.enabled || c.apply(&o, nil) != nil || o.StudyLaunchPolicy != "" || o.CapturePrelaunchTree || o.ExpectedBackend != nil || o.BackendCapture != nil || o.RipgrepBinary != "" {
			t.Fatal("legacy mode acquired controlled options")
		}
	}
}

func TestControlledEvalRunInvalidCombinationsBeforeStore(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"--backend-expect", ""},
		{"--study-launch", "single-root-v1"},
		{"ev-synthetic", "--study-launch", "other", "--backend-expect", "missing", "--ripgrep-binary", "rg", "--opencode-tgz", "tgz"},
		append(controlledArgs("missing"), "--agent", "oracle"),
		append(controlledArgs("missing"), "--attempts", "2"),
		append(controlledArgs("missing"), "--with", "variant"),
		append(controlledArgs("missing"), "second-eval"),
	} {
		r := run(t, dir, "", append([]string{"eval", "run"}, args...)...)
		if r.code == 0 || !strings.Contains(r.stderr, "controlled capture requires") {
			t.Fatalf("invalid flag set reached another path: %s", r.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".hint")); !os.IsNotExist(err) {
		t.Fatal("invalid opt-in created a Store")
	}
}

func TestControlledEvalRunBackendFileRefusals(t *testing.T) {
	dir := t.TempDir()
	file := controlledMetadata(t, dir)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "large.json")
	if err := os.WriteFile(oversized, make([]byte, runner.MaxBackendExpectationBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, oversized, dir, filepath.Join(dir, "missing")} {
		if _, _, err := parsedControlled(t, dir, controlledArgs(path)); !errors.Is(err, errBackendFile) {
			t.Fatalf("unsafe metadata file accepted: %v", err)
		}
	}
}

func TestControlledEvalRunMalformedMetadataAndPolicyRefuseBeforeRunner(t *testing.T) {
	shop, _ := setup(t)
	must(t, run(t, shop, "", "eval", "on", "--run", "docker")) // Model remains off; no runner is selected.
	file := controlledMetadata(t, t.TempDir())
	good, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	private := strings.Replace(string(good), "{", `{"api_key":"synthetic-secret",`, 1)
	if err := os.WriteFile(file, []byte(private), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, shop, "", append([]string{"eval", "run"}, controlledArgs(file)...)...)
	if r.code == 0 || !strings.Contains(r.stderr, "metadata is invalid") || strings.Contains(r.stderr, "synthetic-secret") {
		t.Fatal("invalid metadata was not refused before runner selection")
	}
	if err := os.WriteFile(file, good, 0o600); err != nil {
		t.Fatal(err)
	}
	r = run(t, shop, "", append([]string{"eval", "run"}, controlledArgs(file)...)...)
	if r.code == 0 || !strings.Contains(strings.ToLower(r.stderr), "model") {
		t.Fatalf("model-off policy did not refuse before engine selection: %s", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(shop, ".hint", "state", "evals", "work")); !os.IsNotExist(err) {
		t.Fatal("refused command wrote a run journal")
	}
}

func TestControlledEvalRunOriginalContextDeadlines(t *testing.T) {
	entered := time.Now().Add(-30 * time.Second)
	work, total, cancel := controlledEvalContext(entered)
	defer cancel()
	w, wok := work.Deadline()
	f, fok := total.Deadline()
	if !wok || !fok || !w.Equal(entered.Add(600*time.Second)) || !f.Equal(entered.Add(900*time.Second)) {
		t.Fatal("the command renewed or omitted its original deadlines")
	}
	cancel()
	if work.Err() == nil || total.Err() == nil {
		t.Fatal("cancellation did not reach both contexts")
	}
}
