package activity

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
)

// UsageNormalization is the normalization of the OpenCode 2.0.26 TokenUsage.Info counters
// (07B-E §30/§31): input = non-cached input, output = generated total (visible output plus
// the disjoint reasoning), reasoning a subset of output, cache read/write separate. OpenCode
// turns a missing, negative or non-finite provider counter into 0, so a 0 is never known.
const UsageNormalization = "opencode-2.0.26:tokeninfo-v1"

// UsageAgentVersion is the only OpenCode version whose events this mapping was checked on.
const UsageAgentVersion = "2.0.26"

// Request kinds of a model call (hint-data v0.2 request_kind).
var requestKinds = []string{"primary", "title", "compaction", "generate"}

// ocModelEvent is one reduced OpenCode bus event (only the fields the plugin keeps; never
// text, prompts, headers or raw model names).
type ocModelEvent struct {
	ID                 string    `json:"id"`
	Type               string    `json:"type"`
	SessionID          string    `json:"sessionID"`
	ParentID           string    `json:"parentID"`
	AssistantMessageID string    `json:"assistantMessageID"`
	Source             string    `json:"source"` // session.usage.recorded: title or compaction
	Tokens             *ocTokens `json:"tokens"`
}

// ocTokens is OpenCode's TokenUsage.Info. Values are float64 so that a non-integer or
// negative value can be refused rather than truncated.
type ocTokens struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	Reasoning *float64 `json:"reasoning"`
	Cache     *struct {
		Read  *float64 `json:"read"`
		Write *float64 `json:"write"`
	} `json:"cache"`
}

// ocClose is the plugin's closure line: drained = the plugin stopped and drained its event
// subscription and its in-flight hooks returned before it wrote this line; dropped and errors
// are its stream-wide counters. The acknowledgment is the child's own (Normalizer.Acknowledged).
type ocClose struct {
	Drained bool     `json:"drained"`
	Dropped *float64 `json:"dropped"`
	Errors  *float64 `json:"errors"`
}

// callSession is the collector state of one OpenCode session.
type callSession struct {
	raw       string
	owned     bool
	lifecycle string
	start     string
	primary   int             // model.request hooks of kind primary
	steps     int             // session.step.started events
	open      map[string]bool // started steps without a terminal event
	retried   map[string]bool // steps with a scheduled retry (more than one physical request)
	running   int             // started compactions without a terminal event
	compUsage *ocTokens       // session.usage.recorded (compaction) of the running compaction
	compSeen  bool
	errors    int
	ordinal   map[string]int // model.request hooks per kind (record identity)
	closed    bool
}

// calls tracks the model calls and the collector closure of the sessions of one run. A
// session is owned when its session.created event arrives after the subscription started
// (a root session, or a child of an owned session); a session that this process calls a
// model for without such an event is owned as attached mid-session. Events of other
// sessions are never recorded (no timing-based ownership).
type calls struct {
	attached bool
	sessions map[string]*callSession
	order    []string
	events   map[string]string // scoped source event -> canonical body (dedup)
	closed   bool
}

func newCalls() *calls {
	return &calls{sessions: map[string]*callSession{}, events: map[string]string{}}
}

func (c *calls) session(raw string) *callSession {
	s, ok := c.sessions[raw]
	if !ok {
		s = &callSession{raw: raw, lifecycle: "unknown", open: map[string]bool{}, retried: map[string]bool{}, ordinal: map[string]int{}}
		c.sessions[raw] = s
		c.order = append(c.order, raw)
	}
	return s
}

// modelObs is the common form of a model-call record of an owned session.
func (n *Normalizer) modelObs(op, raw, at string, id string) Observation {
	o := Observation{ObservationID: id, Class: "observation", Op: op, Source: "plugin", SourceID: n.SourceID, Precision: "exact",
		Time: Time{Start: &at, End: &at, Clock: "agent"}, Outcome: Outcome{Status: "success"},
		Correlation: map[string]string{"session_id": hint.DeriveID(n.Key, "session", n.SourceID, raw)},
		Missing:     []string{"correlation.task_id", "correlation.turn_id", "targets"}}
	if n.TaskID != "" {
		o.Correlation["task_id"] = n.TaskID
		if n.TaskFamilyID != "" {
			o.Correlation["task_family_id"] = n.TaskFamilyID
		}
		o.Missing = slices.DeleteFunc(o.Missing, func(s string) bool { return s == "correlation.task_id" })
	}
	return o
}

// callRef is the exact identity of a primary call (OpenCode's assistant message of the step).
func (n *Normalizer) callRef(raw, msg string) string {
	return hint.DeriveID(n.Key, "model-call", n.SourceID, raw, msg)
}

// usageOf maps TokenUsage.Info to the activity usage (07B-E §30 Table A): a counter is known
// only when it is a finite positive integer; output is the generated total output+reasoning,
// known only when output > 0; an all-zero call has unknown usage.
func usageOf(t *ocTokens, unknownReason string) Usage {
	unknown := func(r string) scan.Quantity { return scan.Quantity{Unit: "tokens", State: "unknown", Reason: r} }
	u := func(r string) Usage {
		x, y, z, a, b := unknown(r), unknown(r), unknown(r), unknown(r), unknown(r)
		return Usage{InputTokens: x, OutputTokens: y, ReasoningTokens: &z, CacheReadTokens: &a, CacheWriteTokens: &b}
	}
	if t == nil {
		return u(unknownReason)
	}
	vals := []*float64{t.Input, t.Output, t.Reasoning}
	if t.Cache != nil {
		vals = append(vals, t.Cache.Read, t.Cache.Write)
	} else {
		vals = append(vals, nil, nil)
	}
	for _, v := range vals {
		if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v != math.Trunc(*v) || *v > 1e12 {
			return u("source_error")
		}
	}
	in, out, rs, cr, cw := *vals[0], *vals[1], *vals[2], *vals[3], *vals[4]
	if in+out+rs+cr+cw == 0 {
		return u("not_supported") // a missing provider usage and a real zero are the same here
	}
	q := func(v float64) scan.Quantity {
		if v > 0 {
			return scan.Known("tokens", v, "harness_usage")
		}
		return unknown("not_supported")
	}
	res := Usage{InputTokens: q(in), OutputTokens: unknown("not_supported")}
	if out > 0 {
		res.OutputTokens = scan.Known("tokens", out+rs, "harness_usage")
	}
	r, a, b := q(rs), q(cr), q(cw)
	res.ReasoningTokens, res.CacheReadTokens, res.CacheWriteTokens = &r, &a, &b
	return res
}

func sameTokens(a, b *ocTokens) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return a != nil && b != nil && string(x) == string(y)
}

// modelEvent handles one model hook or bus event; it returns the records to emit.
func (n *Normalizer) modelEvent(c *calls, e ocEvent) []Observation {
	at := Timestamp(time.UnixMilli(e.TWall))
	switch e.Hook {
	case "hint.attach":
		c.attached = true
		return nil
	case "model.request":
		raw, kind := e.Input.SessionID, e.Input.Kind
		if raw == "" || !slices.Contains(requestKinds, kind) {
			return nil
		}
		s := c.session(raw)
		if !s.owned { // this process calls a model for a session whose creation was not observed
			s.owned, s.lifecycle, s.start = true, "attached_mid_session", at
		}
		if kind == "primary" { // the primary record comes from its step (exact call identity)
			s.primary++
			return nil
		}
		s.ordinal[kind]++
		o := n.modelObs("MODEL_CALL", raw, at, hint.DeriveID(n.Key, "model-request", n.SourceID, raw, kind, itoa(s.ordinal[kind])))
		o.RequestKind = kind
		return []Observation{n.finish(o)}
	case "event":
	default:
		return nil
	}
	ev := e.Event
	if ev.SessionID == "" || ev.ID == "" {
		return nil
	}
	if ev.Type == "session.created" {
		s := c.session(ev.SessionID)
		if c.attached && !s.owned && (ev.ParentID == "" || c.sessions[ev.ParentID] != nil && c.sessions[ev.ParentID].owned) {
			s.owned, s.lifecycle, s.start = true, "attached_at_start", at
		}
		return nil
	}
	s, ok := c.sessions[ev.SessionID]
	if !ok || !s.owned {
		return nil // an unrelated session of a shared OpenCode server
	}
	// dedup: an identical replay collapses; the same scoped event with another body is a conflict
	scoped := ev.SessionID + "\x00" + ev.ID
	body, _ := json.Marshal(ev)
	if prev, ok := c.events[scoped]; ok {
		if prev != string(body) {
			s.errors++
		}
		return nil
	}
	c.events[scoped] = string(body)
	id := func(role string) string {
		return hint.DeriveID(n.Key, "model-event", n.SourceID, ev.SessionID, ev.ID, role)
	}
	sourceEvent := hint.DeriveID(n.Key, "source-event", n.SourceID, ev.SessionID, ev.ID)
	out := func(o Observation) []Observation {
		o = n.finish(o)
		o.Provenance.SourceEventID = sourceEvent
		return []Observation{o}
	}
	usage := func(kind string, u Usage, role string) []Observation {
		o := n.modelObs("USAGE", ev.SessionID, at, id(role))
		o.RequestKind, o.Usage = kind, &u
		if kind == "primary" && ev.AssistantMessageID != "" {
			o.Correlation["call_ref"] = n.callRef(ev.SessionID, ev.AssistantMessageID)
		}
		return out(o)
	}
	switch ev.Type {
	case "session.step.started":
		if ev.AssistantMessageID == "" || s.open[ev.AssistantMessageID] {
			s.errors++
			return nil
		}
		s.steps++
		s.open[ev.AssistantMessageID] = true
		o := n.modelObs("MODEL_CALL", ev.SessionID, at, id("start"))
		o.RequestKind = "primary"
		o.Correlation["call_ref"] = n.callRef(ev.SessionID, ev.AssistantMessageID)
		return out(o)
	case "session.retry.scheduled":
		s.retried[ev.AssistantMessageID] = true
		return nil
	case "session.step.ended", "session.step.failed":
		if !s.open[ev.AssistantMessageID] {
			s.errors++ // a terminal without its start (the server also refuses the orphan)
		}
		delete(s.open, ev.AssistantMessageID)
		u := usageOf(ev.Tokens, "source_error")
		if s.retried[ev.AssistantMessageID] { // per-attempt usage of the retried requests is unknown
			u = usageOf(nil, "not_supported")
		}
		return usage("primary", u, "terminal")
	case "session.usage.recorded":
		switch ev.Source {
		case "title":
			return usage("title", usageOf(ev.Tokens, "source_error"), "terminal")
		case "compaction": // an alternative observation of the running compaction call
			if s.running != 1 || s.compSeen {
				s.errors++
				return nil
			}
			s.compUsage, s.compSeen = ev.Tokens, true
		}
		return nil
	case "session.compaction.started":
		if s.running > 0 {
			s.errors++ // overlapping compactions: no unambiguous terminal
		}
		s.running++
		s.compUsage, s.compSeen = nil, false
		return nil
	case "session.compaction.ended", "session.compaction.failed":
		if s.running == 0 {
			s.errors++
		} else {
			s.running--
		}
		u := usageOf(ev.Tokens, "source_error")
		if s.compSeen && !sameTokens(ev.Tokens, s.compUsage) { // two observations of one call disagree
			u = usageOf(nil, "source_error")
			s.errors++
		}
		s.compUsage, s.compSeen = nil, false
		return usage("compaction", u, "terminal")
	}
	return nil
}

// closeSessions emits the closing SESSION record of every owned session: flushed only when
// the plugin drained its subscription and the child acknowledged every preceding line;
// otherwise interrupted (end of input without a closure line) or unknown.
func (n *Normalizer) closeSessions(c *calls, e *ocEvent, skipped int) []Observation {
	if c.closed {
		return nil
	}
	c.closed = true
	closure, closedAt := "interrupted", (*string)(nil)
	at := Timestamp(time.Now())
	if n.Now != nil {
		at = Timestamp(n.Now())
	}
	var cl ocClose
	if e != nil {
		at = Timestamp(time.UnixMilli(e.TWall))
		if e.Close != nil {
			cl = *e.Close
		}
		closure = "unknown"
		// the child read this line after every preceding line, in order; Acknowledged reports
		// that each of them was handled and written (pipe acceptance by itself proves nothing)
		if cl.Drained && n.Acknowledged != nil && n.Acknowledged() {
			closure, closedAt = "flushed", &at
		}
	}
	count := func(v *float64) scan.Quantity {
		if v == nil || *v < 0 || *v != math.Trunc(*v) {
			return scan.Quantity{Unit: "count", State: "unknown", Reason: "not_collected"}
		}
		return scan.Known("count", *v, "count_of_records")
	}
	var res []Observation
	for _, raw := range c.order {
		s := c.sessions[raw]
		if !s.owned || s.closed {
			continue
		}
		s.closed = true
		errs := s.errors
		if s.primary != s.steps { // a lost model.request or a lost step: the two channels disagree
			errs++
		}
		if cl.Errors != nil && *cl.Errors > 0 { // shared stream errors affect every session
			errs += int(*cl.Errors)
		}
		errs += skipped // unreadable lines of the shared stream affect every session
		start := s.start
		o := n.modelObs("SESSION", raw, start, hint.DeriveID(n.Key, "session-closure", n.SourceID, raw))
		o.Time.End = &at
		o.Missing = slices.DeleteFunc(o.Missing, func(m string) bool { return m == "targets" })
		ec := float64(errs)
		oc := float64(len(s.open) + s.running)
		o.Collection = &Collection{Lifecycle: s.lifecycle, Closure: closure, EventsDropped: count(cl.Dropped),
			Errors: count(&ec), OpenCalls: count(&oc), ClosedAt: closedAt}
		if e == nil {
			o.Collection.EventsDropped = scan.Quantity{Unit: "count", State: "unknown", Reason: "not_collected"}
		}
		res = append(res, n.finish(o))
	}
	return res
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
