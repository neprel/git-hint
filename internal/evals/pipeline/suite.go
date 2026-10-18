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
	"github.com/neprel/git-hint/internal/evals/workunits"
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
	Kept     int            `json:"run_dirs_kept,omitempty"` // results that could not be archived stay on disk
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
	// a variant arm runs only variants of that arm; other arms run only baselines (a label
	// alone is not an intervention)
	bind := map[string]suites.TrialInput{}
	for _, m := range sr["members"].([]any) {
		ref := m.(map[string]any)["eval"].(string)
		var rec map[string]any
		if l.Manifest("eval", ref, &rec) != nil {
			continue
		}
		d, err := l.ManifestDigest("eval", ref)
		if err != nil {
			return rep, err
		}
		b := suites.TrialInput{Baseline: ref, BaselineDigest: d, EvalDigest: d}
		b.Objective, _, _ = strings.Cut(ref, "@")
		arm := ""
		if v, _ := rec["variant"].(map[string]any); v != nil {
			arm = str(v["arm"])
			b.Baseline, b.BaselineDigest = str(v["of"]), str(v["of_digest"])
			b.Objective, _, _ = strings.Cut(b.Baseline, "@")
		}
		variantArm := o.Arm == "context_only" || o.Arm == "refactor"
		switch {
		case arm != "" && arm != o.Arm:
			return rep, fmt.Errorf("%s is a %s variant; it cannot run as arm %s", ref, arm, o.Arm)
		case arm == "" && variantArm:
			return rep, fmt.Errorf("arm %s needs a variant that changes the task (eval run --arm %s --with ...); %s is a baseline", o.Arm, o.Arm, ref)
		}
		bind[ref] = b
	}
	h := sha256.Sum256([]byte("suite\x00" + suite + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	job := "jb-" + hex.EncodeToString(h[:8])
	if err := l.StartJob(job, "trials", library.Budget{Trials: lim.Trials, Requests: lim.TrialRequests * lim.Trials, Seconds: lim.Trials * lim.TrialSeconds}); err != nil {
		return rep, err
	}
	defer l.EndJob(job, "done")
	// controlled work units (C7B-E CU5): the run manifest and an open unit for every planned
	// eligible objective are written before the first attempt
	if o.Agent == "opencode" && o.Live != nil {
		m := workunits.Manifest{RunID: job, Suite: suite, Agent: o.Agent, AgentVersion: o.Live.AgentVersion, Arm: o.Arm, Attempts: o.Attempts,
			RunnerVersion: runnerVersion, ModelLabel: o.Live.ModelLabel, MaxRequests: lim.TrialRequests, MaxSeconds: lim.TrialSeconds,
			LaunchDigest: runner.LaunchDigest(runner.RunOptions{Agent: o.Agent, AgentArgs: o.Live.AgentArgs(nil), Env: o.Live.AgentEnv(&runner.Counter{}), Model: o.Live.Model()}),
			CreatedAt:    now.UTC().Format(time.RFC3339)}
		for _, mm := range sr["members"].([]any) {
			ref := mm.(map[string]any)["eval"].(string)
			var rec map[string]any
			if l.Manifest("eval", ref, &rec) != nil {
				continue
			}
			b := bind[ref]
			m.Members = append(m.Members, workunits.Member{Eval: ref, Baseline: b.Baseline, Objective: b.Objective,
				TaskFamily: str(mm.(map[string]any)["task_family"]), Eligible: str(rec["family"]) == "refactor"})
		}
		if err := workunits.Start(s, l, m, now); err != nil {
			return rep, err
		}
	}
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
			b := bind[ref]
			in := suites.TrialInput{RunID: job, Suite: suite, Eval: ref, Attempt: a, Arm: o.Arm, Agent: o.Agent, RunnerVersion: runnerVersion,
				Started: started, MaxSeconds: lim.TrialSeconds, Objective: b.Objective, Baseline: b.Baseline, BaselineDigest: b.BaselineDigest, EvalDigest: b.EvalDigest,
				ContextPolicy: "disabled"} // HINT delivers no managed context in eval trials yet
			var reqs, retries int64
			if counter != nil {
				reqs, retries = counter.Requests(), counter.Retries()
				in.Requests, in.Retries, in.MaxRequests = &reqs, &retries, lim.TrialRequests
				in.AgentVersion, in.Model = o.Live.AgentVersion, o.Live.ModelLabel
				in.ModelCheck = counter.ModelCheck()
				// the launch HINT constructed and enforces (agent arguments, model selection,
				// environment names; the per-trial proxy URL and the dummy key value are not
				// configuration): one setting_override digest
				in.ObservedConfig = []map[string]string{{"kind": "setting_override", "digest": runner.LaunchDigest(ro)}}
				if counter.LimitHit() {
					in.Stop = "request_limit"
				}
				total += reqs
			} else {
				zero := int64(0)
				in.Requests = &zero
			}
			// the results are evidence: archive them before the work directory goes
			_ = l.RecordExternal(library.External{Kind: "job_dir", Name: work})
			archived, kerr := keepRun(l, work)
			if kerr == nil {
				in.Artifacts = []string{archived}
			}
			_ = l.Consume(job, int(reqs), int(retries), 1, time.Since(started).Seconds())
			name, err := suites.RecordTrial(l, in, trials[0])
			if err != nil {
				return rep, err // the work directory stays
			}
			if o.Agent == "opencode" && o.Live != nil {
				if err := workunits.Record(s, l, job, ref, name); err != nil {
					return rep, err
				}
			}
			rep.Trials++
			rep.Outcomes[trials[0].Outcome()]++
			if kerr != nil {
				rep.Kept++ // not archived (size or quota): the directory stays, listed in the external inventory
				continue
			}
			os.RemoveAll(work)
			_ = l.RecordExternal(library.External{Kind: "job_dir", Name: work, Removed: true})
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
	Objective    string   `json:"objective"` // baseline eval ID; arms pair only within one objective
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
	type key struct{ objective, suite, arm, agent string }
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
		obj, _ := t["objective"].(string)
		k := key{obj, t["suite_id"].(string), t["arm"].(string), label}
		r := groups[k]
		if r == nil {
			zero := int64(0)
			z1, z2 := zero, zero
			r = &Row{Objective: k.objective, Suite: k.suite, Arm: k.arm, Agent: k.agent, Requests: new(int64), InputTokens: &z1, OutputTokens: &z2}
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
		return out[i].Objective+out[i].Suite+out[i].Arm+out[i].Agent < out[j].Objective+out[j].Suite+out[j].Arm+out[j].Agent
	})
	return out, nil
}
