// Package suites freezes sets of qualified eval versions and records trials.
//
//   - Task family: episodes linked by depends_on, continues or duplicate_of form one family;
//     a family keeps its evals together (one ID, one partition).
//   - Partition: held_out or calibration, decided by the family ID alone (HeldOutPercent of
//     families are held out), so related tasks never cross the boundary.
//   - A suite names immutable eval versions; later versions do not change it.
//   - Every trial is kept, including failures, timeouts and cancellations, with its
//     consumed resources.
package suites

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
)

// HeldOutPercent of task families go to the held-out partition.
const HeldOutPercent = 30

// Families maps every stored episode to its task family ID.
func Families(l *library.Library) (map[string]string, error) {
	names, err := l.Manifests("episode")
	if err != nil {
		return nil, err
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		r := find(parent[x])
		parent[x] = r
		return r
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	for _, n := range names {
		var ep map[string]any
		if l.Manifest("episode", n, &ep) != nil {
			continue
		}
		find(n)
		links, _ := ep["links"].([]any)
		for _, x := range links {
			m, _ := x.(map[string]any)
			if other, _ := m["episode_id"].(string); other != "" {
				union(n, other)
			}
		}
	}
	// revisions of one proposal belong together too (episodes/<base> -> current)
	idx, _ := l.Indexes("episodes")
	for _, i := range idx {
		var cur struct{ Current string }
		if l.Index(i, &cur) == nil && cur.Current != "" {
			union(strings.TrimPrefix(i, "episodes/"), cur.Current)
		}
	}
	out := map[string]string{}
	for n := range parent {
		h := sha256.Sum256([]byte("family\x00" + find(n)))
		out[n] = "tf-" + hex.EncodeToString(h[:8])
	}
	return out, nil
}

// Partition returns the partition of a task family.
func Partition(family string) string {
	h := sha256.Sum256([]byte("partition\x00" + family))
	if int(binary.BigEndian.Uint16(h[:2])) < HeldOutPercent*65536/100 { // no modulo bias
		return "held_out"
	}
	return "calibration"
}

// Latest returns the latest version of every eval (ref -> record).
func Latest(l *library.Library) (map[string]map[string]any, error) {
	names, err := l.Manifests("eval")
	if err != nil {
		return nil, err
	}
	best := map[string]map[string]any{}
	for _, n := range names {
		var rec map[string]any
		if l.Manifest("eval", n, &rec) != nil {
			continue
		}
		id := rec["eval_id"].(string)
		if cur, ok := best[id]; !ok || rec["version"].(float64) > cur["version"].(float64) {
			best[id] = rec
		}
	}
	out := map[string]map[string]any{}
	for id, rec := range best {
		out[fmt.Sprintf("%s@%d", id, int(rec["version"].(float64)))] = rec
	}
	return out, nil
}

// ErrEmpty is returned when no qualified eval can enter a suite.
var ErrEmpty = errors.New("no validated eval to run (git hint eval lists the states)")

// Freeze stores a suite of the given eval IDs or versions (all validated latest versions
// when none are given). Only validated versions enter; others are reported.
func Freeze(l *library.Library, want []string, now time.Time) (string, []string, error) {
	latest, err := Latest(l)
	if err != nil {
		return "", nil, err
	}
	fams, err := Families(l)
	if err != nil {
		return "", nil, err
	}
	pick := map[string]bool{}
	for _, w := range want {
		pick[w] = true
	}
	var members []any
	var skipped []string
	refs := make([]string, 0, len(latest))
	for r := range latest {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		rec := latest[ref]
		id, _, _ := strings.Cut(ref, "@")
		if len(want) > 0 && !pick[ref] && !pick[id] {
			continue
		}
		if v, _ := rec["variant"].(map[string]any); v != nil && len(want) == 0 {
			continue // a variant enters a suite only when named (eval run --with)
		}
		if rec["state"] != "validated" {
			skipped = append(skipped, ref+": "+fmt.Sprint(rec["state"]))
			continue
		}
		fam := fams[str(rec["episode_id"])]
		if fam == "" {
			h := sha256.Sum256([]byte("family\x00" + str(rec["episode_id"])))
			fam = "tf-" + hex.EncodeToString(h[:8])
		}
		members = append(members, map[string]any{"eval": ref, "task_family": fam, "partition": Partition(fam)})
	}
	if len(members) == 0 {
		return "", skipped, ErrEmpty
	}
	h := sha256.Sum256([]byte(fmt.Sprint(members)))
	suite := map[string]any{"record": "suite", "manifest_version": 1, "suite_id": "su-" + hex.EncodeToString(h[:8]),
		"created_at": now.UTC().Format(time.RFC3339), "qualification_policy": "q3", "members": members}
	var old map[string]any
	if l.Manifest("suite", suite["suite_id"].(string), &old) == nil {
		return suite["suite_id"].(string), skipped, nil // the same set is the same frozen suite
	}
	name, err := l.PutManifest("suite", suite)
	return name, skipped, err
}

func str(v any) string { s, _ := v.(string); return s }

// TrialInput is everything a trial record holds besides the runner result.
type TrialInput struct {
	Suite, Eval   string
	Attempt       int
	Arm           string // baseline, no_change, context_only, refactor, configuration
	Agent         string
	AgentVersion  string
	Model         string
	RunnerVersion string
	Started       time.Time
	MaxRequests   int
	MaxSeconds    int
	Requests      *int64 // counted at HINT's proxy; nil when not observed
	Retries       *int64
	Stop          string   // completed, request_limit, time_limit, cancelled, error
	Artifacts     []string // archived run results (library objects)
	// Objective is the baseline eval ID; Baseline, BaselineDigest and EvalDigest bind the
	// immutable versions (the eval that ran is the baseline or its variant)
	Objective, Baseline, BaselineDigest, EvalDigest string
	// ContextPolicy is the HINT context policy state of the run ("" = unknown) and, when
	// enabled, its identity; DeclaredChanges are the controls the arm changes on purpose
	ContextPolicy                        string
	ContextPolicyID, ContextPolicyDigest string
	ContextPolicyVersion                 int
	DeclaredChanges                      []string
	// RunID is the run (job) whose manifest planned this trial (immutable binding)
	RunID string
	// PlanBinding is the frozen plan binding of the variant that runs (nil: unbound)
	PlanBinding map[string]any
	// ModelCheck is the proxy's check of the model field of every forwarded request
	ModelCheck string
	// RunnerName is the runner that ran the trial: hint-docker (empty: hint-docker; stored
	// harbor trials stay readable, but no new one is written)
	RunnerName string
	// ObservedConfig are digests of the effective launch configuration (kind, sha256 digest);
	// nil: not observed (unknown), empty: observed none
	ObservedConfig []map[string]string
}

// RecordTrial stores one trial (every attempt, whatever its outcome).
func RecordTrial(l *library.Library, in TrialInput, t runner.Trial) (string, error) {
	switch in.RunnerName {
	case "":
		in.RunnerName = "hint-docker"
	case "hint-docker":
	default: // stored harbor trials stay readable; no new one is written
		return "", fmt.Errorf("a new trial needs the built-in runner, not %q", in.RunnerName)
	}
	h := sha256.Sum256([]byte(strings.Join([]string{in.Suite, in.Eval, fmt.Sprint(in.Attempt), in.Arm, in.Agent, in.Model, in.Started.UTC().Format(time.RFC3339Nano), t.Name}, "\x00")))
	checks := []any{}
	ids := make([]string, 0, len(t.Checks))
	for id := range t.Checks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		checks = append(checks, map[string]any{"check_id": id, "status": t.Checks[id]})
	}
	count := func(p *int64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	sec := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	agent := map[string]any{"name": in.Agent}
	if in.AgentVersion != "" {
		agent["version"] = in.AgentVersion
	}
	if in.Model != "" {
		agent["model"] = in.Model
	}
	if in.ModelCheck != "" {
		agent["model_check"] = in.ModelCheck
	}
	outcome := t.Outcome()
	stop := in.Stop
	if stop == "" {
		switch outcome {
		case "timeout":
			stop = "time_limit"
		case "infrastructure_error", "verifier_error":
			stop = "error"
		default:
			stop = "completed"
		}
	}
	if stop == "request_limit" {
		outcome = "budget_exhausted"
	}
	budget := map[string]any{"max_seconds": in.MaxSeconds, "stop_reason": stop}
	if in.MaxRequests > 0 {
		budget["max_requests"] = in.MaxRequests
	}
	rec := map[string]any{
		"record": "trial", "manifest_version": 1, "trial_id": "tr-" + hex.EncodeToString(h[:8]), "suite_id": in.Suite, "eval": in.Eval,
		"attempt": in.Attempt, "arm": in.Arm, "agent": agent, "runner": map[string]any{"name": in.RunnerName, "version": in.RunnerVersion},
		"started_at": in.Started.UTC().Format(time.RFC3339), "outcome": outcome, "checks": checks,
		"counters": map[string]any{"requests": count(in.Requests), "retries": count(in.Retries), "input_tokens": count(t.InputTokens),
			"output_tokens": count(t.OutputTokens), "cache_tokens": count(t.CacheTokens), "wall_seconds": sec(t.WallSeconds), "agent_seconds": sec(t.AgentSeconds)},
		"budget": budget,
	}
	// the runner's trust over the eval's exact required checks (no caller input)
	var ev map[string]any
	if l.Manifest("eval", in.Eval, &ev) == nil {
		rec["result_trust"] = t.TrustOver(runner.RequiredChecks(ev))
	} else {
		rec["result_trust"] = runner.TrustUnknown
	}
	if in.RunID != "" {
		rec["run_id"] = in.RunID
	}
	if in.PlanBinding != nil {
		rec["plan_binding"] = in.PlanBinding
	}
	if in.Objective != "" {
		rec["objective"] = in.Objective
		rec["binding"] = map[string]any{"baseline": in.Baseline, "baseline_digest": in.BaselineDigest, "eval_digest": in.EvalDigest}
	}
	cp := map[string]any{"state": "unknown"}
	switch in.ContextPolicy {
	case "enabled":
		cp = map[string]any{"state": "enabled", "id": in.ContextPolicyID, "version": in.ContextPolicyVersion, "digest": in.ContextPolicyDigest}
	case "disabled", "not_applicable":
		cp = map[string]any{"state": in.ContextPolicy}
	}
	rec["context_policy"] = cp
	if in.ObservedConfig != nil {
		oc := []any{}
		for _, c := range in.ObservedConfig {
			oc = append(oc, map[string]any{"kind": c["kind"], "digest": c["digest"]})
		}
		rec["observed_config"] = oc
	}
	if len(in.DeclaredChanges) > 0 {
		dc := []any{}
		for _, c := range in.DeclaredChanges {
			dc = append(dc, c)
		}
		rec["declared_changes"] = dc
	}
	if len(in.Artifacts) > 0 {
		arts := []any{}
		for _, a := range in.Artifacts {
			arts = append(arts, a)
		}
		rec["artifacts"] = arts
	}
	return l.PutManifest("trial", rec)
}
