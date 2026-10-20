// Package report builds and checks minimized eval result reports (evalreport v0.2; Check
// also reads v0.1).
//
// A report carries only allowlisted fields of trial records. Every local identifier and
// model name becomes a project-scoped opaque reference (HMAC with the project key), times
// have hour resolution, and nothing from task content, code, paths, commands, logs or
// content digests enters it. The same Check runs before a file is written and before an
// upload, and the server applies the same schema.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/version"
)

// Version of the report format that Build writes. Check reads 0.1 and 0.2.
const Version = "0.2.1"

// MediaType of a report on the wire.
const MediaType = "application/vnd.hint-evalreport+json"

var (
	once    sync.Once
	schemas map[string]*jsonschema.Schema // "0.1", "0.2"
	errLoad error
)

func compiled() (map[string]*jsonschema.Schema, error) {
	once.Do(func() {
		schemas = map[string]*jsonschema.Schema{}
		for v, raw := range map[string][]byte{"0.1": githint.SchemaEvalReportV01, "0.2": githint.SchemaEvalReportV02} {
			c := jsonschema.NewCompiler()
			c.DefaultDraft(jsonschema.Draft2020)
			c.AssertFormat()
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				errLoad = err
				return
			}
			id := "urn:hint:schema:evalreport:v" + v + ":report"
			if err = c.AddResource(id, doc); err != nil {
				errLoad = err
				return
			}
			if schemas[v], errLoad = c.Compile(id); errLoad != nil {
				return
			}
		}
	})
	return schemas, errLoad
}

// reportMinor returns "0.1" or "0.2" for a supported report version, "" otherwise.
func reportMinor(v string) string {
	switch {
	case v == "0.1.0":
		return "0.1"
	case regexp.MustCompile(`^0\.2\.(0|[1-9][0-9]*)$`).MatchString(v):
		return "0.2"
	}
	return ""
}

// ErrInvalid is returned by Check.
var ErrInvalid = errors.New("the eval report is not valid")

// Check validates a serialized report: strict JSON, the schema, the content digest and
// unique trial references. Messages hold field names only, never values.
func Check(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil || dec.More() {
		return fmt.Errorf("%w: not one JSON object", ErrInvalid)
	}
	v, _ := raw["report_version"].(string)
	minor := reportMinor(v)
	if minor == "" {
		return fmt.Errorf("%w: unsupported report version", ErrInvalid)
	}
	all, err := compiled()
	if err != nil {
		return err
	}
	s := all[minor]
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: not JSON", ErrInvalid)
	}
	if err := s.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return fmt.Errorf("%w: schema (%s)", ErrInvalid, firstPath(ve))
		}
		return fmt.Errorf("%w: schema", ErrInvalid)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	want, _ := m["content_digest"].(string)
	delete(m, "content_digest")
	got, err := hint.Digest(m)
	if err != nil || got != want {
		return fmt.Errorf("%w: content_digest does not match", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, t := range m["trials"].([]any) {
		r := t.(map[string]any)["trial_ref"].(string)
		if seen[r] {
			return fmt.Errorf("%w: duplicate trial_ref", ErrInvalid)
		}
		seen[r] = true
	}
	if err := trustProblem(m); err != nil {
		return err
	}
	return nil
}

// trustProblem checks result_trust (07B-J §11.4): from 0.2.1 every trial carries its own
// value and the qualification cap, and every attempt closure; a closure whose trial is in
// the report equals the minimum of the trial's value and its cap (never above either).
func trustProblem(m map[string]any) error {
	if v := str(m["report_version"]); reportMinor(v) != "0.2" || v == "0.2.0" {
		return nil // legacy: absent values read as unknown
	}
	trials := map[string]string{}
	for _, t := range m["trials"].([]any) {
		tm := t.(map[string]any)
		q, _ := tm["qualification"].(map[string]any)
		rt, ok1 := tm["result_trust"].(string)
		qt, ok2 := q["result_trust"].(string)
		if !ok1 || !ok2 {
			return fmt.Errorf("%w: trial without result_trust", ErrInvalid)
		}
		trials[tm["trial_ref"].(string)] = runner.MinTrust(rt, qt)
	}
	units, _ := m["work_units"].([]any)
	for _, u := range units {
		as, _ := u.(map[string]any)["attempts"].([]any)
		for _, a := range as {
			am := a.(map[string]any)
			c, _ := am["closure"].(map[string]any)
			ct, ok := c["result_trust"].(string)
			if !ok {
				return fmt.Errorf("%w: closure without result_trust", ErrInvalid)
			}
			if want, in := trials[str(am["trial_ref"])]; in && ct != want {
				return fmt.Errorf("%w: closure result_trust differs from its trial", ErrInvalid)
			}
		}
	}
	return nil
}

// qualTrust is the qualification trust of an eval record; absent (q1, q2) is unknown.
func qualTrust(ev map[string]any) string {
	q, _ := ev["qualification"].(map[string]any)
	return runner.MinTrust(str(q["result_trust"]))
}

func firstPath(ve *jsonschema.ValidationError) string {
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	return "/" + joinPath(ve.InstanceLocation)
}

func joinPath(p []string) string {
	out := ""
	for i, x := range p {
		if i > 0 {
			out += "/"
		}
		out += x
	}
	return out
}

// Builder derives references with the project key.
type Builder struct {
	Key       []byte
	Namespace string
	// Sessions is the declared-task evidence of agent sessions from the activity spool:
	// session pseudonym -> task ID -> observations ("" counts observations without a task).
	// AgentSources are the OpenCode adapter source IDs that derive the session pseudonyms.
	// Without them no trial gets a source task.
	Sessions     map[string]map[string]int
	AgentSources []string
	// WorkUnits are immutable work unit revisions to report (eval report v0.2 work_units).
	WorkUnits []any
}

// sourceTask is the declared task of an eval, or "" when it is not unambiguous (G11 binding
// rule): the eval's episode names exactly one source session; that session has exactly one
// episode; every adapter observation of the session carries the same declared task. Mixed,
// missing, conflicting or absent evidence gives "". Never by time, first or last event.
func (b Builder) sourceTask(l *library.Library, evalRef string, episodes []map[string]any) (string, string) {
	var ev map[string]any
	if l.Manifest("eval", evalRef, &ev) != nil {
		return "", ""
	}
	epID := str(ev["episode_id"])
	var ep map[string]any
	if epID == "" || l.Manifest("episode", epID, &ep) != nil {
		return "", epID
	}
	srcs, _ := ep["sources"].([]any)
	if len(srcs) != 1 {
		return "", epID
	}
	src, _ := srcs[0].(map[string]any)
	session := str(src["session"])
	if session == "" || src["agent"] != "opencode" {
		return "", epID
	}
	n := 0
	for _, other := range episodes {
		for _, x := range other["sources"].([]any) {
			if m, _ := x.(map[string]any); str(m["session"]) == session && m["agent"] == "opencode" {
				n++
			}
		}
	}
	if n != 1 {
		return "", epID
	}
	tasks := map[string]int{}
	for _, sid := range b.AgentSources {
		for t, c := range b.Sessions[hint.DeriveID(b.Key, "session", sid, session)] {
			tasks[t] += c
		}
	}
	if len(tasks) != 1 {
		return "", epID
	}
	for t := range tasks {
		if !hexRef.MatchString(t) {
			return "", epID
		}
		return t, epID
	}
	return "", epID
}

func (b Builder) ref(kind string, parts ...string) string {
	return hint.EvalRef(b.Key, kind, parts...)
}

var hexRef = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Build makes a report of the given trial records (names as stored). It returns the
// serialized report and the trial names it holds.
func (b Builder) Build(l *library.Library, trialNames []string, now time.Time) ([]byte, []string, error) {
	if !hexRef.MatchString(b.Namespace) || len(b.Key) == 0 {
		return nil, nil, errors.New("report builder without a project key")
	}
	sort.Strings(trialNames)
	var episodes []map[string]any
	if len(b.AgentSources) > 0 {
		names, _ := l.Manifests("episode")
		for _, n := range names {
			var ep map[string]any
			if l.Manifest("episode", n, &ep) == nil {
				episodes = append(episodes, ep)
			}
		}
	}
	suitesSeen := map[string]map[string]any{}
	var trials []any
	var used []string
	for _, n := range trialNames {
		var t map[string]any
		if err := l.Manifest("trial", n, &t); err != nil {
			continue
		}
		suite := str(t["suite_id"])
		sr, ok := suitesSeen[suite]
		if !ok {
			if l.Manifest("suite", suite, &sr) != nil {
				continue
			}
			suitesSeen[suite] = sr
		}
		evalRef := str(t["eval"])
		var member map[string]any
		for _, m := range sr["members"].([]any) {
			if mm := m.(map[string]any); mm["eval"] == evalRef {
				member = mm
			}
		}
		var ev map[string]any
		if member == nil || l.Manifest("eval", evalRef, &ev) != nil {
			continue
		}
		ag := t["agent"].(map[string]any)
		agent := map[string]any{"name": ag["name"]}
		if v, ok := ag["version"].(string); ok && semver.MatchString(v) {
			agent["version"] = v
		}
		if m, ok := ag["model"].(string); ok && m != "" {
			agent["model_ref"] = b.ref("model", m)
		}
		if mc, ok := ag["model_check"].(string); ok {
			agent["model_check"] = mc
		}
		var checks []any
		for _, c := range t["checks"].([]any) {
			cm := c.(map[string]any)
			checks = append(checks, map[string]any{"check_ref": b.ref("check", evalRef, str(cm["check_id"])), "status": cm["status"]})
		}
		if checks == nil {
			checks = []any{}
		}
		c := t["counters"].(map[string]any)
		counter := func(k string) any { return c[k] } // null stays null
		bud := t["budget"].(map[string]any)
		maxReq := any(nil)
		if v, ok := bud["max_requests"]; ok {
			maxReq = v
		}
		tr := map[string]any{
			"trial_ref": b.ref("trial", n), "suite_ref": b.ref("suite", suite), "eval_ref": b.ref("eval", evalRef),
			"task_family_ref": b.ref("family", str(member["task_family"])), "family": ev["family"], "partition": member["partition"],
			"arm": t["arm"], "qualification": map[string]any{"policy": sr["qualification_policy"], "state": ev["state"], "basis": "client_claim", "result_trust": qualTrust(ev)},
			"result_trust": runner.MinTrust(str(t["result_trust"])), // absent in older trials: unknown
			"attempt":      t["attempt"], "agent": agent, "runner": t["runner"], "outcome": t["outcome"], "checks": checks,
			"counters": map[string]any{"requests": counter("requests"), "retries": counter("retries"), "input_tokens": counter("input_tokens"),
				"output_tokens": counter("output_tokens"), "cache_tokens": counter("cache_tokens"), "wall_seconds": counter("wall_seconds"),
				"agent_seconds": counter("agent_seconds")},
			"budget": map[string]any{"max_requests": maxReq, "max_seconds": bud["max_seconds"], "stop_reason": bud["stop_reason"]},
		}
		// comparison binding (E14): the objective, the exact baseline version and the
		// environment, as project-scoped references only
		if obj := str(t["objective"]); obj != "" {
			bind, _ := t["binding"].(map[string]any)
			tr["objective_ref"] = b.ref("objective", obj)
			tr["baseline_ref"] = b.ref("eval", str(bind["baseline"]))
			tr["environment_ref"] = b.ref("environment", environmentIdentity(ev)...)
		}
		// the declared task of the eval (G11): through the exact baseline version for a
		// variant, which must keep the baseline's episode; the same opaque ID as the activity
		// correlation.task_id
		if len(b.AgentSources) > 0 {
			source := evalRef
			if bind, _ := t["binding"].(map[string]any); str(t["objective"]) != "" {
				source = str(bind["baseline"])
			}
			task, ep := b.sourceTask(l, source, episodes)
			if source != evalRef && str(ev["episode_id"]) != ep {
				task = "" // a variant with another episode than its baseline: conflict
			}
			if task != "" {
				tr["source_task_id"] = task
			}
		}
		// the context policy: the full identity becomes one project reference; the other
		// states stay distinct (a trial recorded before the field is unknown)
		cp := map[string]any{"state": "unknown"}
		if c, ok := t["context_policy"].(map[string]any); ok {
			cp = map[string]any{"state": c["state"]}
			if c["state"] == "enabled" {
				cp["ref"] = b.ref("context-policy", str(c["id"]), fmt.Sprint(c["version"]), str(c["digest"]))
				cp["version"] = c["version"]
			}
		}
		tr["context_policy"] = cp
		if pb, ok := t["plan_binding"].(map[string]any); ok {
			// a client-declared association; only allowlisted opaque references leave the client
			tr["plan_binding"] = map[string]any{"recommendation_set_id": pb["recommendation_set_id"], "plan_id": pb["plan_id"], "unit_ref": pb["unit_ref"],
				"revision": pb["revision"], "attempt_ref": pb["attempt_ref"], "after_snapshot_id": pb["after_snapshot_id"]}
		}
		if dc, ok := t["declared_changes"].([]any); ok && len(dc) > 0 {
			tr["declared_changes"] = dc
		}
		if oc, ok := t["observed_config"].([]any); ok && len(oc) > 0 {
			var refs []any
			for _, x := range oc {
				m, _ := x.(map[string]any)
				refs = append(refs, b.ref("observed-config", str(m["kind"]), str(m["digest"])))
			}
			tr["observed_config_refs"] = refs
		}
		trials = append(trials, tr)
		used = append(used, n)
	}
	if trials == nil {
		trials = []any{}
	}
	created := now.UTC().Truncate(time.Hour).Format(time.RFC3339)
	var revs []string
	for _, w := range b.WorkUnits {
		m, _ := w.(map[string]any)
		revs = append(revs, str(m["revision_digest"]))
	}
	r := map[string]any{"report_version": Version, "report_id": b.ref("report", created, fmt.Sprint(used), fmt.Sprint(revs)), "project_namespace": b.Namespace,
		"created_at": created, "producer": map[string]any{"tool": "git-hint", "version": version.Info().Version}, "trials": trials}
	if len(b.WorkUnits) > 0 {
		r["work_units"] = b.WorkUnits
	}
	d, err := hint.Digest(r)
	if err != nil {
		return nil, nil, err
	}
	r["content_digest"] = d
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := Check(out); err != nil {
		return nil, nil, err
	}
	return out, used, nil
}

// environmentIdentity is the canonical environment identity of an eval version: image,
// setup and the dependency locks it names (the immutable eval record is the source).
func environmentIdentity(ev map[string]any) []string { return EnvironmentIdentity(ev) }

// EnvironmentIdentity is the canonical environment identity of an eval version (exported for
// the controlled work unit journal; the same parts give the same environment reference).
func EnvironmentIdentity(ev map[string]any) []string {
	env, _ := ev["environment"].(map[string]any)
	parts := []string{str(env["image"]), str(env["setup"])}
	deps, _ := ev["dependencies"].([]any)
	var ds []string
	for _, d := range deps {
		b, _ := json.Marshal(d)
		ds = append(ds, string(b))
	}
	sort.Strings(ds)
	return append(parts, ds...)
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func str(v any) string { s, _ := v.(string); return s }

// Unreported returns the trial records that no earlier report holds.
func Unreported(l *library.Library) ([]string, error) {
	names, err := l.Manifests("trial")
	if err != nil {
		return nil, err
	}
	var sent struct {
		Trials []string `json:"trials"`
	}
	_ = l.Index("reports/sent", &sent)
	done := map[string]bool{}
	for _, t := range sent.Trials {
		done[t] = true
	}
	var out []string
	for _, n := range names {
		if !done[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// MarkReported records trial names that a written or sent report holds.
func MarkReported(l *library.Library, names []string) error {
	var sent struct {
		Trials []string `json:"trials"`
	}
	_ = l.Index("reports/sent", &sent)
	sent.Trials = append(sent.Trials, names...)
	sort.Strings(sent.Trials)
	return l.PutIndex("reports/sent", sent)
}
