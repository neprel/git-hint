package activity

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/shellclass"
)

// Resolver maps a cleaned absolute path to the opaque ID of a file in a configured
// repository. It returns false for a path outside every repository.
type Resolver func(abs string) (string, bool)

// Normalizer maps reduced OpenCode plugin events to observations.
type Normalizer struct {
	Key          []byte
	SourceID     string
	Cwd          string // project directory of the OpenCode session
	Resolve      Resolver
	Now          func() time.Time
	AgentVersion string
	// TaskID is the opaque ID of the task that the orchestrator declared for this agent run
	// (DeriveID(key, "task", raw), as hint_collect derives it); empty: undeclared.
	TaskID string
	// TaskFamilyID is the declared family of that task (HINT_TASK_FAMILY, FamilyRef); empty:
	// not declared.
	TaskFamilyID string
	// RunRef is the opaque reference of the agent run (HINT_RUN_REF, from the runner); empty:
	// the run is not bound to a model ledger.
	RunRef string
	// OnStats receives the counter lines of the plugin (hook "hint.stats"), if set.
	OnStats func(line []byte)
	// ModelCalls maps the model-call events (MODEL_CALL, USAGE, session closure; draft v0.2)
	// of an OpenCode version whose events were checked (UsageAgentVersion); otherwise those
	// lines are skipped.
	ModelCalls bool
	// Acknowledged reports, at the plugin's closure line, that every preceding record was
	// written (no failed or discarded write); without it a closure is never flushed.
	Acknowledged func() bool
	// RootSessions are the raw OpenCode root session IDs that the orchestrator declared for this
	// run (HINT_OPENCODE_SESSION); only they and their observed descendants are owned (C7BE-18).
	RootSessions []string
}

// SourceOptions describe one OpenCode agent run as the orchestrator declared it.
type SourceOptions struct {
	AgentVersion   string
	TaskDeclared   bool           // HINT_TASK_ID: every record of the run carries the task
	FamilyDeclared bool           // HINT_TASK_FAMILY: and its declared family
	RunDeclared    bool           // HINT_RUN_REF: and the opaque reference of the run
	Configuration  *Configuration // the canonical effective configuration, if built
	// ModelCalls: the plugin forwards the model-call events of a checked OpenCode version
	// (UsageAgentVersion): the source reports usage under UsageNormalization (draft v0.2).
	ModelCalls bool
}

// OpenCodeSource returns the source entry of the OpenCode plugin adapter. A declared task or
// family and the effective configuration are part of the source identity (another identity
// and capability list), never a model fact: a changed effective configuration is a new source.
func OpenCodeSource(key []byte, o SourceOptions) Source {
	// the adapter version is part of the identity: another version can have other capabilities
	mode := "undeclared-task"
	if o.TaskDeclared {
		mode = "declared-task"
	}
	parts := []string{"opencode-plugin", o.AgentVersion, Version, mode}
	if o.FamilyDeclared {
		parts = append(parts, "declared-family")
	}
	if o.RunDeclared {
		parts = append(parts, "declared-run")
	}
	if o.Configuration != nil {
		parts = append(parts, "configuration", o.Configuration.Digest())
	}
	if o.ModelCalls { // the usage normalization is part of the source identity
		parts = append(parts, "model-calls", UsageNormalization)
	}
	s := Source{SourceID: hint.DeriveID(key, "source", parts...), Agent: "opencode",
		Adapter: Adapter{Name: "opencode-plugin", Version: Version},
		Capabilities: []string{"time.start", "time.end", "correlation.session_id", "correlation.tool_call_id",
			"targets", "outcome.status", "outcome.exit_code", "size", "command_class", "content_delivery", "context"},
		Unavailable: []string{"correlation.task_id", "correlation.turn_id", "correlation.trace_id",
			"correlation.span_id", "usage", "coordination"},
		Configuration: o.Configuration}
	if o.TaskDeclared {
		s.Capabilities = append(s.Capabilities, "correlation.task_id")
		s.Unavailable = s.Unavailable[1:]
	}
	if o.FamilyDeclared {
		s.Capabilities = append(s.Capabilities, "correlation.task_family_id")
	}
	if o.RunDeclared {
		s.Capabilities = append(s.Capabilities, "correlation.run_ref")
	}
	if o.ModelCalls {
		s.Capabilities = append(s.Capabilities, "usage", "request_kind", "correlation.call_ref", "collection")
		s.Unavailable = slices.DeleteFunc(s.Unavailable, func(f string) bool { return f == "usage" })
		s.UsageNormalization = UsageNormalization
	}
	if o.AgentVersion != "" {
		s.AgentVersion = &o.AgentVersion
	} else {
		s.Unavailable = append(s.Unavailable, "agent_version")
	}
	return s
}

// ocEvent is one reduced plugin event (see adapters/opencode/hint-adapter.ts). The plugin
// keeps an allowlist of argument fields and replaces tool output by its byte length.
type ocEvent struct {
	Hook  string `json:"hook"`
	TWall int64  `json:"t_wall_ms"`
	Input struct {
		Tool      string `json:"tool"`
		SessionID string `json:"sessionID"`
		CallID    string `json:"callID"`
		Kind      string `json:"kind"` // model.request: the request kind
	} `json:"input"`
	Event  *ocModelEvent `json:"event"` // hook "event": one reduced bus event
	Close  *ocClose      `json:"close"` // hook "hint.close"
	Output struct {
		Args           map[string]any  `json:"args"`
		Output         any             `json:"output"`
		Metadata       map[string]any  `json:"metadata"`
		Status         string          `json:"status"`
		Context        json.RawMessage `json:"context"`
		ContextInvalid bool            `json:"context_invalid"`
	} `json:"output"`
}

type effect struct {
	op        string
	secondary []string
	precision string
	source    string
	class     string // command class of a shell call
	targets   []string
	missing   []string
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ContextTools are the OpenCode names of the hint_context tool of the git-hint MCP server
// ("<server>_hint_context"; the documented server name is githint). Only their calls can carry
// a context measurement.
var ContextTools = []string{"githint_hint_context"}

func isContextTool(tool string) bool { return slices.Contains(ContextTools, tool) }

func isShellTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash", "shell", "exec_command", "local_shell":
		return true
	}
	return false
}

// toolEffect maps a tool name and its arguments. Native tools give exact operations.
// Shell commands go through the syntax-only classifier; they are inferred, and a command
// without a rule stays UNKNOWN.
func toolEffect(tool string, args map[string]any) effect {
	nonEmpty := func(s string) []string {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	switch strings.ToLower(tool) {
	case "read":
		return effect{op: "READ", precision: "exact", source: "plugin", targets: nonEmpty(str(args, "filePath", "file_path", "path"))}
	case "grep":
		return effect{op: "SEARCH", precision: "exact", source: "plugin", targets: nonEmpty(str(args, "path"))}
	case "glob", "list", "ls":
		return effect{op: "DISCOVER", precision: "exact", source: "plugin", targets: nonEmpty(str(args, "path"))}
	case "edit", "write", "multiedit", "apply_patch", "patch":
		return effect{op: "EDIT_INTENT", precision: "exact", source: "plugin", targets: nonEmpty(str(args, "filePath", "file_path", "path"))}
	case "task", "agent", "subagent":
		return effect{op: "DELEGATE", precision: "exact", source: "plugin", missing: []string{"coordination"}}
	}
	if isContextTool(tool) {
		return effect{op: "DISCOVER", precision: "exact", source: "plugin"}
	}
	if isShellTool(tool) {
		cmd := str(args, "command", "cmd")
		if cmd == "" {
			return effect{op: "EXECUTE", precision: "unknown", source: "plugin", class: "UNKNOWN", missing: []string{"command_class"}}
		}
		return shellEffect(cmd)
	}
	if slices.Contains(localTools, strings.ToLower(tool)) {
		return effect{op: "TOOL", precision: "exact", source: "plugin"}
	}
	// any other tool (MCP, skills, web tools) may deliver content: its further effects are unknown
	return effect{op: "TOOL", precision: "exact", source: "plugin", secondary: []string{"UNKNOWN"}}
}

// localTools are tools that work only on the agent's own local state (a todo list, a question to
// the user) and never deliver repository or remote content.
var localTools = []string{"todowrite", "todoread", "question"}

var labelOp = map[string]string{
	shellclass.READ: "READ", shellclass.SEARCH: "SEARCH", shellclass.LIST: "DISCOVER", shellclass.TEST: "TEST",
	shellclass.BUILD: "BUILD", shellclass.WRITE: "EDIT_INTENT", shellclass.DELETE: "EDIT_INTENT",
	shellclass.EXECUTE: "EXECUTE",
}

var labelClass = map[string]string{
	shellclass.READ: "READ", shellclass.WRITE: "WRITE", shellclass.DELETE: "WRITE", shellclass.SEARCH: "SEARCH",
	shellclass.LIST: "LIST", shellclass.TEST: "TEST", shellclass.BUILD: "BUILD", shellclass.VCS: "VCS",
	shellclass.NETWORK: "NETWORK",
}

func shellEffect(cmd string) effect {
	r := shellclass.Classify(cmd)
	e := effect{precision: "inferred", source: "shell_parser"}
	if !r.DynamicTargets {
		e.targets = append(slices.Clone(r.ReadTargets), r.WriteTargets...)
	}
	switch {
	case slices.Contains(r.Labels, shellclass.UNKNOWN):
		e.op, e.class, e.precision = "EXECUTE", "UNKNOWN", "unknown"
		return e
	case len(r.Labels) > 1:
		e.class = "MIXED"
	default:
		e.class = labelClass[r.Labels[0]]
		if e.class == "" {
			e.class = "UNKNOWN"
		}
	}
	switch {
	case slices.Contains(r.Labels, shellclass.TEST):
		e.op = "TEST"
	case slices.Contains(r.Labels, shellclass.BUILD):
		e.op = "BUILD"
	case len(r.Labels) == 1 && labelOp[r.Labels[0]] != "":
		e.op = labelOp[r.Labels[0]]
	default:
		e.op = "EXECUTE"
	}
	for _, l := range r.Labels {
		if op := labelOp[l]; op != "" && op != e.op && !slices.Contains(e.secondary, op) {
			e.secondary = append(e.secondary, op)
		}
	}
	slices.Sort(e.secondary)
	return e
}

// shellBase is the directory against which a shell command's relative paths resolve:
// the tool's workdir argument (absolute, or relative to cwd), else cwd.
func shellBase(cwd, tool string, args map[string]any) string {
	wd := str(args, "workdir", "cwd")
	if wd == "" || !isShellTool(tool) {
		return cwd
	}
	if path.IsAbs(wd) || cwd == "" {
		return path.Clean(wd)
	}
	return path.Join(cwd, wd)
}

func (n *Normalizer) targets(base string, raw []string) []Target {
	var out []Target
	seen := map[string]bool{}
	for _, p := range raw {
		if p == "" {
			continue
		}
		if !path.IsAbs(p) {
			if base == "" {
				continue
			}
			p = path.Join(base, p)
		}
		id, ok := n.Resolve(path.Clean(p))
		if ok && !seen[id] {
			seen[id] = true
			out = append(out, Target{EntityID: id})
		}
	}
	return out
}

// redactedLen reads "[redacted:len=N]" (the plugin form) or measures a raw string.
func redactedLen(s string) int {
	if v, ok := strings.CutPrefix(s, "[redacted:len="); ok {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "]")); err == nil && n >= 0 {
			return n
		}
	}
	return len(s)
}

type open struct {
	obs  Observation
	base string
}

// Stream reads plugin events and calls emit for each finished tool call: at the after
// event, or at the end of input for a call without one (OpenCode 2.x calls the after hook
// also for failures; a call cut off by the exit has no end time). If emit fails, Stream
// stops and returns the error: a record never counts as kept when it was not written.
// A malformed line is skipped and counted.
func (n *Normalizer) Stream(r io.Reader, emit func(Observation) error) (skipped int, err error) {
	pending := map[string]*open{}
	var order []string
	mc := newCalls(n.RootSessions)
	emitAll := func(l []Observation) error {
		for _, o := range l {
			if err := emit(o); err != nil {
				return fmt.Errorf("emit: %w", err)
			}
		}
		return nil
	}
	// tool handles one tool hook event (an owned session's, see ownedTool)
	tool := func(e ocEvent) error {
		if e.Input.CallID == "" {
			skipped++
			return nil
		}
		key := e.Input.SessionID + "\x00" + e.Input.CallID
		switch e.Hook {
		case "tool.execute.before":
			if _, dup := pending[key]; dup {
				return nil
			}
			ef := toolEffect(e.Input.Tool, e.Output.Args)
			base := shellBase(n.Cwd, e.Input.Tool, e.Output.Args)
			start := Timestamp(time.UnixMilli(e.TWall))
			o := Observation{
				ObservationID: hint.DeriveID(n.Key, "event", n.SourceID, e.Input.SessionID, e.Input.CallID),
				Class:         "observation", Op: ef.op, OpsSecondary: ef.secondary, Source: ef.source,
				SourceID: n.SourceID, Precision: ef.precision,
				Time: Time{Start: &start, Clock: "agent"},
				Correlation: map[string]string{
					"session_id":   hint.DeriveID(n.Key, "session", n.SourceID, e.Input.SessionID),
					"tool_call_id": hint.DeriveID(n.Key, "call", n.SourceID, e.Input.SessionID, e.Input.CallID)},
				Targets:      n.targets(base, ef.targets),
				Outcome:      Outcome{Status: "unknown"},
				CommandClass: ef.class,
				Missing:      append([]string{"correlation.task_id", "correlation.turn_id"}, ef.missing...),
			}
			if e.Input.SessionID == "" {
				delete(o.Correlation, "session_id")
				o.Missing = append(o.Missing, "correlation.session_id")
			}
			if n.TaskID != "" { // declared for the whole run by the orchestrator
				o.Correlation["task_id"] = n.TaskID
				if n.TaskFamilyID != "" {
					o.Correlation["task_family_id"] = n.TaskFamilyID
				}
				if n.RunRef != "" {
					o.Correlation["run_ref"] = n.RunRef
				}
				o.Missing = slices.DeleteFunc(o.Missing, func(s string) bool { return s == "correlation.task_id" })
			}
			if ef.op == "READ" {
				// the plugin sees the returned text; delivery to the model is not proven
				o.ContentDelivery = "unknown"
			}
			if ef.op == "DELEGATE" {
				o.Coordination = &Coordination{}
			}
			if len(ef.targets) > 0 && len(o.Targets) == 0 || len(ef.targets) == 0 && targetOp(ef.op) {
				o.Missing = append(o.Missing, "targets")
			}
			pending[key] = &open{obs: o, base: base}
			order = append(order, key)
		case "tool.execute.after":
			p, ok := pending[key]
			if !ok {
				return nil
			}
			delete(pending, key)
			o := p.obs
			end := Timestamp(time.UnixMilli(e.TWall))
			o.Time.End = &end
			// apply_patch names its files only in the after event
			if files, ok := e.Output.Metadata["files"].([]any); ok {
				var raw []string
				for _, f := range files {
					if fm, ok := f.(map[string]any); ok {
						raw = append(raw, str(fm, "filePath", "file", "path"))
					}
				}
				for _, t := range n.targets(p.base, raw) {
					if !slices.Contains(o.Targets, t) {
						o.Targets = append(o.Targets, t)
					}
				}
				if len(o.Targets) > 0 {
					o.Missing = slices.DeleteFunc(o.Missing, func(s string) bool { return s == "targets" })
				}
			}
			o.Outcome.Status = "success"
			if e.Output.Status == "error" {
				o.Outcome.Status = "failure"
			}
			if x, ok := e.Output.Metadata["exit"].(float64); ok {
				c := int(x)
				o.Outcome.ExitCode = &c
				if c != 0 {
					o.Outcome.Status = "failure"
				}
			} else if isShellOp(o) {
				o.Missing = append(o.Missing, "outcome.exit_code")
			}
			delivered := -1
			if s, ok := e.Output.Output.(string); ok {
				delivered = redactedLen(s)
				q := scan.Known("bytes", float64(delivered), "plugin_output_length")
				o.Size = &q
			} else {
				o.Missing = append(o.Missing, "size")
			}
			raw, ok := n.contextStages(&o, e, delivered)
			if err := emit(n.finish(o)); err != nil {
				return fmt.Errorf("emit: %w", err)
			}
			if ok {
				if err := emit(n.finish(raw)); err != nil {
					return fmt.Errorf("emit: %w", err)
				}
			}
		default:
			skipped++
		}
		return nil
	}
	held, waiting := map[string][]ocEvent{}, 0
	const maxWaiting = 10000
	// release replays the held tool events of sessions that became owned
	release := func() error {
		for raw, l := range held {
			if s, ok := mc.sessions[raw]; ok && s.owned {
				delete(held, raw)
				waiting -= len(l)
				for _, e := range l { // the held tool and model events of the session, in arrival order
					if e.Hook == "model.request" || e.Hook == "event" {
						if err := emitAll(n.modelEvent(mc, e)); err != nil {
							return err
						}
						continue
					}
					if err := tool(e); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e ocEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			skipped++
			continue
		}
		if e.Hook == "hint.stats" {
			if n.OnStats != nil {
				n.OnStats(sc.Bytes())
			}
			continue
		}
		switch e.Hook {
		case "hint.attach", "model.request", "event", "hint.close":
			if !n.ModelCalls {
				skipped++
				continue
			}
			// a model event of a session not (yet) owned waits like its tool events (C7BE-18):
			// a later observed parent link replays it; it is never dropped silently for a
			// session that becomes owned (an overflow counts as a skipped line)
			if raw := heldSession(e); raw != "" && len(n.RootSessions) > 0 && !mc.closed {
				if s, ok := mc.sessions[raw]; !ok || !s.owned {
					if waiting < maxWaiting {
						held[raw] = append(held[raw], e)
						waiting++
					} else {
						skipped++
					}
					continue
				}
			}
			l := n.modelEvent(mc, e)
			if e.Hook == "hint.close" {
				l = n.closeSessions(mc, &e, skipped)
			}
			if err := emitAll(l); err != nil {
				return skipped, err
			}
			if err := release(); err != nil {
				return skipped, err
			}
			continue
		}
		// with a declared root, tool and context records are kept only for owned sessions; an
		// event of a session not (yet) known to be owned waits for its association, bounded,
		// and is dropped at the end if none is observed (C7BE-18): never adopted by time
		if len(n.RootSessions) > 0 && n.ModelCalls {
			raw := e.Input.SessionID
			if s, ok := mc.sessions[raw]; !ok || !s.owned {
				if waiting < maxWaiting {
					held[raw] = append(held[raw], e)
					waiting++
				} else {
					skipped++
				}
				continue
			}
		}
		if err := tool(e); err != nil {
			return skipped, err
		}
	}
	if n.ModelCalls { // end of input without a closure line: every owned session is interrupted
		if err := emitAll(n.closeSessions(mc, nil, skipped)); err != nil {
			return skipped, err
		}
	}
	for _, k := range order {
		if p, ok := pending[k]; ok {
			o := p.obs
			o.Missing = append(o.Missing, "time.end", "size")
			if isShellOp(o) {
				o.Missing = append(o.Missing, "outcome.exit_code")
			}
			if err := emit(n.finish(o)); err != nil {
				return skipped, fmt.Errorf("emit: %w", err)
			}
		}
	}
	return skipped, sc.Err()
}

func targetOp(op string) bool {
	switch op {
	case "READ", "EDIT_INTENT", "SEARCH", "DISCOVER":
		return true
	}
	return false
}

func isShellOp(o Observation) bool { return o.Source == "shell_parser" || o.CommandClass != "" }

func (n *Normalizer) finish(o Observation) Observation {
	now := time.Now()
	if n.Now != nil {
		now = n.Now()
	}
	o.Provenance = defaultProvenance(now)
	slices.Sort(o.Missing)
	o.Missing = slices.Compact(o.Missing)
	if o.Missing == nil {
		o.Missing = []string{}
	}
	return o
}

// contextStages adds the context measurement of a HINT context call: the observation itself
// is the delivered_context stage (the text parts that the hook received), and the returned
// observation is the raw_tool_output stage of the same call (the tool's answer as sent). Two
// stages of one call share the tool call ID; they are not two calls. A missing or invalid
// measurement leaves "context" missing.
func (n *Normalizer) contextStages(o *Observation, e ocEvent, delivered int) (Observation, bool) {
	if o.Op != "DISCOVER" || o.Source != "plugin" || !isContextToolCall(e) || e.Output.Status == "error" {
		return Observation{}, false
	}
	p, ok := parseContextPart(e.Output.Context)
	if !ok || e.Output.ContextInvalid || delivered < 0 {
		o.Missing = append(o.Missing, "context")
		return Observation{}, false
	}
	for _, t := range p.Targets {
		if tg := (Target{EntityID: t}); !slices.Contains(o.Targets, tg) {
			o.Targets = append(o.Targets, tg)
		}
	}
	if len(o.Targets) > 0 {
		o.Missing = slices.DeleteFunc(o.Missing, func(s string) bool { return s == "targets" })
	}
	raw := *o
	raw.ObservationID = hint.DeriveID(n.Key, "event", n.SourceID, e.Input.SessionID, e.Input.CallID, "raw_tool_output")
	raw.Size = nil
	raw.Missing = slices.DeleteFunc(slices.Clone(o.Missing), func(s string) bool { return s == "size" })
	rc := p.Context
	raw.Context = &rc
	dc := p.Context
	dc.Stage, dc.Bytes = "delivered_context", &delivered
	dc.Tokens = &ContextTokens{Value: delivered / 4, Estimator: "bytes-div-4", EstimatorVersion: "1", Label: "estimate"}
	o.Context = &dc
	return raw, true
}

func isContextToolCall(e ocEvent) bool { return isContextTool(e.Input.Tool) }

// heldSession is the session of a model event that may wait for its session's ownership: a
// model request or a bus event other than a session creation (which carries the parent link
// and is always handled at once); "" for anything else.
func heldSession(e ocEvent) string {
	switch {
	case e.Hook == "model.request":
		return e.Input.SessionID
	case e.Hook == "event" && e.Event != nil && e.Event.Type != "session.created":
		return e.Event.SessionID
	}
	return ""
}
