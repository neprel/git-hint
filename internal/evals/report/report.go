// Package report builds and checks minimized eval result reports (evalreport v0.1).
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
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/version"
)

// Version of the report format.
const Version = "0.1.0"

// MediaType of a report on the wire.
const MediaType = "application/vnd.hint-evalreport+json"

var (
	once    sync.Once
	schema  *jsonschema.Schema
	errLoad error
)

func compiled() (*jsonschema.Schema, error) {
	once.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(githint.SchemaEvalReportV01))
		if err != nil {
			errLoad = err
			return
		}
		if err = c.AddResource("urn:hint:schema:evalreport:v0.1:report", doc); err != nil {
			errLoad = err
			return
		}
		schema, errLoad = c.Compile("urn:hint:schema:evalreport:v0.1:report")
	})
	return schema, errLoad
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
	if v, _ := raw["report_version"].(string); v != Version {
		return fmt.Errorf("%w: unsupported report version", ErrInvalid)
	}
	s, err := compiled()
	if err != nil {
		return err
	}
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
	return nil
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
}

func (b Builder) ref(kind string, parts ...string) string {
	return hint.DeriveID(b.Key, "eval-report:"+kind, parts...)
}

var hexRef = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Build makes a report of the given trial records (names as stored). It returns the
// serialized report and the trial names it holds.
func (b Builder) Build(l *library.Library, trialNames []string, now time.Time) ([]byte, []string, error) {
	if !hexRef.MatchString(b.Namespace) || len(b.Key) == 0 {
		return nil, nil, errors.New("report builder without a project key")
	}
	sort.Strings(trialNames)
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
		trials = append(trials, map[string]any{
			"trial_ref": b.ref("trial", n), "suite_ref": b.ref("suite", suite), "eval_ref": b.ref("eval", evalRef),
			"task_family_ref": b.ref("family", str(member["task_family"])), "family": ev["family"], "partition": member["partition"],
			"arm": t["arm"], "qualification": map[string]any{"policy": sr["qualification_policy"], "state": ev["state"], "basis": "client_claim"},
			"attempt": t["attempt"], "agent": agent, "runner": t["runner"], "outcome": t["outcome"], "checks": checks,
			"counters": map[string]any{"requests": counter("requests"), "retries": counter("retries"), "input_tokens": counter("input_tokens"),
				"output_tokens": counter("output_tokens"), "cache_tokens": counter("cache_tokens"), "wall_seconds": counter("wall_seconds"),
				"agent_seconds": counter("agent_seconds")},
			"budget": map[string]any{"max_requests": maxReq, "max_seconds": bud["max_seconds"], "stop_reason": bud["stop_reason"]},
		})
		used = append(used, n)
	}
	if trials == nil {
		trials = []any{}
	}
	created := now.UTC().Truncate(time.Hour).Format(time.RFC3339)
	r := map[string]any{"report_version": Version, "report_id": b.ref("report", created, fmt.Sprint(used)), "project_namespace": b.Namespace,
		"created_at": created, "producer": map[string]any{"tool": "git-hint", "version": version.Info().Version}, "trials": trials}
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
