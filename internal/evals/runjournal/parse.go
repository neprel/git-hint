package runjournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var ErrStoredRun = errors.New("stored controlled run refused")
var runID = regexp.MustCompile(`^jb-[0-9a-f]{16}$`)
var evalID = regexp.MustCompile(`^ev-[0-9a-f]{16}@[1-9][0-9]*$`)
var objectiveID = regexp.MustCompile(`^ev-[0-9a-f]{16}$`)
var suiteID = regexp.MustCompile(`^su-[0-9a-f]{16}$`)
var trialID = regexp.MustCompile(`^tr-[0-9a-f]{16}$`)
var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var refID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var schemaOnce sync.Once
var revisionSchema, configurationSchema *jsonschema.Schema
var schemaError error

func loadSchemas() {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		var doc any
		doc, schemaError = jsonschema.UnmarshalJSON(bytes.NewReader(githint.SchemaEvalReportV02))
		if schemaError != nil {
			return
		}
		schemaError = c.AddResource("urn:hint:stored-run:revision", doc)
		if schemaError == nil {
			revisionSchema, schemaError = c.Compile("urn:hint:stored-run:revision#/$defs/WorkUnitRevision")
			if schemaError == nil {
				configurationSchema, schemaError = c.Compile("urn:hint:stored-run:revision#/$defs/Configuration")
			}
		}
	})
}
func validRevision(v map[string]any) bool {
	loadSchemas()
	return schemaError == nil && revisionSchema.Validate(v) == nil
}
func validConfiguration(v any) bool {
	loadSchemas()
	return schemaError == nil && configurationSchema.Validate(v) == nil
}

// strictJSON bounds depth and rejects duplicate members before typed decoding.
func strictJSON(ctx context.Context, raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) bool
	walk = func(depth int) bool {
		if ctx.Err() != nil || depth > 128 {
			return false
		}
		t, e := d.Token()
		if e != nil {
			return false
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				name, ok := k.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim('}')
		case json.Delim('['):
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim(']')
		default:
			_, delim := t.(json.Delim)
			return !delim
		}
	}
	if !walk(0) {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}

// requiredObject preserves explicit received presence. Typed zero values do not
// convert a missing eligibility flag or journal inventory into known absence.
func requiredObject(raw []byte, model any) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	typ := reflect.TypeOf(model)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")
		value, ok := obj[tag[0]]
		optional := len(tag) > 1 && tag[1] == "omitempty"
		if !ok {
			if !optional {
				return nil, false
			}
			continue
		}
		switch field.Type.Kind() {
		case reflect.String, reflect.Int, reflect.Bool:
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, false
			}
		}
	}
	return obj, true
}

// Parse validates exact received bytes and their filename. It does not read a Store,
// resolve a latest version, or establish a trustworthy outcome/configuration/usage.
func Parse(ctx context.Context, raw []byte, filename string, key []byte) (*Run, error) {
	if ctx == nil || ctx.Err() != nil || len(raw) == 0 || len(raw) > 16<<20 || len(key) == 0 || !strictJSON(ctx, raw) {
		return nil, ErrStoredRun
	}
	root, ok := requiredObject(raw, Run{})
	if !ok {
		return nil, ErrStoredRun
	}
	manifest, ok := requiredObject(root["manifest"], Manifest{})
	if !ok {
		return nil, ErrStoredRun
	}
	var receivedMembers []json.RawMessage
	if json.Unmarshal(manifest["members"], &receivedMembers) != nil {
		return nil, ErrStoredRun
	}
	for _, member := range receivedMembers {
		if _, ok := requiredObject(member, Member{}); !ok {
			return nil, ErrStoredRun
		}
	}
	var receivedUnits []json.RawMessage
	if json.Unmarshal(root["units"], &receivedUnits) != nil {
		return nil, ErrStoredRun
	}
	for _, unit := range receivedUnits {
		if _, ok := requiredObject(unit, Unit{}); !ok {
			return nil, ErrStoredRun
		}
	}
	var r Run
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrStoredRun
	}
	m := r.Manifest
	if !runID.MatchString(m.RunID) || filename != m.RunID+".json" || !suiteID.MatchString(m.Suite) || m.Agent == "" || m.RunnerVersion == "" || m.Attempts < 1 || m.Attempts > 1000 || m.Members == nil || len(m.Members) > 10000 || len(r.Units) > 10000 || m.MaxRequests < 0 || m.MaxSeconds < 0 {
		return nil, ErrStoredRun
	}
	if _, e := time.Parse(time.RFC3339Nano, m.CreatedAt); e != nil {
		return nil, ErrStoredRun
	}
	if m.Arm != "baseline" && m.Arm != "configuration" && m.Arm != "no_change" && m.Arm != "context_only" && m.Arm != "refactor" {
		return nil, ErrStoredRun
	}
	if m.Harness != "" && m.Harness != "harbor" && m.Harness != "hint-docker" {
		return nil, ErrStoredRun
	}
	if !validConfiguration(plain(ScopeConfiguration(key, m))) {
		return nil, ErrStoredRun
	}
	// Empty harness retains the producer's historical Harbor normalization.
	if !digest.MatchString(m.LaunchDigest) {
		return nil, ErrStoredRun
	}
	eligible := map[string]Member{}
	members := map[string]bool{}
	for _, mb := range m.Members {
		if ctx.Err() != nil {
			return nil, ErrStoredRun
		}
		if !evalID.MatchString(mb.Eval) || !evalID.MatchString(mb.Baseline) || !digest.MatchString(mb.EvalDigest) || !digest.MatchString(mb.BaselineDigest) || !objectiveID.MatchString(mb.Objective) || mb.TaskFamily == "" || members[mb.Eval] {
			return nil, ErrStoredRun
		}
		members[mb.Eval] = true
		if mb.Eligible {
			eligible[mb.Eval] = mb
		}
	}
	if len(eligible) != len(r.Units) {
		return nil, ErrStoredRun
	}
	seen := map[string]bool{}
	taskOwners := map[string]string{}
	for _, u := range r.Units {
		if ctx.Err() != nil {
			return nil, ErrStoredRun
		}
		if u == nil || seen[u.Eval] {
			return nil, ErrStoredRun
		}
		mb, ok := eligible[u.Eval]
		if !ok {
			return nil, ErrStoredRun
		}
		seen[u.Eval] = true
		ref := func(kind string, parts ...string) string { return hint.EvalRef(key, kind, parts...) }
		if u.UnitRef != ref("work-unit", "controlled", m.RunID, mb.Eval) || len(u.Revisions) == 0 || len(u.Revisions) != len(u.Attempts)+1 || len(u.Attempts) > m.Attempts || len(u.Trials) != len(u.Attempts) || len(u.Reported) > len(u.Revisions) {
			return nil, ErrStoredRun
		}
		sc := u.Scope
		if sc == nil || sc["run_ref"] != ref("run", m.RunID) || sc["eval_ref"] != ref("eval", mb.Eval) || sc["baseline_ref"] != ref("eval", mb.Baseline) || sc["objective_ref"] != ref("objective", mb.Objective) || sc["family_ref"] != ref("family", mb.TaskFamily) || sc["arm"] != m.Arm || sc["planned_attempts"] != float64(m.Attempts) || !reflect.DeepEqual(sc["configuration"], plain(ScopeConfiguration(key, m))) {
			return nil, ErrStoredRun
		}
		scopeDigest, e := hint.Digest(sc)
		if e != nil {
			return nil, ErrStoredRun
		}
		seenOrdinals, seenTrials := map[int]bool{}, map[string]bool{}
		for i, a := range u.Attempts {
			ordinal, ok := a["ordinal"].(float64)
			n := int(ordinal)
			if !ok || ordinal != float64(n) || n < 1 || n > m.Attempts || seenOrdinals[n] || !trialID.MatchString(u.Trials[i]) || seenTrials[u.Trials[i]] || a["attempt_ref"] != ref("attempt", "controlled", m.RunID, mb.Eval, strconv.Itoa(n)) || a["trial_ref"] != ref("trial", u.Trials[i]) {
				return nil, ErrStoredRun
			}
			seenOrdinals[n], seenTrials[u.Trials[i]] = true, true
			ids, ok := a["task_ids"].([]any)
			if !ok || len(ids) != 1 || ids[0] != hint.DeriveID(key, "task", mb.Objective) {
				return nil, ErrStoredRun
			}
			for _, id := range ids {
				task, ok := id.(string)
				if !ok || !refID.MatchString(task) || (taskOwners[task] != "" && taskOwners[task] != ref("objective", mb.Objective)) {
					return nil, ErrStoredRun
				}
				taskOwners[task] = ref("objective", mb.Objective)
			}
		}
		var previous any
		for i, rev := range u.Revisions {
			if ctx.Err() != nil {
				return nil, ErrStoredRun
			}
			attempts, ok := rev["attempts"].([]any)
			state := "open"
			if i == m.Attempts {
				state = "closed"
			}
			if !validRevision(rev) || !ok || len(attempts) != i || rev["unit_ref"] != u.UnitRef || rev["origin"] != "controlled" || rev["revision"] != float64(i+1) || rev["predecessor"] != previous || rev["scope_digest"] != scopeDigest || !reflect.DeepEqual(rev["scope"], sc) || rev["state"] != state || !reflect.DeepEqual(attempts, plain(u.Attempts[:i])) {
				return nil, ErrStoredRun
			}
			body := map[string]any{}
			for k, v := range rev {
				if k != "revision_digest" {
					body[k] = v
				}
			}
			dg, e := hint.Digest(body)
			if e != nil || rev["revision_digest"] != dg {
				return nil, ErrStoredRun
			}
			previous = dg
		}
		if u.State != u.Revisions[len(u.Revisions)-1]["state"] {
			return nil, ErrStoredRun
		}
		reported := map[string]bool{}
		for _, dg := range u.Reported {
			if reported[dg] {
				return nil, ErrStoredRun
			}
			reported[dg] = true
			found := false
			for _, rev := range u.Revisions {
				if rev["revision_digest"] == dg {
					found = true
				}
			}
			if !found {
				return nil, ErrStoredRun
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ErrStoredRun
	}
	return &r, nil
}

// Ownership returns only the actual unit/attempt/task refs in this received journal.
// Task holders retain their objective domain across distinct runs. This is not
// independence or usage admission. Arbitrary plan strings never become ownership.
func (r *Run) Ownership() map[string]string {
	out := map[string]string{}
	for _, u := range r.Units {
		out[u.UnitRef] = "unit:" + u.UnitRef
		for _, a := range u.Attempts {
			out[fmt.Sprint(a["attempt_ref"])] = "attempt:" + u.UnitRef
			for _, id := range a["task_ids"].([]any) {
				out[fmt.Sprint(id)] = "task:" + fmt.Sprint(u.Scope["objective_ref"])
			}
		}
	}
	return out
}
