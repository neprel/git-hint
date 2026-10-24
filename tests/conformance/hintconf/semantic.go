package hintconf

import (
	"encoding/json"
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
	// CodeLedger: a model ledger record (v0.2) whose counts, coverage, components or state do
	// not agree, whose canonical usage is known, whose correlation differs from its binding, whose
	// source is not a descriptive ledger source, or a second body, binding or seal time for the
	// same ledger revision or ledger in one document
	CodeLedger = "ledger_inconsistent"
	// CodeSourceCLIRoot: a source of the root CLI adapter (opencode-cli-root) whose declaration is
	// not the adapter's fixed measurement scope, or an observation of it that is not log-derived
	// or claims what the scope cannot observe (usage, a model call or ledger, a context
	// measurement, a turn)
	CodeSourceCLIRoot = "source_cli_root_inconsistent"
	// CodeMembership: an administrative membership epoch that is not one complete, ordered,
	// hash-chained and sealed enrollment (draft v0.2), or two records of one document that share
	// an execution, a run or a unit epoch
	CodeMembership = "membership_inconsistent"
	// CodeSourceOrchestrator: a project orchestrator source beyond its administrative scope, or an
	// observation of it that is not a MEMBERSHIP record or carries behavior, usage or closure fields
	CodeSourceOrchestrator = "source_orchestrator_inconsistent"
)

// The project orchestrator source (administrative membership records only; claims, no trust).
const (
	OrchestratorAdapter        = "hint-project-orchestrator"
	OrchestratorAdapterVersion = "0.1.0"
	MembershipMaxEvents        = 4096
	MembershipMaxExecutions    = 1024
)

var orchestratorUnavailable = []string{"usage", "request_kind", "collection", "context", "coordination", "targets", "correlation.session_id",
	"correlation.turn_id", "correlation.trace_id", "correlation.span_id", "correlation.task_id", "correlation.run_ref"}

// SourceOrchestratorProblem reports whether an orchestrator source declares more than the
// administrative scope: the exact adapter version, the single membership capability, the
// behavior, usage and closure fields unavailable, no usage normalization.
func SourceOrchestratorProblem(src map[string]any) bool {
	a, _ := src["adapter"].(map[string]any)
	caps := strs(src["capabilities"])
	if str(a["version"]) != OrchestratorAdapterVersion || len(caps) != 1 || caps[0] != "membership" {
		return true
	}
	for _, u := range orchestratorUnavailable {
		if !contains(strs(src["unavailable"]), u) {
			return true
		}
	}
	_, norm := src["usage_normalization"]
	return norm
}

// ObservationOrchestratorProblem reports whether an observation of an orchestrator source is
// anything but an administrative MEMBERSHIP record: no behavior, session, trace, task or run
// correlation (the membership body carries the actual identities), no usage or closure fields.
func ObservationOrchestratorProblem(o map[string]any) bool {
	if str(o["op"]) != "MEMBERSHIP" || str(o["source"]) != "orchestrator" {
		return true
	}
	if corr, _ := o["correlation"].(map[string]any); len(corr) > 0 {
		return true
	}
	for _, k := range []string{"usage", "model_ledger", "context", "request_kind", "collection", "coordination", "targets"} {
		if _, ok := o[k]; ok {
			return true
		}
	}
	return false
}

// membershipFinish are the terminal states without and after a launch.
var (
	finishWithoutLaunch = []string{"refused_before_launch", "start_unconfirmed"}
	finishAfterLaunch   = []string{"exited", "exited_nonzero", "stopped_after_timeout", "stopped_after_cancel", "stopped_after_wait_error", "stop_unconfirmed"}
)

// MembershipProblem reports whether one membership epoch is not complete, ordered and sealed:
// seq contiguous from 1; exactly one epoch_open first and one epoch_seal last; every prev_digest
// the canonical digest of the previous event (null only first) and head_digest that of the last;
// epoch 1 exactly when there is no predecessor; per execution enroll, then launch and a terminal
// after-launch finish, or a launch-less terminal finish; every enrolled execution terminal before
// the seal; unique execution and run refs; the seal counts the enrolled executions; bounds.
func MembershipProblem(m map[string]any) bool {
	events := list(m["events"])
	if len(events) < 2 || len(events) > MembershipMaxEvents {
		return true
	}
	prev := ""
	state := map[string]string{} // execution -> enrolled | launched | terminal
	runs := map[string]bool{}
	enrolled := 0
	for i, e := range events {
		ev, _ := e.(map[string]any)
		if int(num(ev["seq"])) != i+1 {
			return true
		}
		if i == 0 {
			if ev["prev_digest"] != nil {
				return true
			}
		} else if str(ev["prev_digest"]) != prev {
			return true
		}
		d, err := Digest(ev)
		if err != nil {
			return true
		}
		prev = d
		kind := str(ev["kind"])
		first, last := i == 0, i == len(events)-1
		if (kind == "epoch_open") != first || (kind == "epoch_seal") != last {
			return true
		}
		x := str(ev["execution_ref"])
		switch kind {
		case "epoch_open":
			if (int(num(ev["epoch"])) == 1) != (ev["predecessor_epoch_digest"] == nil) {
				return true
			}
		case "enroll":
			if _, seen := state[x]; seen || runs[str(ev["run_ref"])] {
				return true
			}
			state[x], runs[str(ev["run_ref"])] = "enrolled", true
			enrolled++
			if enrolled > MembershipMaxExecutions {
				return true
			}
		case "launch":
			if state[x] != "enrolled" {
				return true
			}
			state[x] = "launched"
		case "finish":
			f := str(ev["finish_state"])
			switch state[x] {
			case "enrolled":
				if !contains(finishWithoutLaunch, f) {
					return true
				}
			case "launched":
				if !contains(finishAfterLaunch, f) {
					return true
				}
			default:
				return true
			}
			state[x] = "terminal"
		case "epoch_seal":
			if int(num(ev["executions"])) != enrolled {
				return true
			}
			for _, s := range state {
				if s != "terminal" {
					return true
				}
			}
		}
	}
	return str(m["head_digest"]) != prev
}

// The root CLI adapter: its version fixes the measurement scope (opencode-cli-root 0.1.0: the
// terminal tool records of the root session in OpenCode 2.0.26 run JSON output).
const (
	CLIRootAdapter        = "opencode-cli-root"
	CLIRootAdapterVersion = "0.1.0"
	CLIRootAgentVersion   = "2.0.26"
)

var (
	cliRootCapabilities = []string{"time.start", "time.end", "targets", "outcome.status", "outcome.exit_code", "size", "content_delivery",
		"command_class", "correlation.session_id", "correlation.tool_call_id"}
	cliRootOptional    = []string{"correlation.task_id", "correlation.task_family_id", "correlation.run_ref"}
	cliRootUnavailable = []string{"usage", "request_kind", "collection", "correlation.turn_id", "correlation.trace_id", "correlation.span_id",
		"coordination", "context"}
)

// SourceCLIRootProblem reports whether a root CLI adapter source declares anything else than its
// scope: the exact reviewed adapter version, agent and agent version; every fixed capability,
// optionally the declared task, family and run correlations, nothing more; every unobservable
// field listed as unavailable; no usage normalization.
func SourceCLIRootProblem(src map[string]any) bool {
	a, _ := src["adapter"].(map[string]any)
	if str(a["version"]) != CLIRootAdapterVersion || str(src["agent"]) != "opencode" || str(src["agent_version"]) != CLIRootAgentVersion {
		return true
	}
	caps, unavail := strs(src["capabilities"]), strs(src["unavailable"])
	for _, c := range cliRootCapabilities {
		if !contains(caps, c) {
			return true
		}
	}
	for _, c := range caps {
		if !contains(cliRootCapabilities, c) && !contains(cliRootOptional, c) {
			return true
		}
	}
	for _, u := range cliRootUnavailable {
		if !contains(unavail, u) {
			return true
		}
	}
	_, norm := src["usage_normalization"]
	return norm
}

// ObservationCLIRootProblem reports whether an observation of a root CLI adapter source claims
// more than the scope: it must be a log-derived terminal tool record, never a session, turn, model
// call or usage record, and never carry usage, a model ledger, a context measurement, a request
// kind, a collection (closure authority), coordination, or a turn, trace or span correlation.
func ObservationCLIRootProblem(o map[string]any) bool {
	switch str(o["op"]) {
	case "MODEL_CALL", "USAGE", "SESSION", "TURN":
		return true
	}
	if str(o["source"]) != "log" {
		return true
	}
	for _, k := range []string{"usage", "model_ledger", "context", "request_kind", "collection", "coordination"} {
		if _, ok := o[k]; ok {
			return true
		}
	}
	corr, _ := o["correlation"].(map[string]any)
	for _, k := range []string{"turn_id", "trace_id", "span_id"} {
		if _, ok := corr[k]; ok {
			return true
		}
	}
	return false
}

func strs(v any) []string {
	var out []string
	for _, x := range list(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Canary values of the public research conventions; also used by the negative tests.
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
	cliRoot := map[string]bool{}      // root CLI adapter sources
	orchestrator := map[string]bool{} // project orchestrator sources
	for i, s := range list(c.doc["sources"]) {
		m, _ := s.(map[string]any)
		sources[str(m["source_id"])] = true
		a, _ := m["adapter"].(map[string]any)
		if str(a["name"]) == CLIRootAdapter {
			cliRoot[str(m["source_id"])] = true
			if SourceCLIRootProblem(m) {
				c.add(CodeSourceCLIRoot, "sources[%d]", i)
			}
		}
		if str(a["name"]) == OrchestratorAdapter {
			orchestrator[str(m["source_id"])] = true
			if SourceOrchestratorProblem(m) {
				c.add(CodeSourceOrchestrator, "sources[%d]", i)
			}
		}
	}
	memberExec, memberRun, memberEpoch := map[string]bool{}, map[string]bool{}, map[string]bool{} // across the document's membership records
	measured := map[string]bool{}
	family := map[string]string{} // task -> its declared family
	closed := map[string]bool{}   // source, session, collection ref
	calls := map[string]bool{}    // source, session, call ref, op
	kinds := map[string]string{}  // source, session, call ref -> request kind
	norms := map[string]string{}  // source -> usage normalization
	for _, s := range list(c.doc["sources"]) {
		m, _ := s.(map[string]any)
		norms[str(m["source_id"])] = str(m["usage_normalization"])
	}
	ledgers := map[string]string{}                                         // ledger ref (+ revision) -> canonical body, binding and seal time
	revisions, revisionAt := map[string]map[string]any{}, map[string]int{} // ledger ref + revision -> record
	for i, o := range list(payload["observations"]) {
		m, _ := o.(map[string]any)
		c.modelCall(m, i, closed, calls, kinds)
		if l, ok := m["model_ledger"].(map[string]any); ok {
			if ledgerProblem(m, l, norms, ledgers) {
				c.add(CodeLedger, "payload.observations[%d].model_ledger", i)
			}
			ref := str(l["ledger_ref"]) + "\x00" + fmt.Sprint(num(l["revision"]))
			revisions[ref], revisionAt[ref] = l, i
		}
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
		if cliRoot[str(m["source_id"])] && ObservationCLIRootProblem(m) {
			c.add(CodeSourceCLIRoot, "payload.observations[%d]", i)
		}
		if mem, ok := m["membership"].(map[string]any); ok {
			if !orchestrator[str(m["source_id"])] || ObservationOrchestratorProblem(m) {
				c.add(CodeSourceOrchestrator, "payload.observations[%d]", i)
			}
			bad := MembershipProblem(mem)
			for _, e := range list(mem["events"]) {
				ev, _ := e.(map[string]any)
				switch str(ev["kind"]) {
				case "enroll":
					x, r := str(ev["execution_ref"]), str(ev["run_ref"])
					bad = bad || memberExec[x] || memberRun[r]
					memberExec[x], memberRun[r] = true, true
				case "epoch_open":
					k := str(mem["unit_ref"]) + "\x00" + fmt.Sprint(num(ev["epoch"]))
					bad = bad || memberEpoch[k]
					memberEpoch[k] = true
				}
			}
			if bad {
				c.add(CodeMembership, "payload.observations[%d].membership", i)
			}
		} else if orchestrator[str(m["source_id"])] {
			c.add(CodeSourceOrchestrator, "payload.observations[%d]", i)
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
	// a later revision of a ledger in the document never heals or rewrites the earlier one
	for key, next := range revisions {
		ref, _, _ := strings.Cut(key, "\x00")
		if prev, ok := revisions[ref+"\x00"+fmt.Sprint(num(next["revision"])-1)]; ok && ledgerLifecycle(prev, next) {
			c.add(CodeLedger, "payload.observations[%d].model_ledger", revisionAt[key])
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
	case "hint-task-tokens-v1", "openai-compat-provider-totals-v1":
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
// cost_unknown needs a whole-task effort delta (hint-task-tokens-v1 and full_task; provider
// totals or any other semantics never qualify); chosen names
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
				if sem, scope := effortLabels(e); !hasEffort || sem != "hint-task-tokens-v1" || scope != "full_task" || !usableDelta(e, paired) || !usableDelta(suc, paired) {
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
	if eb, ok := fb["effect_basis"].(map[string]any); ok && effectBasisProblem(eb, metric, unit) {
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
// The basis never clears a forecast condition by itself. Both tuples are token estimands, so
// the forecast objective is model_tokens in tokens (and the objective rule then requires the
// separate per-task model_tokens prediction); another objective needs its own tuple.
func effectBasisProblem(eb map[string]any, metric, unit string) bool {
	if metric != "model_tokens" || unit != "tokens" {
		return true
	}
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
	return str(e["unit"]) != "tokens" || !pok || !lok || !hok || pt < lo || pt > hi
}

// ledgerEntryStates are the terminal states of admitted requests; their counts add up to admitted.
var ledgerEntryStates = []string{"complete", "usage_partial", "missing_usage", "usage_conflict", "unreadable", "truncated", "error", "cancelled", "unknown"}

// ledgerProblem checks one model ledger record against itself, its record and the earlier
// ledger records of the document (fail closed: a missing or non-integer count is a problem).
func ledgerProblem(m, l map[string]any, norms, seen map[string]string) bool {
	counts, _ := l["counts"].(map[string]any)
	n := func(k string) (int, bool) {
		v, ok := counts[k].(float64)
		return int(v), ok && v >= 0 && v == float64(int(v))
	}
	admitted, ok := n("admitted")
	sum := 0
	for _, k := range ledgerEntryStates {
		v, okk := n(k)
		ok = ok && okk
		sum += v
	}
	late, ok1 := n("late")
	rejected, ok2 := n("rejected_limit")
	complete, _ := n("complete")
	if !ok || !ok1 || !ok2 || sum != admitted {
		return true
	}
	cov, _ := l["component_coverage"].(map[string]any)
	comps, _ := l["components"].(map[string]any)
	for _, k := range []string{"prompt", "completion", "cached", "reasoning"} {
		c, okc := cov[k].(float64)
		_, known := comps[k].(float64)
		if !okc || c < 0 || int(c) > admitted || known != (c > 0) {
			return true
		}
	}
	if LedgerEntriesProblem(l) != "" || LedgerAgentRunProblem(l) != "" || LedgerBackendCaptureProblem(l) != "" {
		return true
	}
	full := admitted > 0 && complete == admitted && int(num(cov["prompt"])) == admitted && int(num(cov["completion"])) == admitted &&
		l["model_check"] == "requested_model_observed" && l["scope"] == "isolated_container" && l["bypass"] == "enforced" && late == 0 && rejected == 0
	reasons := list(l["reasons"])
	if (l["state"] == "complete") != (full && len(reasons) == 0) || (l["state"] == "incomplete" && len(reasons) == 0) {
		return true
	}
	// the canonical usage of a ledger record stays unknown (the provider usage is descriptive)
	if u, _ := m["usage"].(map[string]any); u != nil {
		for _, k := range []string{"input_tokens", "output_tokens"} {
			if q, _ := u[k].(map[string]any); q == nil || q["state"] == "known" {
				return true
			}
		}
	}
	if norms[str(m["source_id"])] != "openai-compat-usage-v1" {
		return true
	}
	corr, _ := m["correlation"].(map[string]any)
	b, _ := l["binding"].(map[string]any)
	if str(corr["run_ref"]) != str(b["run_ref"]) || str(corr["task_id"]) != str(b["task_id"]) || str(corr["task_family_id"]) != str(b["task_family_id"]) {
		return true
	}
	body, _ := json.Marshal(l)
	ref, rev := str(l["ledger_ref"]), fmt.Sprint(num(l["revision"]))
	fixed, _ := json.Marshal([]any{l["binding"], l["sealed_at"]})
	if prev, ok := seen[ref+"\x00"+rev]; ok && prev != string(body) {
		return true // a second body for one revision
	}
	if prev, ok := seen[ref]; ok && prev != string(fixed) {
		return true // a revision with another binding or seal time
	}
	seen[ref+"\x00"+rev], seen[ref] = string(body), string(fixed)
	return false
}

func num(v any) float64 { f, _ := v.(float64); return f }

// ledgerLifecycle says whether ledger revision next cannot follow prev (the Go client's
// activity.LedgerLifecycleProblem on the wire form): frozen evidence, non-decreasing late and
// rejected counts, no recovery from incompleteness, no lost reason.
func ledgerLifecycle(prev, next map[string]any) bool {
	same := func(k string) bool {
		a, _ := json.Marshal(prev[k])
		b, _ := json.Marshal(next[k])
		return string(a) == string(b)
	}
	// entries and their timing are frozen at the first seal: present or absent in every revision alike
	for _, k := range append([]string{"ledger_ref", "binding", "sealed_at", "model_check", "scope", "bypass", "components", "component_coverage", "entries", "agent_run", "backend_capture"}, LedgerTiming...) {
		_, inPrev := prev[k]
		_, inNext := next[k]
		if !same(k) || inPrev != inNext {
			return true
		}
	}
	pc, _ := prev["counts"].(map[string]any)
	nc, _ := next["counts"].(map[string]any)
	for _, k := range append([]string{"admitted"}, ledgerEntryStates...) {
		if num(pc[k]) != num(nc[k]) {
			return true
		}
	}
	if num(nc["late"]) < num(pc["late"]) || num(nc["rejected_limit"]) < num(pc["rejected_limit"]) {
		return true
	}
	if prev["state"] == "incomplete" && next["state"] != "incomplete" {
		return true
	}
	have := set(list(next["reasons"]))
	for _, r := range list(prev["reasons"]) {
		if !have[str(r)] {
			return true
		}
	}
	return false
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
