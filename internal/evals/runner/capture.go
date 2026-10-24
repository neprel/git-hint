package runner

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Backend captures (draft v0.2 ModelLedger.backend_capture): a sanitized read-only inspect of the
// model backend before the agent's launch and after the drain, each bracketed on the counter's
// monotonic clock. DISABLED BY DEFAULT: only a trial whose options carry a capture transport and
// a reference function captures; no public option enables it and no production transport exists.
const (
	CaptureMethod    = "runner_bracket_v1"
	CaptureCollector = "hint-runner-capture"
	CaptureVersion   = "0.1.0"
	captureBound     = 60 * time.Second  // one capture
	captureBudget    = 120 * time.Second // the combined active time of both captures of a trial
	maxCaptureField  = 4096              // bytes of one raw inspect value
)

// Capture statuses.
const (
	CaptureComplete     = "complete"
	CaptureTimeout      = "timeout"
	CaptureError        = "error"
	CaptureNotAttempted = "not_attempted"
)

// captureTransport performs one read-only inspect of the model backend and returns its
// allowlisted values (tests replace it).
type captureTransport interface {
	Inspect(ctx context.Context) (rawCapture, error)
}

// rawCapture is the allowlisted inspect as received. Host, container and served-model names stay
// in memory: only references derived from them are recorded. A nil value was not reported.
type rawCapture struct {
	ImageID, Host, Container, ServedModel string
	StartedAt                             string
	RestartCount                          *int64
	PromptTokensDetails, PrefixCaching    *bool
	MaxModelLen                           *int64
	ReasoningParser, ToolCallParser       string
}

// The reviewed parser names (the public enum); a reported name outside the list is recorded as
// other, which never supports a profile.
var (
	reasoningParsers = []string{"deepseek_r1", "qwen3", "granite", "glm45", "gpt_oss", "mistral", "hunyuan_a13b", "step3", "seed_oss"}
	toolCallParsers  = []string{"hermes", "mistral", "llama3_json", "llama4_json", "pythonic", "granite", "internlm", "jamba", "xlam", "qwen3_coder",
		"qwen3_xml", "deepseek_v3", "deepseek_v31", "kimi_k2", "glm45", "openai", "minimax", "hunyuan_a13b", "seed_oss"}
	imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// CaptureSnapshot is the sanitized inspect: identifiers are references derived on the client;
// an unreported value is nil, never false or zero.
type CaptureSnapshot struct {
	ImageID      string       `json:"image_id"`
	HostRef      string       `json:"host_ref"`
	ContainerRef string       `json:"container_ref"`
	StartedAt    *string      `json:"started_at"`
	RestartCount *int64       `json:"restart_count"`
	Flags        CaptureFlags `json:"flags"`
}

// CaptureFlags are the allowlisted backend flags.
type CaptureFlags struct {
	PromptTokensDetails *bool   `json:"prompt_tokens_details"`
	PrefixCaching       *bool   `json:"prefix_caching"`
	MaxModelLen         *int64  `json:"max_model_len"`
	ReasoningParser     *string `json:"reasoning_parser"`
	ToolCallParser      *string `json:"tool_call_parser"`
	ServedModelRef      *string `json:"served_model_ref"`
}

// errUnusableCapture is a response outside the allowlist's bounds (recorded as an error capture).
var errUnusableCapture = errors.New("the capture response is not usable")

// sanitize maps a raw inspect to the snapshot with references from ref, or refuses it.
func sanitize(r rawCapture, ref func(kind, raw string) string) (*CaptureSnapshot, error) {
	for _, v := range []string{r.ImageID, r.Host, r.Container, r.ServedModel, r.StartedAt, r.ReasoningParser, r.ToolCallParser} {
		if len(v) > maxCaptureField {
			return nil, errUnusableCapture
		}
	}
	if !imageID.MatchString(r.ImageID) || r.Host == "" || r.Container == "" {
		return nil, errUnusableCapture
	}
	if (r.RestartCount != nil && (*r.RestartCount < 0 || *r.RestartCount > 1<<53-1)) || (r.MaxModelLen != nil && (*r.MaxModelLen < 1 || *r.MaxModelLen > 1<<31-1)) {
		return nil, errUnusableCapture
	}
	s := &CaptureSnapshot{ImageID: r.ImageID, HostRef: ref("capture-host", r.Host), ContainerRef: ref("capture-container", r.Container),
		StartedAt: engineTime(r.StartedAt), RestartCount: clonePtr(r.RestartCount),
		Flags: CaptureFlags{PromptTokensDetails: clonePtr(r.PromptTokensDetails), PrefixCaching: clonePtr(r.PrefixCaching), MaxModelLen: clonePtr(r.MaxModelLen),
			ReasoningParser: parser(r.ReasoningParser, reasoningParsers), ToolCallParser: parser(r.ToolCallParser, toolCallParsers)}}
	if r.ServedModel != "" {
		v := ref("capture-served-model", r.ServedModel)
		s.Flags.ServedModelRef = &v
	}
	return s, nil
}

func parser(name string, known []string) *string {
	if name == "" {
		return nil
	}
	if !slices.Contains(known, name) {
		name = "other"
	}
	return &name
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (s *CaptureSnapshot) clone() *CaptureSnapshot {
	if s == nil {
		return nil
	}
	c := *s
	c.StartedAt, c.RestartCount = clonePtr(s.StartedAt), clonePtr(s.RestartCount)
	f := s.Flags
	c.Flags = CaptureFlags{PromptTokensDetails: clonePtr(f.PromptTokensDetails), PrefixCaching: clonePtr(f.PrefixCaching), MaxModelLen: clonePtr(f.MaxModelLen),
		ReasoningParser: clonePtr(f.ReasoningParser), ToolCallParser: clonePtr(f.ToolCallParser), ServedModelRef: clonePtr(f.ServedModelRef)}
	return &c
}

// captureSide is one capture as recorded: offsets from the counter's anchor.
type captureSide struct {
	status     string
	send, recv *time.Duration
	snap       *CaptureSnapshot
}

// clone is a copy that shares no memory with s (the ledger takes ownership of its own copy).
func (s captureSide) clone() captureSide {
	return captureSide{status: s.status, send: clonePtr(s.send), recv: clonePtr(s.recv), snap: s.snap.clone()}
}

// capturer runs the captures of one trial within one finite budget; no retry.
// The budget counts only the time spent inside captures: the agent's run, the setup and the drain
// between the two captures never consume it; the trial's own limits and cancellation stay separate.
type capturer struct {
	transport captureTransport
	ref       func(kind, raw string) string
	bound     time.Duration    // one capture (captureBound)
	budget    time.Duration    // the combined active capture time (captureBudget)
	spent     time.Duration    // active capture time used so far
	now       func() time.Time // the clock of the active time (tests replace it)
}

func newCapturer(t captureTransport, ref func(kind, raw string) string) *capturer {
	return &capturer{transport: t, ref: ref, bound: captureBound, budget: captureBudget, now: time.Now}
}

// capture brackets one inspect on the counter's monotonic clock: send is read before the request,
// recv after the complete response. It is cancelled with ctx and bounded by captureBound and by
// the rest of the active-time budget; a spent budget is not_attempted.
func (k *capturer) capture(ctx context.Context, c *Counter) captureSide {
	rest := k.budget - k.spent
	if rest <= 0 || ctx.Err() != nil {
		return captureSide{status: CaptureNotAttempted}
	}
	cctx, cancel := context.WithTimeout(ctx, min(k.bound, rest))
	defer cancel()
	began := k.now()
	send := c.mark()
	raw, err := k.transport.Inspect(cctx)
	k.spent += max(k.now().Sub(began), 0)
	if cctx.Err() != nil { // the bound or the trial's cancellation ended it; a late reply is not used
		if ctx.Err() == nil {
			return captureSide{status: CaptureTimeout, send: &send}
		}
		return captureSide{status: CaptureError, send: &send}
	}
	if err != nil {
		return captureSide{status: CaptureError, send: &send}
	}
	recv := c.mark()
	snap, err := sanitize(raw, k.ref)
	if err != nil {
		return captureSide{status: CaptureError, send: &send}
	}
	return captureSide{status: CaptureComplete, send: &send, recv: &recv, snap: snap}
}

// ReportCapture is one capture of the account; offsets only when the account has entries.
type ReportCapture struct {
	Status   string           `json:"status"`
	SendMs   *int64           `json:"send_ms"`
	RecvMs   *int64           `json:"recv_ms"`
	Snapshot *CaptureSnapshot `json:"snapshot"`
}

// ReportBackendCapture is the account's backend capture record.
type ReportBackendCapture struct {
	Method    string            `json:"method"`
	Collector map[string]string `json:"collector"`
	Start     ReportCapture     `json:"start"`
	End       ReportCapture     `json:"end"`
}
