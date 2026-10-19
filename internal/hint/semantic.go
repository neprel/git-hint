package hint

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Finding is one semantic rule violation (hint-data §9.2). Code is one of the codes below.
type Finding struct {
	Code string
	Path string
}

func (f Finding) String() string { return f.Code + " at " + f.Path }

// Semantic finding codes.
const (
	CodeDigest         = "digest_mismatch"
	CodeSnapshotDigest = "snapshot_digest_mismatch"
	CodeContent        = "content_forbidden"
	CodeRepoUndeclared = "repository_not_declared"
	CodeRecordType     = "record_type_not_declared"
	CodePolicy         = "policy_violation"
	CodeTimeResolution = "time_resolution"
	CodeTimeOrder      = "time_order"
	CodeUnsorted       = "unsorted"
	CodeDuplicate      = "duplicate_id"
	// CodeDuplicateMeasurement: two context measurements of one tool call at the same stage,
	// action and reference (v0.2); raw output and delivered context are two stages of one call
	CodeDuplicateMeasurement = "duplicate_measurement"
	CodeDangling             = "dangling_reference"
	CodeNamespace            = "namespace_mismatch"
	CodeStepGraph            = "step_graph"
	CodeInterval             = "interval_order"
	CodeForecast             = "forecast_inconsistent"
	CodeCoverage             = "coverage_inconsistent"
	CodeSnapshotSet          = "plan_snapshot_set"
	CodePlanApplication      = "plan_application_inconsistent"
	CodeTaskFamily           = "task_family_conflict"
	// CodeCollection: a collector closure of a session (v0.2) with a non-integer count, a
	// closure time before the session start, or a second closure of the same session scope
	CodeCollection = "collection_inconsistent"
	// CodeCallLink: one model call (source, session, call_ref) with two start or two terminal
	// records, or a terminal of another request kind than its start (v0.2)
	CodeCallLink = "call_link_conflict"
	// CodeEffortScope: an effort delta whose counter semantics and scope do not belong together,
	// or a decision that uses an effort delta without whole-task scope (v0.2)
	CodeEffortScope = "effort_scope_inconsistent"
	// CodeEffectBasis: a forecast effect basis (v0.2) whose source, estimand and uncertainty do
	// not belong together, a model projection with observed fields, an observed comparison
	// without its summary, references, population or policy, or an observed summary in another
	// unit than the objective or outside its own interval
	CodeEffectBasis = "effect_basis_inconsistent"
)

// Canary values of the conformance suite; also used by the negative tests.
var Canaries = []string{"HINTCANARY_", "canary_dir_e1d2", "canarySymbolQ7"}

// tokenRe is the character set of every string that a v0.1 schema admits
// (hex identifiers, digests, timestamps, versions and enumeration tokens).
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_.:+-]{1,80}$`)

// recordArrays maps payload arrays to record types (hint-data §3).
var recordArrays = map[string]string{
	"snapshots": "Snapshot", "entities": "Entity", "relations": "Relation", "constraints": "PolicyConstraint",
	"changes": "Change", "observations": "Observation", "metrics": "Metric", "plans": "Plan", "effects": "Effect",
}

// Check runs the semantic rules on a schema-valid document. policy may be nil; when it is
// given (an export_policy payload), the field allowlist is checked as well.
func Check(doc map[string]any, policy map[string]any) []Finding {
	c := &checker{doc: doc}
	c.run(policy)
	sort.SliceStable(c.out, func(i, j int) bool { return c.out[i].Path < c.out[j].Path })
	return c.out
}

type checker struct {
	doc map[string]any
	out []Finding
}

func (c *checker) add(code, path string, args ...any) {
	if len(args) > 0 {
		path = fmt.Sprintf(path, args...)
	}
	c.out = append(c.out, Finding{code, path})
}

func (c *checker) run(policy map[string]any) {
	doc := c.doc
	payload, _ := doc["payload"].(map[string]any)
	if d, err := Digest(payload); err != nil || d != doc["content_digest"] {
		c.add(CodeDigest, "content_digest")
	}
	c.scan(doc, "")
	red, _ := doc["redaction"].(map[string]any)
	declared := set(list(red["record_types"]))
	for key, rt := range recordArrays {
		if len(list(payload[key])) > 0 && !declared[rt] {
			c.add(CodeRecordType, "payload.%s", key)
		}
	}
	if red["profile"] == "aggregate" && len(list(payload["observations"])) > 0 {
		c.add(CodePolicy, "payload.observations")
	}
	if policy != nil {
		c.policy(red, payload, policy)
	}
	c.timeResolution(doc, toInt(red["time_resolution_seconds"]))
	c.numbers(payload, "payload")
	repos := set(list(doc["repositories"]))
	switch doc["document_type"] {
	case "repository_snapshot":
		c.snapshot(payload, repos)
	case "change_set":
		c.uniq(payload, "changes", "change_id")
		if !repos[str(payload["repo_id"])] {
			c.add(CodeRepoUndeclared, "payload.repo_id")
		}
		// a declared cross-repository group names the declaring repository among its members
		if pa, ok := payload["plan_application"].(map[string]any); ok {
			if g, ok := pa["group"].(map[string]any); ok {
				if r, _ := g["repo_ids"].([]any); !set(r)[str(payload["repo_id"])] {
					c.add(CodePlanApplication, "payload.plan_application.group.repo_ids")
				}
			}
		}
	case "activity_stream":
		c.activity(payload)
	case "recommendation_set":
		c.plans(payload, repos)
	case "effect_report":
		c.uniq(payload, "effects", "effect_id")
	}
}

func (c *checker) scan(v any, path string) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if !tokenRe.MatchString(k) {
				c.add(CodeContent, "%s.<key>", path)
			}
			c.scan(t[k], path+"."+k)
		}
	case []any:
		for i, e := range t {
			c.scan(e, fmt.Sprintf("%s[%d]", path, i))
		}
	case string:
		bad := !tokenRe.MatchString(t)
		for _, cn := range Canaries {
			bad = bad || strings.Contains(t, cn)
		}
		if bad {
			c.add(CodeContent, strings.TrimPrefix(path, "."))
		}
	}
}

func (c *checker) policy(red, payload, pol map[string]any) {
	if red["policy_id"] != pol["policy_id"] || red["profile"] != pol["profile"] ||
		toInt(red["time_resolution_seconds"]) < toInt(pol["time_resolution_seconds"]) {
		c.add(CodePolicy, "redaction")
	}
	allowedTypes := set(list(pol["record_types"]))
	fields, _ := pol["fields"].(map[string]any)
	for key, rt := range recordArrays {
		recs := list(payload[key])
		if len(recs) == 0 {
			continue
		}
		if !allowedTypes[rt] {
			c.add(CodePolicy, "payload.%s", key)
			continue
		}
		allowed := set(list(fields[rt]))
		for i, r := range recs {
			m, _ := r.(map[string]any)
			for _, f := range sortedKeys(m) {
				if !allowed[f] {
					c.add(CodePolicy, "payload.%s[%d].%s", key, i, f)
				}
			}
		}
	}
}

var tsKeys = map[string]bool{"start": true, "end": true, "captured_at": true, "valid_from": true, "valid_to": true,
	"created_at": true, "expires_at": true}

func (c *checker) timeResolution(v any, res int) {
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(t) {
				if s, ok := t[k].(string); ok && tsKeys[k] {
					ts, err := time.Parse(time.RFC3339Nano, s)
					if err == nil && res >= 1 && (ts.Nanosecond() != 0 || ts.Unix()%int64(res) != 0) {
						c.add(CodeTimeResolution, path+"."+k)
					}
					continue
				}
				walk(t[k], path+"."+k)
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(v, "")
}

// numbers checks quantities and intervals anywhere in the payload.
func (c *checker) numbers(v any, path string) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["state"]; ok && t["unit"] != nil {
			c.quantity(t, path)
		}
		if lo, ok1 := t["lower"].(float64); ok1 {
			if hi, ok2 := t["upper"].(float64); ok2 && lo > hi {
				c.add(CodeInterval, path)
			}
		}
		for _, k := range sortedKeys(t) {
			c.numbers(t[k], path+"."+k)
		}
	case []any:
		for i, e := range t {
			c.numbers(e, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func (c *checker) quantity(q map[string]any, path string) {
	cov, _ := q["coverage"].(map[string]any)
	if cov != nil && cov["total"] != nil {
		covered, total := toInt(cov["covered"]), toInt(cov["total"])
		switch {
		case covered > total,
			q["state"] == "partial" && covered == total,
			q["state"] == "known" && covered != total:
			c.add(CodeCoverage, path)
		}
	}
	if b, ok := q["bounds"].(map[string]any); ok {
		lo, _ := b["lower"].(float64)
		if v, ok := q["value"].(float64); ok && (v < lo || (b["upper"] != nil && v > b["upper"].(float64))) {
			c.add(CodeInterval, path)
		}
	}
}

func (c *checker) uniq(payload map[string]any, key, idField string) map[string]map[string]any {
	seen := map[string]map[string]any{}
	for i, r := range list(payload[key]) {
		m, _ := r.(map[string]any)
		id := str(m[idField])
		if _, dup := seen[id]; dup {
			c.add(CodeDuplicate, "payload.%s[%d].%s", key, i, idField)
		}
		seen[id] = m
	}
	return seen
}

// sortedBy checks strict ascending order of (snapshot_id, id) and reports duplicates.
func (c *checker) sortedBy(payload map[string]any, key, idField string) map[string]bool {
	keys := map[string]bool{}
	prev := ""
	for i, r := range list(payload[key]) {
		m, _ := r.(map[string]any)
		k := str(m["snapshot_id"]) + "/" + str(m[idField])
		if keys[k] {
			c.add(CodeDuplicate, "payload.%s[%d]", key, i)
		} else if k < prev {
			c.add(CodeUnsorted, "payload.%s[%d]", key, i)
		}
		keys[k] = true
		prev = k
	}
	return keys
}

func (c *checker) snapshot(payload map[string]any, repos map[string]bool) {
	snaps := c.uniq(payload, "snapshots", "snapshot_id")
	for i, s := range list(payload["snapshots"]) {
		m, _ := s.(map[string]any)
		if !repos[str(m["repo_id"])] {
			c.add(CodeRepoUndeclared, "payload.snapshots[%d].repo_id", i)
		}
		if d, err := SnapshotDigest(payload, str(m["snapshot_id"])); err != nil || d != m["digest"] {
			c.add(CodeSnapshotDigest, "payload.snapshots[%d].digest", i)
		}
	}
	ents := c.sortedBy(payload, "entities", "entity_id")
	c.sortedBy(payload, "relations", "relation_id")
	c.sortedBy(payload, "constraints", "constraint_id")
	in := func(snap, id string) bool { return ents[snap+"/"+id] }
	for i, e := range list(payload["entities"]) {
		m, _ := e.(map[string]any)
		sn := str(m["snapshot_id"])
		s, ok := snaps[sn]
		if !ok {
			c.add(CodeDangling, "payload.entities[%d].snapshot_id", i)
			continue
		}
		if m["repo_id"] != s["repo_id"] {
			c.add(CodeDangling, "payload.entities[%d].repo_id", i)
		}
		if p, ok := m["parent_id"].(string); ok && !in(sn, p) {
			c.add(CodeDangling, "payload.entities[%d].parent_id", i)
		}
	}
	for i, r := range list(payload["relations"]) {
		m, _ := r.(map[string]any)
		sn := str(m["snapshot_id"])
		if !in(sn, str(m["source"])) || !in(sn, str(m["target"])) {
			c.add(CodeDangling, "payload.relations[%d]", i)
		}
	}
	for i, r := range list(payload["constraints"]) {
		m, _ := r.(map[string]any)
		for _, id := range list(m["scope"]) {
			if !in(str(m["snapshot_id"]), str(id)) {
				c.add(CodeDangling, "payload.constraints[%d].scope", i)
			}
		}
	}
}

func (c *checker) activity(payload map[string]any) {
	obs := c.uniq(payload, "observations", "observation_id")
	c.uniq(payload, "metrics", "metric_id")
	sources := map[string]bool{}
	for _, s := range list(c.doc["sources"]) {
		m, _ := s.(map[string]any)
		sources[str(m["source_id"])] = true
	}
	measured := map[string]bool{}
	family := map[string]string{} // task -> its declared family
	closed := map[string]bool{}   // source, session, collection ref
	calls := map[string]bool{}    // source, session, call ref, op
	kinds := map[string]string{}  // source, session, call ref -> request kind
	for i, o := range list(payload["observations"]) {
		m, _ := o.(map[string]any)
		c.modelCall(m, i, closed, calls, kinds)
		// one task declares one family; a second family for the same task is a conflict
		if corr, _ := m["correlation"].(map[string]any); str(corr["task_family_id"]) != "" {
			task, fam := str(corr["task_id"]), str(corr["task_family_id"])
			if prev, ok := family[task]; ok && prev != fam {
				c.add(CodeTaskFamily, "payload.observations[%d].correlation.task_family_id", i)
			}
			family[task] = fam
		}
		if !sources[str(m["source_id"])] {
			c.add(CodeDangling, "payload.observations[%d].source_id", i)
		}
		t, _ := m["time"].(map[string]any)
		s, e := str(t["start"]), str(t["end"])
		if s != "" && e != "" && before(e, s) {
			c.add(CodeTimeOrder, "payload.observations[%d].time", i)
		}
		if ctx, ok := m["context"].(map[string]any); ok {
			corr, _ := m["correlation"].(map[string]any)
			key := str(corr["tool_call_id"]) + "\x00" + str(ctx["stage"]) + "\x00" + str(ctx["action"]) + "\x00" + str(ctx["ref_id"])
			if measured[key] {
				c.add(CodeDuplicateMeasurement, "payload.observations[%d].context", i)
			}
			measured[key] = true
		}
	}
	for i, x := range list(payload["metrics"]) {
		m, _ := x.(map[string]any)
		for _, id := range list(m["inputs"]) {
			if _, ok := obs[str(id)]; !ok {
				c.add(CodeDangling, "payload.metrics[%d].inputs", i)
			}
		}
	}
}

// modelCall checks the v0.2 model-call link and the collector closure of one observation.
func (c *checker) modelCall(m map[string]any, i int, closed, calls map[string]bool, kinds map[string]string) {
	corr, _ := m["correlation"].(map[string]any)
	scope := str(m["source_id"]) + "\x00" + str(corr["session_id"])
	if ref := str(corr["call_ref"]); ref != "" {
		key := scope + "\x00" + ref
		if calls[key+"\x00"+str(m["op"])] {
			c.add(CodeCallLink, "payload.observations[%d].correlation.call_ref", i)
		}
		calls[key+"\x00"+str(m["op"])] = true
		if k, ok := kinds[key]; ok && k != str(m["request_kind"]) {
			c.add(CodeCallLink, "payload.observations[%d].request_kind", i)
		}
		kinds[key] = str(m["request_kind"])
	}
	col, ok := m["collection"].(map[string]any)
	if !ok {
		return
	}
	at := fmt.Sprintf("payload.observations[%d].collection", i)
	key := scope + "\x00" + str(col["collection_ref"])
	if closed[key] {
		c.add(CodeCollection, at)
	}
	closed[key] = true
	for _, f := range []string{"events_dropped", "errors", "open_calls"} {
		q, _ := col[f].(map[string]any)
		if v, ok := q["value"].(float64); ok && v != math.Trunc(v) {
			c.add(CodeCollection, at+"."+f)
		}
	}
	t, _ := m["time"].(map[string]any)
	if s, e := str(t["start"]), str(col["closed_at"]); s != "" && e != "" && before(e, s) {
		c.add(CodeCollection, at+".closed_at")
	}
}

// effortLabels returns the counter semantics and scope of an effort delta ("unknown" when absent).
func effortLabels(e map[string]any) (string, string) {
	sem, scope := str(e["counter_semantics"]), str(e["scope"])
	if sem == "" {
		sem = "unknown"
	}
	if scope == "" {
		scope = "unknown"
	}
	return sem, scope
}

// effortPaired reports whether an effort delta's semantics and scope belong together.
func effortPaired(e map[string]any) bool {
	sem, scope := effortLabels(e)
	switch sem {
	case "harbor-0.23.0+opencode:n-tokens-root-v1":
		return scope == "root_session"
	case "hint-task-tokens-v1":
		return scope == "full_task"
	}
	return scope == "unknown"
}

// usableDelta reports whether an observed difference is complete evidence: a numeric observed
// value, an interval with numeric bounds, and coverage [n, n] with n > 0 that equals the
// alternative's paired objectives when they are given.
func usableDelta(d map[string]any, paired any) bool {
	if d == nil {
		return false
	}
	if _, ok := d["observed"].(float64); !ok {
		return false
	}
	iv, ok := d["interval"].(map[string]any)
	if !ok {
		return false
	}
	_, lo := iv["lower"].(float64)
	_, hi := iv["upper"].(float64)
	cov := list(d["coverage"])
	if !lo || !hi || len(cov) != 2 {
		return false
	}
	k, kok := cov[0].(float64)
	n, nok := cov[1].(float64)
	if !kok || !nok || n <= 0 || k != n {
		return false
	}
	if p, ok := paired.(float64); ok && p != n {
		return false
	}
	return true
}

// decisions checks the effort deltas of the findings' decisions and of the project comparisons:
// a delta's semantics and scope belong together; an alternative in state chosen, known or
// cost_unknown needs a whole-task effort delta (hint-task-tokens-v1, full_task); chosen names
// the alternative in state chosen; NO_CHANGE needs every alternative excluded for correctness or
// known with whole-task effort.
func (c *checker) decisions(payload map[string]any) {
	for i, x := range list(payload["project_comparisons"]) {
		m, _ := x.(map[string]any)
		if e, ok := m["effort_delta"].(map[string]any); ok && !effortPaired(e) {
			c.add(CodeEffortScope, "payload.project_comparisons[%d].effort_delta", i)
		}
	}
	for i, f := range list(payload["findings"]) {
		fm, _ := f.(map[string]any)
		d, ok := fm["decision"].(map[string]any)
		if !ok {
			continue
		}
		at := fmt.Sprintf("payload.findings[%d].decision", i)
		chosen := str(d["chosen"])
		hasChosen := false
		for j, a := range list(d["alternatives"]) {
			am, _ := a.(map[string]any)
			e, hasEffort := am["effort_delta"].(map[string]any)
			if hasEffort && !effortPaired(e) {
				c.add(CodeEffortScope, "%s.alternatives[%d].effort_delta", at, j)
			}
			state := str(am["state"])
			switch state {
			case "chosen", "known", "cost_unknown":
				// actionable: whole-task labels AND complete evidence (labels alone are not data)
				var paired any
				if sc, ok := am["scope"].(map[string]any); ok {
					paired = sc["paired_objectives"]
				}
				suc, _ := am["success_delta"].(map[string]any)
				if _, scope := effortLabels(e); !hasEffort || scope != "full_task" || !effortPaired(e) || !usableDelta(e, paired) || !usableDelta(suc, paired) {
					c.add(CodeEffortScope, "%s.alternatives[%d].state", at, j)
				}
			}
			if state == "chosen" {
				hasChosen = true
				if str(am["intervention"]) != chosen {
					c.add(CodeEffortScope, "%s.alternatives[%d].state", at, j)
				}
			}
			if chosen == "NO_CHANGE" && state != "excluded_correctness" && state != "known" && state != "cost_unknown" {
				c.add(CodeEffortScope, "%s.chosen", at)
			}
		}
		if (chosen == "CONTEXT_ONLY" || chosen == "REFACTOR_CODE") && !hasChosen {
			c.add(CodeEffortScope, "%s.chosen", at)
		}
	}
}

// DecisionEffortProblem reports whether one finding decision breaks the effort_scope_inconsistent
// rule (for consumers that render stored documents: such a decision is historical/unverified).
func DecisionEffortProblem(d map[string]any) bool {
	c := &checker{doc: map[string]any{}}
	c.decisions(map[string]any{"findings": []any{map[string]any{"decision": d}}})
	return len(c.out) > 0
}

func (c *checker) plans(payload map[string]any, repos map[string]bool) {
	c.decisions(payload)
	c.uniq(payload, "plans", "plan_id")
	for i, p := range list(payload["plans"]) {
		m, _ := p.(map[string]any)
		at := fmt.Sprintf("payload.plans[%d]", i)
		if m["project_namespace"] != c.doc["project_namespace"] {
			c.add(CodeNamespace, at+".project_namespace")
		}
		planRepos := set(list(m["repo_ids"]))
		for r := range planRepos {
			if !repos[r] {
				c.add(CodeRepoUndeclared, at+".repo_ids")
			}
		}
		// exactly one snapshot for each plan repository: none missing, repeated or extra
		perRepo := map[string]int{}
		for _, s := range list(m["snapshots"]) {
			sm, _ := s.(map[string]any)
			perRepo[str(sm["repo_id"])]++
		}
		for r, n := range perRepo {
			if !planRepos[r] || n > 1 {
				c.add(CodeSnapshotSet, at+".snapshots")
			}
		}
		for r := range planRepos {
			if perRepo[r] == 0 {
				c.add(CodeSnapshotSet, at+".snapshots")
			}
		}
		if !before(str(m["created_at"]), str(m["expires_at"])) {
			c.add(CodeTimeOrder, at+".expires_at")
		}
		targets := set(list(m["targets"]))
		seen := map[int]bool{}
		for j, s := range list(m["steps"]) {
			sm, _ := s.(map[string]any)
			id := toInt(sm["step_id"])
			if seen[id] {
				c.add(CodeStepGraph, "%s.steps[%d].step_id", at, j)
			}
			for _, d := range list(sm["depends_on"]) {
				if !seen[toInt(d)] { // a step depends only on steps listed before it
					c.add(CodeStepGraph, "%s.steps[%d].depends_on", at, j)
				}
			}
			seen[id] = true
			for _, t := range list(sm["targets"]) {
				if !targets[str(t)] {
					c.add(CodeDangling, "%s.steps[%d].targets", at, j)
				}
			}
		}
		for j, pc := range list(m["preconditions"]) {
			pm, _ := pc.(map[string]any)
			for _, t := range list(pm["targets"]) {
				if !targets[str(t)] {
					c.add(CodeDangling, "%s.preconditions[%d].targets", at, j)
				}
			}
		}
		if fb, ok := m["forecast_basis"].(map[string]any); ok {
			c.forecast(m, fb, at)
		}
	}
}

// forecast checks a plan's forecast_basis (v0.2) against its predictions, cost, break-even and
// simulation labels: one unit per objective, no numeric net value without a known one-time
// cost, the threshold provenance, and no production recommendation from a scenario, synthetic
// evidence or an out-of-distribution qualification.
func (c *checker) forecast(m, fb map[string]any, at string) {
	bad := func(field string) { c.add(CodeForecast, "%s.forecast_basis.%s", at, field) }
	obj, _ := fb["objective"].(map[string]any)
	unit, metric := str(obj["unit"]), str(obj["metric"])
	sim, _ := m["simulation"].(map[string]any)
	if sim == nil {
		bad("objective")
		return
	}
	found := false
	for _, p := range list(m["predictions"]) {
		pm, _ := p.(map[string]any)
		if str(pm["metric"]) == metric && str(pm["unit"]) == unit && str(pm["per"]) == "task" {
			found = true
		}
	}
	if !found {
		bad("objective")
	}
	cost, _ := m["refactoring_cost_estimate"].(map[string]any)
	if cost != nil && str(cost["unit"]) != unit {
		bad("objective")
	}
	_, hasBreakEven := m["break_even_horizon"]
	if (fb["p_net_gain_positive"] != nil || fb["net_gain_interval"] != nil || hasBreakEven) && (fb["cost_basis"] == nil || cost == nil) {
		bad("cost_basis")
	}
	if eb, ok := fb["effect_basis"].(map[string]any); ok && effectBasisProblem(eb, unit) {
		c.add(CodeEffectBasis, "%s.forecast_basis.effect_basis", at)
	}
	if fb["threshold_basis"] == "heuristic_default" && fb["threshold"] != DefaultForecastThreshold {
		bad("threshold_basis")
	}
	recommend := m["decision"] == "recommend"
	if recommend && (fb["cost_basis"] == "assumption_scenario" || fb["evidence"] == "synthetic_fixture") {
		bad("evidence")
	}
	conds := set(list(fb["conditional_on"]))
	ood := false
	for _, k := range []string{"unseen_transformation", "feature_extrapolation", "unseen_task_mix", "unseen_configuration", "coordination_unmodeled"} {
		ood = ood || conds[k]
	}
	if ood && (sim["out_of_distribution"] != true || recommend) {
		bad("conditional_on")
	}
	if (sim["level"] == "L2" || sim["level"] == "L3") && !conds["behavior_transport"] {
		bad("conditional_on")
	}
	// a necessary (not sufficient) consistency gate for recommend: an L3 forecast with a
	// baseline calibration, explicitly in distribution, with a known comparable cost and a
	// known probability at or above the threshold and a known net interval; nothing passes by
	// omission; break-even is not required
	if recommend {
		p, pok := fb["p_net_gain_positive"].(float64)
		th, _ := fb["threshold"].(float64)
		cal := sim["calibration_status"]
		if sim["level"] != "L3" || (cal != "backtested" && cal != "holdout_validated") || sim["out_of_distribution"] != false ||
			fb["cost_basis"] != "observed_comparable_work" || cost == nil || !pok || p < th || fb["net_gain_interval"] == nil {
			bad("decision")
		}
	}
}

// effectBasisTuples are the only (source, estimand, uncertainty) combinations of an effect basis.
var effectBasisTuples = map[string][2]string{
	"model_projection":             {"model_task_mix_mean_delta", "model_conditional_interval"},
	"observed_objective_clustered": {"controlled_task_token_delta", "uncertainty_of_mean_delta"},
}

// observedFields are the fields that only an observed controlled comparison carries.
var observedFields = []string{"effect", "objectives", "record_refs", "population_ref", "evidence_policy"}

// effectBasisProblem reports whether a forecast effect basis is internally inconsistent. It checks
// the labels, the fields and the arithmetic of the summary only: whether the opaque records belong
// to the population and support the summary is a producer check, not a property of the document.
// The basis never clears a forecast condition by itself.
func effectBasisProblem(eb map[string]any, unit string) bool {
	want, ok := effectBasisTuples[str(eb["source"])]
	if !ok || str(eb["estimand"]) != want[0] || str(eb["uncertainty"]) != want[1] {
		return true
	}
	observed := eb["source"] == "observed_objective_clustered"
	for _, f := range observedFields {
		if _, has := eb[f]; has != observed {
			return true
		}
	}
	if !observed {
		return false
	}
	e, _ := eb["effect"].(map[string]any)
	iv, _ := e["interval"].(map[string]any)
	pt, pok := e["point"].(float64)
	lo, lok := iv["lower"].(float64)
	hi, hok := iv["upper"].(float64)
	return str(e["unit"]) != unit || !pok || !lok || !hok || pt < lo || pt > hi
}

// DefaultForecastThreshold is the heuristic default decision threshold (a versioned example
// value, not a policy).
const DefaultForecastThreshold = 0.9

func before(a, b string) bool {
	ta, e1 := time.Parse(time.RFC3339Nano, a)
	tb, e2 := time.Parse(time.RFC3339Nano, b)
	return e1 == nil && e2 == nil && ta.Before(tb)
}

func set(l []any) map[string]bool {
	m := map[string]bool{}
	for _, v := range l {
		m[str(v)] = true
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}
