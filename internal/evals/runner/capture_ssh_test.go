package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

const (
	sshImage  = "sha256:5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	sshSecret = "sk-capture-secret-never-leaves"
)

func TestNewSSHCaptureRefusesUnusableNames(t *testing.T) {
	if _, err := NewSSHCapture("capture-target.example.invalid", "example-model-container"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"", "c"}, {"-oProxyCommand=x", "c"}, {"user@host", "c"}, {"a b", "c"}, {"h;id", "c"}, {"h", ""},
		{"h", "-c"}, {"h", "c d"}, {"h", "c;id"}, {"h", "c$(id)"}, {"h", "c'"}, {"h", strings.Repeat("c", 129)}, {strings.Repeat("h", 254), "c"}} {
		if _, err := NewSSHCapture(c[0], c[1]); !errors.Is(err, errCaptureConfig) {
			t.Errorf("%q accepted", c)
		}
	}
}

func TestSSHCaptureArgvIsFixed(t *testing.T) {
	s, _ := NewSSHCapture("capture-host.example.invalid", "example-model-container")
	want := []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ClearAllForwardings=yes",
		"-o", "RequestTTY=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "--", "capture-host.example.invalid", "python3", "-I", "-", "example-model-container"}
	if got := s.argv(); !slices.Equal(got, want) {
		t.Fatalf("argv %q", got)
	}
	// host-key verification stays as the SSH configuration has it
	for _, a := range s.argv() {
		if strings.Contains(a, "StrictHostKeyChecking") || strings.Contains(a, "UserKnownHostsFile") {
			t.Fatal(a)
		}
	}
}

// hostLine is a valid host output with every key; edit changes it.
func hostLine(edit func(m map[string]any)) []byte {
	m := map[string]any{"image_id": sshImage, "host": "capture-host.example.invalid", "container": "c0ffeec0ffee", "started_at": "2026-01-02T03:04:05.000000006Z",
		"restart_count": 0, "prompt_tokens_details": true, "prefix_caching": true, "max_model_len": 262144, "served_model": "example-model",
		"reasoning_parser": "qwen3", "tool_call_parser": "qwen3_coder"}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

func fakeRun(out []byte, err error, got *[]string, stdin *[]byte) func(context.Context, []string, []byte, io.Writer) error {
	return func(_ context.Context, argv []string, in []byte, w io.Writer) error {
		if got != nil {
			*got, *stdin = argv, in
		}
		if _, werr := w.Write(out); werr != nil {
			return werr
		}
		return err
	}
}

func TestSSHCaptureInspect(t *testing.T) {
	s, _ := NewSSHCapture("capture-host.example.invalid", "example-model-container")
	var argv []string
	var stdin []byte
	s.run = fakeRun(hostLine(nil), nil, &argv, &stdin)
	r, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, s.argv()) || !bytes.Equal(stdin, captureHostScript) {
		t.Fatal("not the fixed argv and script")
	}
	snap, err := sanitize(r, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ImageID != sshImage || *snap.StartedAt != "2026-01-02T03:04:05.000000006Z" || *snap.RestartCount != 0 || !*snap.Flags.PromptTokensDetails ||
		*snap.Flags.MaxModelLen != 262144 || *snap.Flags.ReasoningParser != "qwen3" || *snap.Flags.ToolCallParser != "qwen3_coder" ||
		*snap.Flags.ServedModelRef != testRef("capture-served-model", "example-model") || snap.HostRef != testRef("capture-host", "capture-host.example.invalid") {
		t.Fatalf("%+v", snap)
	}
	b, _ := json.Marshal(snap)
	for _, raw := range []string{"capture-host.example.invalid", "c0ffeec0ffee", "\"example-model\""} {
		if bytes.Contains(b, []byte(raw)) {
			t.Fatalf("raw %s recorded", raw)
		}
	}
}

func TestSSHCaptureRefusesUnusableOutput(t *testing.T) {
	good := hostLine(nil)
	cases := map[string][]byte{
		"empty":            nil,
		"malformed":        []byte("{\"image_id\":\n"),
		"not an object":    []byte("[1]\n"),
		"error object":     []byte("{\"error\":\"inspect_failed\"}\n"),
		"two lines":        append(append([]byte{}, good...), good...),
		"trailing data":    append(bytes.TrimSuffix(good, []byte("\n")), []byte(" {}\n")...),
		"extra member":     hostLine(func(m map[string]any) { m["env"] = "x" }),
		"missing member":   hostLine(func(m map[string]any) { delete(m, "restart_count") }),
		"duplicate member": []byte(strings.Replace(string(good), `"host":"capture-host.example.invalid"`, `"host":"capture-host.example.invalid","host":"other"`, 1)),
		"wrong type":       hostLine(func(m map[string]any) { m["restart_count"] = "0" }),
		"bool as string":   hostLine(func(m map[string]any) { m["prefix_caching"] = "true" }),
		"served marker":    hostLine(func(m map[string]any) { m["served_model"] = "\x00invalid" }),
		"served empty":     hostLine(func(m map[string]any) { m["served_model"] = "" }),
		"over the limit":   bytes.Repeat([]byte("x"), captureStdoutLimit+1),
		"long line":        hostLine(func(m map[string]any) { m["host"] = strings.Repeat("h", maxCaptureField) }),
	}
	if !strings.Contains(string(cases["duplicate member"]), `"host":"other"`) {
		t.Fatal("duplicate case not built")
	}
	for name, out := range cases {
		s, _ := NewSSHCapture("capture-host.example.invalid", "c")
		s.run = fakeRun(out, nil, nil, nil)
		if _, err := s.Inspect(context.Background()); !errors.Is(err, errUnusableCapture) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a runner that ignores the refused write still yields no capture
	s, _ := NewSSHCapture("capture-host.example.invalid", "c")
	s.run = func(_ context.Context, _ []string, _ []byte, w io.Writer) error {
		_, _ = w.Write(good)
		_, _ = w.Write(bytes.Repeat([]byte(" "), captureStdoutLimit)) // refused; the kept part alone is valid
		return nil
	}
	if _, err := s.Inspect(context.Background()); !errors.Is(err, errUnusableCapture) {
		t.Fatal("output over the limit was used")
	}
	s.run = fakeRun(good, errors.New("exit status 255"), nil, nil)
	if _, err := s.Inspect(context.Background()); !errors.Is(err, errUnusableCapture) {
		t.Fatal("a failed command was used")
	}
}

func TestSSHCaptureUnknownValues(t *testing.T) {
	s, _ := NewSSHCapture("capture-host.example.invalid", "c")
	s.run = fakeRun(hostLine(func(m map[string]any) {
		for _, k := range []string{"restart_count", "prompt_tokens_details", "prefix_caching", "max_model_len", "served_model", "reasoning_parser"} {
			m[k] = nil
		}
		m["tool_call_parser"] = "\x00invalid"
	}), nil, nil, nil)
	r, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := sanitize(r, testRef)
	if err != nil {
		t.Fatal(err)
	}
	f := snap.Flags
	if snap.RestartCount != nil || f.PromptTokensDetails != nil || f.PrefixCaching != nil || f.MaxModelLen != nil || f.ServedModelRef != nil ||
		f.ReasoningParser != nil || *f.ToolCallParser != "other" {
		t.Fatalf("%+v", f)
	}
}

func TestCaptureOptionsOptIn(t *testing.T) {
	s, _ := NewSSHCapture("capture-host.example.invalid", "c")
	if tr, ref := (RunOptions{}).captureOptions(); tr != nil || ref != nil {
		t.Fatal("capture without opt-in")
	}
	if tr, ref := (RunOptions{BackendCapture: s}).captureOptions(); tr != nil || ref != nil {
		t.Fatal("capture without a key")
	}
	if tr, ref := (RunOptions{CaptureKey: []byte("k")}).captureOptions(); tr != nil || ref != nil {
		t.Fatal("capture without a transport")
	}
	key := []byte("k")
	tr, ref := (RunOptions{BackendCapture: s, CaptureKey: key}).captureOptions()
	if tr != captureTransport(s) || ref("capture-host", "capture-host.example.invalid") != hint.DeriveID([]byte("k"), "capture-host", "capture-host.example.invalid") {
		t.Fatal("not the opt-in transport and keyed references")
	}
	key[0] = 'x' // the derivation owns its key
	if ref("capture-host", "capture-host.example.invalid") != hint.DeriveID([]byte("k"), "capture-host", "capture-host.example.invalid") {
		t.Fatal("the reference follows a changed caller key")
	}
}

// The host script, run locally against a fake docker (no SSH, no backend).

type inspectFixture struct {
	cmd  []string
	env  []string
	rc   int
	raw  string // replaces the inspect document when set
	name string // the container argument (default "example-model-container")
}

func runHostScript(t *testing.T, f inspectFixture) (map[string]any, string, int) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	doc := f.raw
	if doc == "" {
		b, _ := json.Marshal([]map[string]any{{"Id": "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff", "Image": sshImage,
			"RestartCount": 0, "State": map[string]any{"StartedAt": "2026-01-02T03:04:05.000000006Z"},
			"Config": map[string]any{"Cmd": f.cmd, "Env": f.env}}})
		doc = string(b)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = 'inspect --type container --' ] || exit 9\ncat \"$(dirname \"$0\")/doc.json\"\nexit " + itoa(f.rc) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	name := f.name
	if name == "" {
		name = "example-model-container"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, py, "-I", "-", name)
	c.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	c.Stdin = bytes.NewReader(captureHostScript)
	var out bytes.Buffer
	c.Stdout = &out
	err = c.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	return m, out.String(), code
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

var servedCmd = []string{"vllm", "serve", "/models/ai-model", "--served-model-name", "example-model", "--api-key", sshSecret, "--max-model-len", "262144",
	"--enable-prefix-caching", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_coder", "--reasoning-parser", "qwen3", "--enable-prompt-tokens-details"}

func TestHostScriptFiltersOnTheHost(t *testing.T) {
	m, out, code := runHostScript(t, inspectFixture{cmd: servedCmd, env: []string{"AI_KEY=" + sshSecret}})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, sshSecret) || strings.Contains(out, "AI_KEY") || strings.Contains(out, "/models/ai-model") || strings.Contains(out, "auto-tool") {
		t.Fatalf("unfiltered output: %s", out)
	}
	if strings.Count(out, "\n") != 1 || !exactKeys(bytes.TrimSpace([]byte(out)), hostCaptureKeys) {
		t.Fatalf("not one line with exactly the allowlisted keys: %s", out)
	}
	if m["prompt_tokens_details"] != true || m["prefix_caching"] != true || m["max_model_len"] != 262144.0 || m["served_model"] != "example-model" ||
		m["reasoning_parser"] != "qwen3" || m["tool_call_parser"] != "qwen3_coder" || m["image_id"] != sshImage || m["restart_count"] != 0.0 {
		t.Fatalf("%v", m)
	}
	// the Go side accepts it end to end
	if _, err := parseHostCapture([]byte(out)); err != nil {
		t.Fatal(err)
	}
}

func TestHostScriptAmbiguousFlagsAreUnknown(t *testing.T) {
	base := []string{"vllm", "serve", "/m"}
	cases := []struct {
		name string
		cmd  []string
		want map[string]any // expected values; nil = JSON null
	}{
		{"absent", base, map[string]any{"prompt_tokens_details": nil, "prefix_caching": nil, "max_model_len": nil, "served_model": nil}},
		{"explicit off", append(slices.Clone(base), "--no-enable-prefix-caching"), map[string]any{"prefix_caching": false}},
		{"equals form", append(slices.Clone(base), "--max-model-len=4096", "--served-model-name=example-model"), map[string]any{"max_model_len": 4096.0, "served_model": "example-model"}},
		{"bool repeated", append(slices.Clone(base), "--enable-prefix-caching", "--enable-prefix-caching"), map[string]any{"prefix_caching": nil}},
		{"bool both forms", append(slices.Clone(base), "--enable-prefix-caching", "--no-enable-prefix-caching"), map[string]any{"prefix_caching": nil}},
		{"bool with value", append(slices.Clone(base), "--enable-prompt-tokens-details=true"), map[string]any{"prompt_tokens_details": nil}},
		{"bool followed by value", append(slices.Clone(base), "--enable-prompt-tokens-details", "false"), map[string]any{"prompt_tokens_details": nil}},
		{"off repeated", append(slices.Clone(base), "--no-enable-prefix-caching", "--no-enable-prefix-caching"), map[string]any{"prefix_caching": nil}},
		{"length repeated", append(slices.Clone(base), "--max-model-len", "4096", "--max-model-len", "8192"), map[string]any{"max_model_len": -1.0}},
		{"length conflicting forms", append(slices.Clone(base), "--max-model-len", "4096", "--max-model-len=4096"), map[string]any{"max_model_len": -1.0}},
		{"length not a number", append(slices.Clone(base), "--max-model-len", "32k"), map[string]any{"max_model_len": -1.0}},
		{"length bare", append(slices.Clone(base), "--max-model-len"), map[string]any{"max_model_len": -1.0}},
		{"served repeated", append(slices.Clone(base), "--served-model-name", "a", "--served-model-name", "b"), map[string]any{"served_model": "\x00invalid"}},
		{"parser bad chars", append(slices.Clone(base), "--reasoning-parser", "qwen3;id"), map[string]any{"reasoning_parser": "\x00invalid"}},
		{"parser repeated", append(slices.Clone(base), "--tool-call-parser", "hermes", "--tool-call-parser", "hermes"), map[string]any{"tool_call_parser": "\x00invalid"}},
	}
	for _, c := range cases {
		m, out, code := runHostScript(t, inspectFixture{cmd: c.cmd})
		if code != 0 {
			t.Fatalf("%s: exit %d %s", c.name, code, out)
		}
		for k, v := range c.want {
			if m[k] != v {
				t.Errorf("%s: %s = %#v, want %#v", c.name, k, m[k], v)
			}
		}
		// the client never turns an ambiguous value into a usable profile flag
		r, err := parseHostCapture([]byte(out))
		if err != nil {
			continue // an unusable served name is an error capture
		}
		if snap, err := sanitize(r, testRef); err == nil {
			f := snap.Flags
			if c.name != "explicit off" && f.PrefixCaching != nil && *f.PrefixCaching == false {
				t.Errorf("%s: false inferred", c.name)
			}
			if p := f.ReasoningParser; p != nil && *p != "other" && *p != "qwen3" {
				t.Errorf("%s: parser %q", c.name, *p)
			}
		}
	}
}

func TestHostScriptFailures(t *testing.T) {
	for name, f := range map[string]inspectFixture{
		"inspect fails":     {cmd: servedCmd, rc: 1},
		"not one container": {raw: "[]"},
		"two containers":    {raw: `[{"Id":"a"},{"Id":"b"}]`},
		"not json":          {raw: "nope"},
		"bad name":          {cmd: servedCmd, name: "a;id"},
	} {
		_, out, code := runHostScript(t, f)
		if code != 3 || !strings.HasPrefix(out, `{"error": "`) || strings.Contains(out, sshSecret) {
			t.Errorf("%s: exit %d %q", name, code, out)
		}
		if _, err := parseHostCapture([]byte(out)); err == nil {
			t.Errorf("%s: error output accepted", name)
		}
	}
}

// The whole chain without SSH: the capturer drives SSHCapture whose command runs the real host
// script locally against the fake docker.
func TestSSHCaptureChainThroughCapturer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	_, out, code := runHostScript(t, inspectFixture{cmd: servedCmd})
	if code != 0 {
		t.Fatal(out)
	}
	s, _ := NewSSHCapture("capture-host.example.invalid", "example-model-container")
	s.run = fakeRun([]byte(out), nil, nil, nil)
	k := newCapturer(s, testRef)
	c, _ := agentCounter()
	side := k.capture(context.Background(), c)
	if side.status != CaptureComplete || side.snap == nil || side.snap.ImageID != sshImage {
		t.Fatalf("%+v", side)
	}
}

// The real command runner: cancellation ends the command and a child that keeps the output open
// cannot extend it beyond WaitDelay; output over the limit is refused.
func TestRunSSHBounds(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	var out limitedBuffer
	err := runSSH(ctx, []string{"sh", "-c", "sleep 30 & sleep 30"}, nil, &out)
	if err == nil || time.Since(began) > 200*time.Millisecond+captureWaitDelay+2*time.Second {
		t.Fatalf("not bounded: %v after %v", err, time.Since(began))
	}
	s := &SSHCapture{target: "h", container: "c", run: func(ctx context.Context, _ []string, in []byte, w io.Writer) error {
		return runSSH(ctx, []string{"sh", "-c", "head -c 9000 /dev/zero"}, in, w)
	}}
	if _, err := s.Inspect(context.Background()); !errors.Is(err, errUnusableCapture) {
		t.Fatal("output over the limit was used")
	}
}

// A zero value or a struct literal outside the constructor never runs a command and never panics.
func TestSSHCaptureZeroAndLiteralNeverRun(t *testing.T) {
	ran := false
	spy := func(context.Context, []string, []byte, io.Writer) error { ran = true; return nil }
	for name, s := range map[string]*SSHCapture{
		"nil":              nil,
		"zero":             {},
		"no runner":        {target: "h", container: "c"},
		"metacharacter":    {target: "h", container: "c;id", run: spy},
		"option as target": {target: "-oProxyCommand=x", container: "c", run: spy},
		"user in target":   {target: "u@h", container: "c", run: spy},
		"substitution":     {target: "h", container: "$(id)", run: spy},
	} {
		if _, err := s.Inspect(context.Background()); !errors.Is(err, errUnusableCapture) || ran {
			t.Errorf("%s: %v ran=%v", name, err, ran)
		}
	}
}
