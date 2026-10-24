package runner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SSHCapture is the real capture transport of the controlled research setup: one read-only
// docker inspect of one model container over the existing SSH configuration, filtered on the
// host to the capture allowlist (capture_host.py) before anything leaves it. It is never a
// default: only a caller that sets RunOptions.BackendCapture uses it.
// Only NewSSHCapture creates a usable one: its fields are private, and Inspect checks them again,
// so no value outside the patterns reaches the argv.
type SSHCapture struct {
	target    string // host name or address from the existing SSH configuration
	container string // the model container's name
	// run executes the fixed argv with the host script on stdin (tests replace it)
	run func(ctx context.Context, argv []string, stdin []byte, stdout io.Writer) error
}

const (
	captureStdoutLimit = 8 << 10
	captureWaitDelay   = 2 * time.Second
)

var (
	//go:embed capture_host.py
	captureHostScript []byte
	sshTarget         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
	containerName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	errCaptureConfig  = errors.New("the capture target or container name is not usable")
)

// NewSSHCapture checks the target and the container name; a value outside the patterns is
// refused before any run.
func NewSSHCapture(target, container string) (*SSHCapture, error) {
	if !sshTarget.MatchString(target) || !containerName.MatchString(container) {
		return nil, errCaptureConfig
	}
	return &SSHCapture{target: target, container: container, run: runSSH}, nil
}

// argv is the fixed command: no local shell, no forwarding, no TTY, no shared connection; the
// remote command is three fixed words and the checked container name.
func (s *SSHCapture) argv() []string {
	return []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ClearAllForwardings=yes",
		"-o", "RequestTTY=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "--", s.target, "python3", "-I", "-", s.container}
}

func runSSH(ctx context.Context, argv []string, stdin []byte, stdout io.Writer) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(stdin), stdout, io.Discard // stderr is never recorded
	c.WaitDelay = captureWaitDelay
	return c.Run()
}

// limitedBuffer keeps at most limit bytes and remembers an overflow.
type limitedBuffer struct {
	buf  bytes.Buffer
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > captureStdoutLimit {
		b.over = true
		return 0, errUnusableCapture
	}
	return b.buf.Write(p)
}

// hostCapture is the host script's one output line; every key is required.
type hostCapture struct {
	ImageID             string  `json:"image_id"`
	Host                string  `json:"host"`
	Container           string  `json:"container"`
	StartedAt           string  `json:"started_at"`
	RestartCount        *int64  `json:"restart_count"`
	PromptTokensDetails *bool   `json:"prompt_tokens_details"`
	PrefixCaching       *bool   `json:"prefix_caching"`
	MaxModelLen         *int64  `json:"max_model_len"`
	ServedModel         *string `json:"served_model"`
	ReasoningParser     *string `json:"reasoning_parser"`
	ToolCallParser      *string `json:"tool_call_parser"`
}

var hostCaptureKeys = []string{"image_id", "host", "container", "started_at", "restart_count", "prompt_tokens_details",
	"prefix_caching", "max_model_len", "served_model", "reasoning_parser", "tool_call_parser"}

// Inspect runs the fixed command once (no retry) under ctx, which carries the capture bound.
func (s *SSHCapture) Inspect(ctx context.Context) (rawCapture, error) {
	if s == nil || s.run == nil || !sshTarget.MatchString(s.target) || !containerName.MatchString(s.container) {
		return rawCapture{}, errUnusableCapture // a zero value or a value outside the patterns never runs
	}
	var out limitedBuffer
	if err := s.run(ctx, s.argv(), captureHostScript, &out); err != nil || out.over {
		return rawCapture{}, errUnusableCapture
	}
	return parseHostCapture(out.buf.Bytes())
}

// exactKeys reports whether b is one JSON object whose members are exactly keys, each once (a
// repeated member would otherwise silently keep its last value).
func exactKeys(b []byte, keys []string) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		k, ok := t.(string)
		if err != nil || !ok || seen[k] || !slices.Contains(keys, k) {
			return false
		}
		seen[k] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return false
		}
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return false
	}
	_, err := d.Token()
	return err == io.EOF && len(seen) == len(keys)
}

// parseHostCapture decodes exactly one line with exactly the allowlisted keys.
func parseHostCapture(b []byte) (rawCapture, error) {
	line := bytes.TrimSuffix(b, []byte("\n"))
	if len(line) == 0 || bytes.ContainsRune(line, '\n') || len(line) > maxCaptureField {
		return rawCapture{}, errUnusableCapture
	}
	if !exactKeys(line, hostCaptureKeys) {
		return rawCapture{}, errUnusableCapture
	}
	var h hostCapture // exactKeys has refused every other member
	if json.Unmarshal(line, &h) != nil {
		return rawCapture{}, errUnusableCapture
	}
	r := rawCapture{ImageID: h.ImageID, Host: h.Host, Container: h.Container, StartedAt: h.StartedAt, RestartCount: h.RestartCount,
		PromptTokensDetails: h.PromptTokensDetails, PrefixCaching: h.PrefixCaching, MaxModelLen: h.MaxModelLen}
	if h.ServedModel != nil {
		if strings.ContainsRune(*h.ServedModel, 0) || *h.ServedModel == "" {
			return rawCapture{}, errUnusableCapture // a served name the host could not pass
		}
		r.ServedModel = *h.ServedModel
	}
	for _, p := range []struct {
		in  *string
		out *string
	}{{h.ReasoningParser, &r.ReasoningParser}, {h.ToolCallParser, &r.ToolCallParser}} {
		if p.in != nil {
			*p.out = *p.in // a name outside the reviewed list (also the host's marker) becomes other
			if *p.in == "" || strings.ContainsRune(*p.in, 0) {
				*p.out = "\x00"
			}
		}
	}
	return r, nil
}
