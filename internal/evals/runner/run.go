package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// ErrUnavailable is returned when the container engine cannot run; nothing falls back to
// the host.
var ErrUnavailable = errors.New("the eval runner is unavailable")

// Trial is one trial result as HINT reads it.
type Trial struct {
	Name         string            `json:"name"`
	Reward       *float64          `json:"reward"`
	Exception    string            `json:"exception,omitempty"` // exception type only
	Tampered     bool              `json:"tampered,omitempty"`  // a check line appeared more than once
	Checks       map[string]string `json:"checks"`
	Tests        map[string]string `json:"tests,omitempty"` // "C1 file::Name" -> pass or fail
	InputTokens  *int64            `json:"input_tokens"`
	OutputTokens *int64            `json:"output_tokens"`
	CacheTokens  *int64            `json:"cache_tokens"`
	WallSeconds  *float64          `json:"wall_seconds"`
	AgentSeconds *float64          `json:"agent_seconds"`
	// the runner's trust statement (hint_container): unknown when absent
	ResultTrust  string            `json:"result_trust"`
	CheckTrust   map[string]string `json:"check_trust,omitempty"`
	TrustInvalid bool              `json:"trust_invalid,omitempty"` // an invalid statement: never a pass
}

// Outcome classifies a trial for HINT records.
func (t Trial) Outcome() string {
	switch {
	case t.Tampered, t.TrustInvalid:
		return "verifier_error"
	case t.Exception != "" && strings.Contains(strings.ToLower(t.Exception), "timeout"):
		return "timeout"
	case t.Exception != "" && strings.Contains(t.Exception, "Reward"):
		return "verifier_error"
	case t.Exception != "":
		return "infrastructure_error"
	case t.Reward == nil:
		return "verifier_error"
	case *t.Reward >= 1:
		// a pass needs the verifier's own check lines, all passing: a reward file written
		// by other code is not a pass
		if len(t.Checks) == 0 {
			return "verifier_error"
		}
		for _, st := range t.Checks {
			if st != "pass" {
				return "verifier_error"
			}
		}
		for k, st := range t.Tests {
			if st != "pass" && t.Checks[strings.Fields(k)[0]] == "pass" {
				return "verifier_error" // a passing check with a failed required test
			}
		}
		return "passed"
	}
	return "failed"
}

// RunOptions of one runner job.
type RunOptions struct {
	StudyLaunchPolicy StudyLaunchPolicy
	// ExpectLaunchDigest is the frozen planned A; required by a study launch only.
	ExpectLaunchDigest string
	// BeforeAgentStart, when set, is called once after every prestart check passed and
	// immediately before the agent container's start is requested (study launch only). It
	// records a prestart intent, never a confirmed start; an error refuses the attempt before
	// the launch. Nil keeps the runner's behavior unchanged.
	BeforeAgentStart func(LaunchFacts) error
	// CapturePrelaunchTree opts controlled studies into local observed-start evidence.
	// It does not enable a trusted producer, export source, or change launch policy.
	CapturePrelaunchTree bool
	Agent                string // nop, oracle, opencode
	Attempts             int
	JobsDir              string
	JobName              string
	Env                  []string // the agent's environment for a live attempt (never credentials)
	Timeout              time.Duration
	// the container runner's live attempts: the model proxy and this attempt's counter
	Live     *Live
	Counter  *Counter
	OpenCode *OpenCodeSource // the pinned OpenCode of a live opencode attempt
	Ripgrep  *RipgrepSource  // explicit offline study runtime only; nil preserves the old recipe
	// ExpectTarget is the OpenCode target frozen in the run's configuration; the attempt
	// refuses another
	ExpectTarget string
	// agentCmd is the agent command of the internal test agent ("test-agent"): the opencode
	// installation is not wired into the container runner yet
	agentCmd []string
	// testDigest is the execution digest that agent_run records for a test-agent attempt (tests);
	// an opencode attempt computes its own
	testDigest string
	// capture and captureRef enable the backend captures of a live attempt (both needed; no public
	// option sets them): the transport of the read-only inspect and the derivation of the
	// references that replace raw backend identifiers
	capture    captureTransport
	captureRef func(kind, raw string) string
	// ExpectedBackend requires an exact complete start snapshot immediately before launch.
	// Nil preserves the existing capture behavior; it proves no health or loaded weights.
	ExpectedBackend *CaptureSnapshot
	// BackendCapture and CaptureKey are the explicit opt-in of the controlled research setup to
	// the real capture transport (both needed; never a default, no CLI flag or configuration
	// field sets them): the references replacing raw backend identifiers are derived with
	// CaptureKey
	BackendCapture *SSHCapture
	CaptureKey     []byte
}

// captureOptions is the attempt's capture transport and reference derivation, or nil: the real
// transport only with both opt-in fields, otherwise the unexported test fields.
func (o RunOptions) captureOptions() (captureTransport, func(kind, raw string) string) {
	if o.BackendCapture != nil && len(o.CaptureKey) > 0 {
		key := bytes.Clone(o.CaptureKey)
		return o.BackendCapture, func(kind, raw string) string { return hint.DeriveID(key, kind, raw) }
	}
	return o.capture, o.captureRef
}

// check IDs: C<n> (generated verifier) and B<n> (process-separated behavior checks, B1)
var checkLine = regexp.MustCompile(`^CHECK ([CB][1-9][0-9]{0,2}) (PASS|FAIL)$`)

var testLine = regexp.MustCompile(`^TEST ([CB][1-9][0-9]{0,2}) ([A-Za-z0-9._/+-]+::[A-Za-z_][A-Za-z0-9_]*) (PASS|FAIL)$`)

// ReadJob reads every trial of a job directory.
func ReadJob(jobDir string) ([]Trial, error) {
	ms, err := filepath.Glob(filepath.Join(jobDir, "*", "result.json"))
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("no trial results in the job directory")
	}
	sort.Strings(ms)
	var out []Trial
	for _, m := range ms {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var r struct {
			TrialName      string `json:"trial_name"`
			VerifierResult *struct {
				Rewards map[string]float64 `json:"rewards"`
			} `json:"verifier_result"`
			ExceptionInfo *struct {
				ExceptionType string `json:"exception_type"`
			} `json:"exception_info"`
			AgentResult *struct {
				NInput  *int64 `json:"n_input_tokens"`
				NOutput *int64 `json:"n_output_tokens"`
				NCache  *int64 `json:"n_cache_tokens"`
			} `json:"agent_result"`
			StartedAt      string `json:"started_at"`
			FinishedAt     string `json:"finished_at"`
			AgentExecution *struct {
				StartedAt  string `json:"started_at"`
				FinishedAt string `json:"finished_at"`
			} `json:"agent_execution"`
			Container json.RawMessage `json:"hint_container"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unreadable trial result")
		}
		t := Trial{Name: r.TrialName, Checks: map[string]string{}, Tests: map[string]string{}}
		if rt, ct, err := readTrust(r.Container); err != nil {
			t.ResultTrust, t.TrustInvalid = TrustUnknown, true
		} else {
			t.ResultTrust, t.CheckTrust = rt, ct
		}
		if r.VerifierResult != nil {
			if v, ok := r.VerifierResult.Rewards["reward"]; ok {
				t.Reward = &v
			}
		}
		if r.ExceptionInfo != nil {
			t.Exception = r.ExceptionInfo.ExceptionType
			if t.Exception == "" {
				t.Exception = "unknown"
			}
		}
		if r.AgentResult != nil {
			t.InputTokens, t.OutputTokens, t.CacheTokens = r.AgentResult.NInput, r.AgentResult.NOutput, r.AgentResult.NCache
		}
		t.WallSeconds = span(r.StartedAt, r.FinishedAt)
		if r.AgentExecution != nil {
			t.AgentSeconds = span(r.AgentExecution.StartedAt, r.AgentExecution.FinishedAt)
		}
		if f, err := os.Open(filepath.Join(filepath.Dir(m), "verifier", "test-stdout.txt")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if c := checkLine.FindStringSubmatch(strings.TrimSpace(sc.Text())); c != nil {
					if _, dup := t.Checks[c[1]]; dup {
						t.Tampered = true // the generated verifier prints each check once
					}
					t.Checks[c[1]] = strings.ToLower(c[2])
				}
				if c := testLine.FindStringSubmatch(strings.TrimSpace(sc.Text())); c != nil {
					k := c[1] + " " + c[2]
					if _, dup := t.Tests[k]; dup {
						t.Tampered = true // the generated verifier prints each test once
					}
					t.Tests[k] = strings.ToLower(c[3])
				}
			}
			f.Close()
		}
		out = append(out, t)
	}
	return out, nil
}

func span(a, b string) *float64 {
	ta, e1 := time.Parse(time.RFC3339Nano, a)
	tb, e2 := time.Parse(time.RFC3339Nano, b)
	if e1 != nil || e2 != nil {
		return nil
	}
	d := tb.Sub(ta).Seconds()
	return &d
}
