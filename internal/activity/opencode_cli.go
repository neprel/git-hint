package activity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

const (
	CLIRootVersion      = "0.1.0"
	CLIRootAgentVersion = "2.0.26"
	maxCLICaptureBytes  = 64 << 20
	maxCLILineBytes     = 16 << 20
)

// CLIRootOptions are supplied by the orchestrator, not read from CLI assertions. Resolve
// maps the actual frozen /app baseline. Configuration and all bindings must be genuine.
type CLIRootOptions struct {
	Key             []byte
	AgentVersion    string
	Configuration   *Configuration
	TaskID          string
	TaskFamilyID    string
	RunRef          string
	Cwd             string
	Resolve         Resolver
	CaptureComplete bool      // the caller observed the complete capture; step_finish is not proof
	CapturedAt      time.Time // optional import diagnostic only; never an event timestamp
}

// CLIRootDiagnostics describe only this observed root terminal-tool capture. Counts do not
// establish child-session coverage, missing-record absence or model-request completeness.
type CLIRootDiagnostics struct {
	Scope             string `json:"scope"`
	ImportCapturedAt  string `json:"import_captured_at,omitempty"`
	RootSessionRef    string `json:"root_session_ref"`
	Records           int    `json:"records"`
	TerminalTools     int    `json:"terminal_tools"`
	FailedTools       int    `json:"failed_tools"`
	DuplicateTools    int    `json:"duplicate_tools"`
	IgnoredRecords    int    `json:"ignored_records"`
	StepStarts        int    `json:"step_starts"`
	StepFinishes      int    `json:"step_finishes"`
	MissingStepFinish bool   `json:"missing_step_finish"`
	Delegates         int    `json:"delegates"`
	UnknownEffects    int    `json:"unknown_effects"`
	UnknownReadSizes  int    `json:"unknown_read_sizes"`
}

// CLIRootImport is returned only after the entire capture passes integrity checks. Callers
// must persist Source and Observations together. The decoder never writes a spool.
type CLIRootImport struct {
	Source       Source             `json:"source"`
	Observations []Observation      `json:"observations"`
	Diagnostics  CLIRootDiagnostics `json:"diagnostics"`
}

// CLIRootError carries bounded codes and record numbers, never raw source/error text.
type CLIRootError struct {
	Code   string
	Record int
}

func (e *CLIRootError) Error() string {
	return fmt.Sprintf("CLI capture rejected: %s (record %d).", e.Code, e.Record)
}

var cliOpaque = regexp.MustCompile(`^[0-9a-f]{32}$`)

func cliConfigurationValid(c Configuration) bool {
	version := func(v VersionState) bool {
		if v.State == "unknown" || v.State == "not_applicable" {
			return v.Value == ""
		}
		return v.State == "known" && v.Value != "" && len(v.Value) <= 128 && !strings.ContainsAny(v.Value, " /\\\r\n\t")
	}
	limit := func(v Limit) bool {
		return v.State == "known" && v.Value > 0 || (v.State == "unknown" || v.State == "unbounded") && v.Value == 0
	}
	if !version(c.Agent.Version) || !version(c.Harness.Version) || !slices.Contains([]string{"harbor", "hint-docker", "direct", "other"}, c.Harness.Kind) || !limit(c.Budget.MaxRequests) || !limit(c.Budget.MaxSeconds) {
		return false
	}
	if c.Model.State == "known" {
		if !cliOpaque.MatchString(c.Model.Ref) || !slices.Contains([]string{"launch_intent", "launch_parameter", "agent_reported"}, c.Model.Provenance) {
			return false
		}
	} else if c.Model.State != "unknown" || c.Model.Ref != "" || c.Model.Provenance != "" {
		return false
	}
	if c.ContextPolicy.State == "enabled" {
		if !cliOpaque.MatchString(c.ContextPolicy.Ref) || c.ContextPolicy.Version < 1 {
			return false
		}
	} else if !slices.Contains([]string{"disabled", "unknown", "not_applicable"}, c.ContextPolicy.State) || c.ContextPolicy.Ref != "" || c.ContextPolicy.Version != 0 {
		return false
	}
	if c.ConfigRefs.State != "known" && c.ConfigRefs.State != "unknown" || c.ConfigRefs.State == "unknown" && len(c.ConfigRefs.Refs) != 0 || len(c.ConfigRefs.Refs) > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range c.ConfigRefs.Refs {
		if !cliOpaque.MatchString(ref) || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	if c.Declared != nil && c.Declared.Model != nil {
		d := c.Declared.Model
		if d.State == "known" {
			if !cliOpaque.MatchString(d.Ref) {
				return false
			}
		} else if d.State != "unknown" || d.Ref != "" {
			return false
		}
	}
	return true
}

// CLIRootSource returns the exact source declaration of this run and configuration.
func CLIRootSource(o CLIRootOptions) (Source, error) {
	bad := func() (Source, error) { return Source{}, &CLIRootError{Code: "options_invalid"} }
	if len(o.Key) != 32 || o.AgentVersion != CLIRootAgentVersion || o.Configuration == nil ||
		!cliOpaque.MatchString(o.TaskID) || !cliOpaque.MatchString(o.TaskFamilyID) || !cliOpaque.MatchString(o.RunRef) ||
		o.Cwd != "/app" || o.Resolve == nil {
		return bad()
	}
	cfg := *o.Configuration
	if !cliConfigurationValid(cfg) || cfg.Agent.Name != "opencode" || cfg.Agent.Version.State != "known" || cfg.Agent.Version.Value != CLIRootAgentVersion {
		return bad()
	}
	// Clone pointer/slice fields, so later caller mutation cannot rewrite the returned source.
	cfg.ConfigRefs.Refs = slices.Clone(cfg.ConfigRefs.Refs)
	if cfg.Declared != nil {
		decl := *cfg.Declared
		if decl.Model != nil {
			model := *decl.Model
			decl.Model = &model
		}
		cfg.Declared = &decl
	}
	version := CLIRootAgentVersion
	return Source{SourceID: hint.DeriveID(o.Key, "source", hint.CLIRootAdapter, CLIRootVersion, version,
		"configuration", cfg.Digest(), "run", o.RunRef, "task", o.TaskID, "family", o.TaskFamilyID),
		Agent: "opencode", AgentVersion: &version, Adapter: Adapter{Name: hint.CLIRootAdapter, Version: CLIRootVersion}, Configuration: &cfg,
		Capabilities: []string{"time.start", "time.end", "targets", "outcome.status", "outcome.exit_code", "size", "content_delivery",
			"command_class", "correlation.session_id", "correlation.tool_call_id", "correlation.task_id", "correlation.task_family_id", "correlation.run_ref"},
		Unavailable: []string{"usage", "request_kind", "collection", "correlation.turn_id", "correlation.trace_id", "correlation.span_id", "coordination", "context"}}, nil
}

func cliNumber(value any) (float64, bool) {
	if n, ok := value.(json.Number); ok {
		f, err := n.Float64()
		return f, err == nil
	}
	f, ok := value.(float64)
	return f, ok
}

func cliMS(value any) (int64, bool) {
	f, ok := cliNumber(value)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f <= 0 || f > 253402300799999 {
		return 0, false
	}
	return int64(f), true
}

func cliRawID(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok && s != "" && len(s) <= 256 && !strings.ContainsRune(s, 0)
}

func cliExit(metadata map[string]any) (*int, bool) {
	var result *int
	nested, _ := metadata["metadata"].(map[string]any)
	for _, m := range []map[string]any{metadata, nested} {
		value, exists := m["exit"]
		if !exists || value == nil {
			continue
		}
		f, ok := cliNumber(value)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
			return nil, false
		}
		x := int(f)
		if result != nil && *result != x {
			return nil, false
		}
		result = &x
	}
	return result, true
}

// cliJSONValue rejects duplicate object keys at every depth. The general schema decoder
// accepts last-key-wins JSON, which is unsuitable for an atomic capture integrity check.
func cliJSONValue(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key")
			}
			if _, exists := object[name]; exists {
				return nil, fmt.Errorf("JSON duplicate key")
			}
			value, err := cliJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("JSON object end")
		}
		return object, nil
	case json.Delim('['):
		array := []any{}
		for d.More() {
			value, err := cliJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("JSON array end")
		}
		return array, nil
	default:
		if _, delim := token.(json.Delim); delim {
			return nil, fmt.Errorf("JSON unexpected delimiter")
		}
		return token, nil
	}
}

func decodeCLILine(line []byte) (any, error) {
	if !utf8.Valid(line) {
		return nil, fmt.Errorf("JSON invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.UseNumber()
	value, err := cliJSONValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON trailing data")
	}
	return value, nil
}

// DecodeCLIRoot consumes OpenCode 2.0.26 run --format json records directly. Tool intervals
// are observed state intervals (possibly starting at input tracking), not execution durations.
// Missing step_finish never creates a closure. Provider executed flags are not success flags.
func DecodeCLIRoot(r io.Reader, o CLIRootOptions) (CLIRootImport, error) {
	reject := func(code string, record int) (CLIRootImport, error) {
		return CLIRootImport{}, &CLIRootError{Code: code, Record: record}
	}
	source, err := CLIRootSource(o)
	if err != nil {
		return CLIRootImport{}, err
	}
	if !o.CaptureComplete {
		return reject("capture_not_complete", 0)
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxCLICaptureBytes+1))
	if err != nil {
		return reject("capture_read_failed", 0)
	}
	if len(raw) == 0 || len(raw) > maxCLICaptureBytes {
		return reject("capture_size_invalid", 0)
	}
	if raw[len(raw)-1] != '\n' {
		return reject("capture_unterminated", 0)
	}
	out := CLIRootImport{Source: source, Observations: []Observation{}, Diagnostics: CLIRootDiagnostics{Scope: "root_session_terminal_tools"}}
	if !o.CapturedAt.IsZero() {
		out.Diagnostics.ImportCapturedAt = Timestamp(o.CapturedAt)
	}
	seen, seenParts, root := map[string]string{}, map[string]string{}, ""
	for index, line := range bytes.Split(raw[:len(raw)-1], []byte{'\n'}) {
		n := index + 1
		if len(line) == 0 || len(line) > maxCLILineBytes {
			return reject("record_size_invalid", n)
		}
		decoded, err := decodeCLILine(line)
		if err != nil {
			return reject("record_json_invalid", n)
		}
		record, ok := decoded.(map[string]any)
		if !ok {
			return reject("record_invalid", n)
		}
		session, ok := cliRawID(record["sessionID"])
		if !ok {
			return reject("session_invalid", n)
		}
		if root == "" {
			root = session
		} else if session != root {
			return reject("multiple_root_sessions", n)
		}
		timestamp, ok := cliMS(record["timestamp"])
		if !ok {
			return reject("timestamp_invalid", n)
		}
		kind, _ := record["type"].(string)
		out.Diagnostics.Records++
		if kind != "tool_use" {
			switch kind {
			case "step_start":
				out.Diagnostics.StepStarts++
			case "step_finish":
				out.Diagnostics.StepFinishes++
			case "text", "reasoning":
			default:
				return reject("record_type_unsupported", n)
			}
			out.Diagnostics.IgnoredRecords++
			continue
		}
		part, ok := record["part"].(map[string]any)
		if !ok || part["type"] != "tool" || part["sessionID"] != session {
			return reject("tool_binding_invalid", n)
		}
		call, callOK := cliRawID(part["id"])
		message, messageOK := cliRawID(part["messageID"])
		partID, partOK := cliRawID(part["partID"])
		tool, toolOK := cliRawID(part["tool"])
		if !callOK || !messageOK || !partOK || !toolOK {
			return reject("tool_identity_invalid", n)
		}
		state, ok := part["state"].(map[string]any)
		if !ok {
			return reject("tool_state_invalid", n)
		}
		status := state["status"]
		if status != "completed" && status != "error" {
			return reject("tool_not_terminal", n)
		}
		args, ok := state["input"].(map[string]any)
		if !ok {
			return reject("tool_input_invalid", n)
		}
		window, ok := state["time"].(map[string]any)
		if !ok {
			return reject("tool_time_invalid", n)
		}
		start, startOK := cliMS(window["start"])
		end, endOK := cliMS(window["end"])
		if !startOK || !endOK || end < start || timestamp < end {
			return reject("tool_time_invalid", n)
		}
		digest, err := hint.Digest(record)
		if err != nil {
			return reject("record_digest_invalid", n)
		}
		key := session + "\x00" + message + "\x00" + call
		partKey := session + "\x00" + partID
		if previous, exists := seenParts[partKey]; exists && previous != digest {
			return reject("tool_duplicate_conflict", n)
		}
		if previous, exists := seen[key]; exists {
			if previous != digest {
				return reject("tool_duplicate_conflict", n)
			}
			out.Diagnostics.DuplicateTools++
			continue
		}
		seen[key] = digest
		seenParts[partKey] = digest
		effect := ClassifyTool(tool, args)
		metadata, _ := state["metadata"].(map[string]any)
		rawTargets := slices.Clone(effect.Targets)
		if effect.Op == "EDIT_INTENT" {
			inner, _ := metadata["metadata"].(map[string]any)
			for _, m := range []map[string]any{metadata, inner} {
				files, _ := m["files"].([]any)
				for _, file := range files {
					if f, ok := file.(map[string]any); ok {
						rawTargets = append(rawTargets, str(f, "filePath", "file", "path"))
					}
				}
			}
		}
		targets := ResolveTargets(shellBase(o.Cwd, tool, args), rawTargets, o.Resolve)
		for _, target := range targets {
			if !cliOpaque.MatchString(target.EntityID) {
				return reject("resolver_reference_invalid", n)
			}
		}
		a, b := Timestamp(time.UnixMilli(start)), Timestamp(time.UnixMilli(end))
		eventTime := Timestamp(time.UnixMilli(timestamp))
		obs := Observation{Class: "observation", ObservationID: hint.DeriveID(o.Key, "event", hint.CLIRootAdapter, session, message, call),
			Op: effect.Op, OpsSecondary: slices.Clone(effect.Secondary), Source: "log", SourceID: source.SourceID, Precision: effect.Precision,
			Time: Time{Start: &a, End: &b, Clock: "agent"}, Targets: targets, Outcome: Outcome{Status: "success"}, CommandClass: effect.CommandClass,
			Correlation: map[string]string{"session_id": hint.DeriveID(o.Key, "session", hint.CLIRootAdapter, session),
				"tool_call_id": hint.DeriveID(o.Key, "call", hint.CLIRootAdapter, session, message, call), "task_id": o.TaskID, "task_family_id": o.TaskFamilyID, "run_ref": o.RunRef},
			Missing: append([]string{"correlation.turn_id"}, effect.Missing...),
			Provenance: Provenance{CapturedAt: eventTime, SourceEventID: hint.DeriveID(o.Key, "source-event", hint.CLIRootAdapter, session, message, call),
				Transformations: []Transform{{Step: "normalize", Tool: "hint-cli", Version: CLIRootVersion}, {Step: "redact", Tool: "hint-cli", Version: CLIRootVersion}}}}
		if status == "error" {
			obs.Outcome.Status = "failure"
		}
		if isShellTool(tool) {
			exit, valid := cliExit(metadata)
			if !valid {
				return reject("tool_exit_invalid", n)
			}
			obs.Outcome.ExitCode = exit
			if exit != nil && *exit != 0 {
				obs.Outcome.Status = "failure"
			}
			if exit == nil {
				obs.Missing = append(obs.Missing, "outcome.exit_code")
			}
		}
		// An errored content/navigation tool may have read only part of its input. The
		// terminal error does not prove zero reading or complete navigation coverage.
		if obs.Outcome.Status == "failure" && (slices.Contains([]string{"READ", "SEARCH", "DISCOVER"}, obs.Op) || slices.Contains(obs.OpsSecondary, "READ")) && !slices.Contains(obs.OpsSecondary, "UNKNOWN") {
			obs.OpsSecondary = append(obs.OpsSecondary, "UNKNOWN")
			slices.Sort(obs.OpsSecondary)
		}
		if output, known := state["output"].(string); known && obs.Outcome.Status == "success" {
			// scanner is the schema's generic exact measurement method: this counts
			// only decoded CLI output bytes, never file size or model delivery.
			quantity := scan.Known("bytes", float64(len(output)), "scanner")
			obs.Size = &quantity
		} else {
			obs.Missing = append(obs.Missing, "size")
		}
		if obs.Op == "READ" || slices.Contains(obs.OpsSecondary, "READ") {
			obs.ContentDelivery = "unknown"
			if obs.Size == nil {
				out.Diagnostics.UnknownReadSizes++
			}
		}
		if obs.Op == "DELEGATE" {
			obs.OpsSecondary = append(obs.OpsSecondary, "UNKNOWN")
			out.Diagnostics.Delegates++
		}
		if slices.Contains(obs.OpsSecondary, "UNKNOWN") || obs.CommandClass == "UNKNOWN" {
			out.Diagnostics.UnknownEffects++
		}
		if len(rawTargets) > 0 && len(targets) == 0 || len(rawTargets) == 0 && targetOp(obs.Op) {
			obs.Missing = append(obs.Missing, "targets")
		}
		slices.Sort(obs.Missing)
		obs.Missing = slices.Compact(obs.Missing)
		out.Observations = append(out.Observations, obs)
		out.Diagnostics.TerminalTools++
		if obs.Outcome.Status == "failure" {
			out.Diagnostics.FailedTools++
		}
	}
	out.Diagnostics.RootSessionRef = hint.DeriveID(o.Key, "session", hint.CLIRootAdapter, root)
	out.Diagnostics.MissingStepFinish = out.Diagnostics.StepStarts > out.Diagnostics.StepFinishes
	return out, nil
}
