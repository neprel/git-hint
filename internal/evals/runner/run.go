package runner

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SupportedHarbor is the tested Harbor version prefix.
const SupportedHarbor = "0.23."

// ErrUnavailable is returned when Harbor or Docker cannot run; nothing falls back to the host.
var ErrUnavailable = errors.New("the eval runner is unavailable")

// Harbor is the external runner.
type Harbor struct {
	Bin     string // harbor executable
	Version string
}

var versionRe = regexp.MustCompile(`([0-9]+\.[0-9]+\.[0-9]+)`)

// FindHarbor locates Harbor (HINT_HARBOR, else PATH) and checks its version, Docker and
// Docker buildx.
func FindHarbor(ctx context.Context) (*Harbor, error) {
	bin := os.Getenv("HINT_HARBOR")
	if bin == "" {
		p, err := exec.LookPath("harbor")
		if err != nil {
			return nil, fmt.Errorf("%w: harbor is not installed (uv tool install harbor==0.23.0)", ErrUnavailable)
		}
		bin = p
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("%w: harbor --version failed", ErrUnavailable)
	}
	v := versionRe.FindString(string(out))
	if !strings.HasPrefix(v, SupportedHarbor) {
		return nil, fmt.Errorf("%w: Harbor %q is not a tested version (%sx)", ErrUnavailable, v, SupportedHarbor)
	}
	if exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run() != nil {
		return nil, fmt.Errorf("%w: Docker is not running", ErrUnavailable)
	}
	// Harbor builds its network-control sidecar (the verifier has no network) with buildx
	if exec.CommandContext(ctx, "docker", "buildx", "version").Run() != nil {
		return nil, fmt.Errorf("%w: Docker buildx is not installed (Harbor needs it to cut the verifier's network)", ErrUnavailable)
	}
	return &Harbor{Bin: bin, Version: v}, nil
}

// Trial is one Harbor trial as HINT reads it.
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

// RunOptions of one Harbor job.
type RunOptions struct {
	Agent     string // nop, oracle, opencode
	Attempts  int
	JobsDir   string
	JobName   string
	AgentArgs []string // extra --ak values (opencode version, model config)
	Model     string   // provider/model for live agents
	Env       []string // extra environment for the runner (never credentials in arguments)
	Timeout   time.Duration
	// the container runner's live attempts: the model proxy and this attempt's counter
	Live     *Live
	Counter  *Counter
	OpenCode *OpenCodeSource // the pinned OpenCode of a live opencode attempt
	// agentCmd is the agent command of the internal test agent ("test-agent"): the opencode
	// installation is not wired into the container runner yet
	agentCmd []string
}

// Run executes one job on a task directory and reads its trials.
func (h *Harbor) Run(ctx context.Context, taskDir string, o RunOptions) ([]Trial, error) {
	if o.Attempts <= 0 {
		o.Attempts = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Hour
	}
	switch o.Agent {
	case "nop", "oracle", "opencode":
	default:
		return nil, fmt.Errorf("agent %q is not allowed", o.Agent)
	}
	work, err := os.MkdirTemp("", "hint-harbor-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	agent, env := o.Agent, o.Env
	if o.Agent == "opencode" && openCodeV2(o.AgentArgs) {
		// OpenCode v2 runs through HINT's Harbor agent class (pinned, hash-checked binary)
		if err := os.WriteFile(filepath.Join(work, "hint_opencode_v2.py"), openCodeV2Adapter, 0o600); err != nil {
			return nil, err
		}
		agent, env = "hint_opencode_v2:OpenCodeV2", append(append([]string{}, env...), "PYTHONPATH="+work)
	}
	args := []string{"run", "-p", taskDir, "-a", agent, "-k", fmt.Sprint(o.Attempts), "-n", "1",
		"-o", o.JobsDir, "--job-name", o.JobName, "-e", "docker", "-q", "-y"}
	for _, a := range o.AgentArgs {
		args = append(args, "--ak", a)
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Bin, args...)
	cmd.Dir = work // Harbor loads ./.env.local: keep the working directory empty
	cmd.Env = append(baseEnv(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	trials, perr := ReadJob(filepath.Join(o.JobsDir, o.JobName))
	if perr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("%w: harbor run failed and wrote no results", ErrUnavailable)
		}
		return nil, perr
	}
	return trials, nil
}

//go:embed hint_opencode_v2.py
var openCodeV2Adapter []byte

// openCodeV2 reports whether the agent arguments pin an OpenCode 2.x release.
func openCodeV2(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "version=2.") {
			return true
		}
	}
	return false
}

// baseEnv is the runner environment: PATH, HOME and the Docker settings only, with
// telemetry and remote price maps off.
func baseEnv() []string {
	env := []string{"HARBOR_TELEMETRY=0", "LITELLM_LOCAL_MODEL_COST_MAP=True", "DO_NOT_TRACK=1"}
	for _, k := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
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
