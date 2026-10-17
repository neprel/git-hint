package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/qualify"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/suites"
	"github.com/neprel/git-hint/internal/store"
)

// Arms that label a run for comparisons.
var Arms = []string{"baseline", "configuration", "no_change", "context_only", "refactor"}

// RunOptions of a suite run.
type SuiteOptions struct {
	Evals    []string // eval IDs or versions; empty: every validated eval
	Agent    string   // opencode (live), oracle or nop (checks without a model)
	Attempts int
	Arm      string
	Live     *runner.Live // LAN model access for live agents (nil for oracle/nop)
}

// SuiteReport counts a suite run.
type SuiteReport struct {
	Suite    string         `json:"suite"`
	Skipped  []string       `json:"skipped,omitempty"`
	Trials   int            `json:"trials"`
	Outcomes map[string]int `json:"outcomes"`
	Stopped  string         `json:"stopped,omitempty"`
	Requests *int64         `json:"model_requests"`
}

// RunSuite freezes a suite and runs bounded trials. Every attempt is recorded.
func RunSuite(ctx context.Context, s *store.Store, r qualify.Runner, runnerVersion string, o SuiteOptions, now time.Time) (SuiteReport, error) {
	rep := SuiteReport{Outcomes: map[string]int{}}
	p := s.Cfg.Evals
	if err := p.Check(evals.OpExecute, ""); err != nil {
		return rep, err
	}
	if o.Agent == "opencode" {
		if err := p.Check(evals.OpModel, ""); err != nil {
			return rep, fmt.Errorf("a live agent needs the local model in the eval policy: %w", err)
		}
		if o.Live == nil {
			return rep, fmt.Errorf("a live agent needs the counting model proxy")
		}
	}
	if o.Attempts <= 0 {
		o.Attempts = 1
	}
	if o.Arm == "" {
		o.Arm = "baseline"
	}
	okArm := false
	for _, a := range Arms {
		okArm = okArm || a == o.Arm
	}
	if !okArm {
		return rep, fmt.Errorf("unknown arm %q", o.Arm)
	}
	lim := p.Limits()
	l := library.Open(s, lim.QuotaBytes)
	suite, skipped, err := suites.Freeze(l, o.Evals, now)
	rep.Suite, rep.Skipped = suite, skipped
	if err != nil {
		return rep, err
	}
	var sr map[string]any
	if err := l.Manifest("suite", suite, &sr); err != nil {
		return rep, err
	}
	h := sha256.Sum256([]byte("suite\x00" + suite + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	job := "jb-" + hex.EncodeToString(h[:8])
	if err := l.StartJob(job, "trials", library.Budget{Trials: lim.Trials, Requests: lim.TrialRequests * lim.Trials, Seconds: lim.Trials * lim.TrialSeconds}); err != nil {
		return rep, err
	}
	defer l.EndJob(job, "done")
	settings := runner.DefaultSettings
	settings.AgentSeconds, settings.VerifierSeconds = lim.TrialSeconds, lim.TrialSeconds
	var total int64
	totalKnown := o.Agent != "opencode" // no model requests for oracle and nop
	for _, m := range sr["members"].([]any) {
		ref := m.(map[string]any)["eval"].(string)
		var rec map[string]any
		if l.Manifest("eval", ref, &rec) != nil {
			rep.Outcomes["missing_eval"]++
			continue
		}
		for a := 1; a <= o.Attempts; a++ {
			st, _ := l.Job(job)
			if ok, why := st.Remaining(); !ok {
				rep.Stopped = why
				break
			}
			work := s.Path("cache", "evals", "work", strings.ReplaceAll(ref, "@", "-")+"-"+job+fmt.Sprintf("-%d", a))
			os.RemoveAll(work)
			if err := os.MkdirAll(work, 0o700); err != nil {
				return rep, err
			}
			mode := runner.Offline
			if o.Live != nil {
				mode = runner.LANModel
			}
			settings.Mode, settings.LANHost = mode, ""
			if o.Live != nil {
				settings.LANHost = o.Live.ContainerHost
			}
			if _, err := runner.Export(l, rec, work+"/task", settings); err != nil {
				return rep, err
			}
			ro := runner.RunOptions{Agent: o.Agent, JobsDir: work + "/jobs", JobName: "trial"}
			var counter *runner.Counter
			if o.Live != nil {
				counter = o.Live.NewTrial(lim.TrialRequests)
				ro.AgentArgs, ro.Env, ro.Model = o.Live.AgentArgs(counter), o.Live.AgentEnv(counter), o.Live.Model()
			}
			started := time.Now()
			trials, err := r.Run(ctx, work+"/task", ro)
			if err != nil || len(trials) == 0 {
				trials = []runner.Trial{{Name: "trial", Exception: "RunnerError", Checks: map[string]string{}}}
			}
			in := suites.TrialInput{Suite: suite, Eval: ref, Attempt: a, Arm: o.Arm, Agent: o.Agent, RunnerVersion: runnerVersion,
				Started: started, MaxSeconds: lim.TrialSeconds}
			var reqs, retries int64
			if counter != nil {
				reqs, retries = counter.Requests(), counter.Retries()
				in.Requests, in.Retries, in.MaxRequests = &reqs, &retries, lim.TrialRequests
				in.AgentVersion, in.Model = o.Live.AgentVersion, o.Live.ModelLabel
				if counter.LimitHit() {
					in.Stop = "request_limit"
				}
				total += reqs
			} else {
				zero := int64(0)
				in.Requests = &zero
			}
			if _, err := suites.RecordTrial(l, in, trials[0]); err != nil {
				return rep, err
			}
			rep.Trials++
			rep.Outcomes[trials[0].Outcome()]++
			_ = l.Consume(job, int(reqs), int(retries), 1, time.Since(started).Seconds())
			os.RemoveAll(work)
		}
		if rep.Stopped != "" {
			break
		}
	}
	if totalKnown || o.Live != nil {
		rep.Requests = &total
	}
	return rep, nil
}

// Row is one group of trials in a comparison (raw counters only; statistical comparison
// and derived effort metrics are not computed locally).
type Row struct {
	Suite        string   `json:"suite"`
	Arm          string   `json:"arm"`
	Agent        string   `json:"agent"`
	Trials       int      `json:"trials"`
	Passed       int      `json:"passed"`
	Failed       int      `json:"failed"`
	Timeouts     int      `json:"timeouts"`
	Errors       int      `json:"errors"`
	Budget       int      `json:"budget_exhausted"`
	Requests     *int64   `json:"model_requests"`
	InputTokens  *int64   `json:"input_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	MedianWall   *float64 `json:"median_wall_seconds"`
	UnknownUsage int      `json:"trials_with_unknown_usage"`
}

// Compare groups the stored trials (all of them: failed and timed-out attempts count).
func Compare(l *library.Library, suite string) ([]Row, error) {
	names, err := l.Manifests("trial")
	if err != nil {
		return nil, err
	}
	type key struct{ suite, arm, agent string }
	groups := map[key]*Row{}
	walls := map[key][]float64{}
	for _, n := range names {
		var t map[string]any
		if l.Manifest("trial", n, &t) != nil {
			continue
		}
		if suite != "" && t["suite_id"] != suite {
			continue
		}
		ag := t["agent"].(map[string]any)
		label := fmt.Sprint(ag["name"])
		if m, ok := ag["model"]; ok {
			label += "/" + fmt.Sprint(m)
		}
		k := key{t["suite_id"].(string), t["arm"].(string), label}
		r := groups[k]
		if r == nil {
			zero := int64(0)
			z1, z2 := zero, zero
			r = &Row{Suite: k.suite, Arm: k.arm, Agent: k.agent, Requests: new(int64), InputTokens: &z1, OutputTokens: &z2}
			groups[k] = r
		}
		r.Trials++
		switch t["outcome"] {
		case "passed":
			r.Passed++
		case "failed":
			r.Failed++
		case "timeout":
			r.Timeouts++
		case "budget_exhausted":
			r.Budget++
		default:
			r.Errors++
		}
		c := t["counters"].(map[string]any)
		add := func(dst **int64, v any) {
			if *dst == nil {
				return
			}
			f, ok := v.(float64)
			if !ok {
				*dst = nil // unknown is not zero: the sum becomes unknown
				return
			}
			**dst += int64(f)
		}
		if c["input_tokens"] == nil || c["output_tokens"] == nil {
			r.UnknownUsage++
		}
		add(&r.Requests, c["requests"])
		add(&r.InputTokens, c["input_tokens"])
		add(&r.OutputTokens, c["output_tokens"])
		if w, ok := c["wall_seconds"].(float64); ok {
			walls[k] = append(walls[k], w)
		}
	}
	var out []Row
	for k, r := range groups {
		if ws := walls[k]; len(ws) > 0 {
			sort.Float64s(ws)
			m := ws[len(ws)/2]
			if len(ws)%2 == 0 {
				m = (ws[len(ws)/2-1] + ws[len(ws)/2]) / 2
			}
			r.MedianWall = &m
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Suite+out[i].Arm+out[i].Agent < out[j].Suite+out[j].Arm+out[j].Agent
	})
	return out, nil
}
