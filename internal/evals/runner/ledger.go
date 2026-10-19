package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageNormalization labels the provider usage that the ledger records: the OpenAI-compatible
// usage object as reported (prompt_tokens, completion_tokens, prompt_tokens_details.cached_tokens,
// completion_tokens_details.reasoning_tokens). Whether prompt tokens include cache reads or
// writes and whether completion tokens include reasoning depends on the provider; the label
// maps nothing and is never canonical model_tokens.
const UsageNormalization = "openai-compat-usage-v1"

// Terminal states of one admitted request.
const (
	EntryInFlight      = "in_flight"
	EntryComplete      = "complete"       // a 2xx response with exactly one usage object (a finished stream)
	EntryMissingUsage  = "missing_usage"  // a 2xx response without a readable usage object
	EntryUsageConflict = "usage_conflict" // a stream with two different usage objects
	EntryTruncated     = "truncated"      // a stream that ended without its [DONE] marker
	EntryError         = "error"          // a non-2xx response (usage kept when present)
	EntryCancelled     = "cancelled"      // the client or the upstream aborted the exchange
	EntryUnknown       = "unknown"        // still in flight when the drain ended
)

// Egress boundary and process scope of a run, stated by the launcher (the ledger cannot see
// them). Only an enforced run-wide egress boundary and an isolated scope allow a complete ledger.
const (
	BypassEnforced         = "enforced"           // the run's network reaches only the proxy
	BypassObservedNone     = "observed_no_bypass" // a passive watch saw no other endpoint: not proof
	BypassUnverified       = "unverified"
	ScopeIsolatedContainer = "isolated_container" // every process of the run is inside one container
	ScopeRestricted        = "restricted"         // a process group or less: descendants can escape
)

// usageComponents are the numeric usage fields that the ledger keeps; nothing else of a
// request or response is stored.
type usageComponents struct {
	Prompt, Completion, Cached, Reasoning *int64
}

func (u *usageComponents) equal(o *usageComponents) bool {
	eq := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return eq(u.Prompt, o.Prompt) && eq(u.Completion, o.Completion) && eq(u.Cached, o.Cached) && eq(u.Reasoning, o.Reasoning)
}

type ledgerEntry struct {
	model string // verified | mismatch | unverified
	state string
	usage *usageComponents
	done  chan struct{}
}

// ledger is the per-run model ledger of one trial counter.
type ledger struct {
	mu       sync.Mutex
	entries  []*ledgerEntry
	sealed   bool
	late     int
	rejected int
	report   *LedgerReport
}

// admit decides one request atomically against the seal and the limit: a request after the
// seal is late, one over the limit is rejected; otherwise it is admitted before it is forwarded.
func (c *Counter) admit() (*ledgerEntry, int) {
	l := &c.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		l.late++
		return nil, http.StatusForbidden
	}
	if c.requests.Load() >= c.limit {
		l.rejected++
		c.hit.Store(true)
		c.retries.Add(1)
		return nil, http.StatusTooManyRequests
	}
	c.requests.Add(1)
	e := &ledgerEntry{state: EntryInFlight, done: make(chan struct{})}
	l.entries = append(l.entries, e)
	return e, 0
}

func (c *Counter) setModel(e *ledgerEntry, label string) {
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	e.model = label
}

func (c *Counter) finish(e *ledgerEntry, state string, u *usageComponents) {
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	if e.state == EntryInFlight { // an entry already declared unknown by the seal stays unknown
		e.state, e.usage = state, u
		close(e.done)
	}
}

// LedgerReport is the sealed account of one run: counts per entry state, the summed provider
// usage components of every entry that reported usage (failures included), and whether that
// sum is the complete run total. Only allowlisted numbers; no prompt, content, header or key.
type LedgerReport struct {
	State         string           `json:"state"` // complete | incomplete
	Reasons       []string         `json:"reasons,omitempty"`
	Counts        map[string]int   `json:"counts"`
	Components    map[string]int64 `json:"components"`     // sums over entries with usage, any state
	ComponentsN   map[string]int   `json:"components_n"`   // entries that reported each component
	TotalComplete bool             `json:"total_complete"` // the sums are the run total
	Normalization string           `json:"normalization"`
	ModelCheck    string           `json:"model_check"` // verified: every admitted request named the selected model
	Bypass        string           `json:"bypass"`
	Scope         string           `json:"scope"`
}

// Seal revokes admission atomically, then waits for the already admitted requests until one
// shared deadline (now + drain), and freezes the report. Later requests are counted as late; a
// request still open at the deadline is unknown. A second call returns the frozen report.
func (c *Counter) Seal(drain time.Duration, bypass, scope string) *LedgerReport {
	l := &c.ledger
	l.mu.Lock()
	if l.report != nil {
		r := l.report
		l.mu.Unlock()
		return r
	}
	l.sealed = true
	pending := append([]*ledgerEntry(nil), l.entries...)
	l.mu.Unlock()
	deadline := time.NewTimer(drain)
	defer deadline.Stop()
wait:
	for _, e := range pending {
		select {
		case <-e.done:
		case <-deadline.C:
			break wait
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := &LedgerReport{Counts: map[string]int{}, Components: map[string]int64{}, ComponentsN: map[string]int{},
		Normalization: UsageNormalization, Bypass: bypass, Scope: scope}
	model := map[string]int{}
	for _, e := range l.entries {
		if e.state == EntryInFlight {
			e.state = EntryUnknown
			close(e.done)
		}
		r.Counts[e.state]++
		model[e.model]++
		if u := e.usage; u != nil {
			for k, v := range map[string]*int64{"prompt": u.Prompt, "completion": u.Completion, "cached": u.Cached, "reasoning": u.Reasoning} {
				if v != nil {
					r.Components[k] += *v
					r.ComponentsN[k]++
				}
			}
		}
	}
	if l.late > 0 {
		r.Counts["late"] = l.late
	}
	if l.rejected > 0 {
		r.Counts["rejected_limit"] = l.rejected
	}
	switch {
	case model["mismatch"] > 0:
		r.ModelCheck = "mismatch"
	case model["unverified"] > 0:
		r.ModelCheck = "unverified"
	case model["verified"] > 0:
		r.ModelCheck = "verified"
	default:
		r.ModelCheck = "not_observed"
	}
	reason := func(cond bool, s string) {
		if cond {
			r.Reasons = append(r.Reasons, s)
		}
	}
	reason(bypass != BypassEnforced, "bypass_not_enforced")
	reason(scope != ScopeIsolatedContainer, "scope_not_isolated")
	reason(l.late > 0, "late_requests")
	reason(len(l.entries) == 0, "no_requests")
	for _, s := range []string{EntryMissingUsage, EntryUsageConflict, EntryTruncated, EntryError, EntryCancelled, EntryUnknown} {
		reason(r.Counts[s] > 0, s)
	}
	reason(r.ModelCheck != "verified", "model_"+r.ModelCheck)
	reason(l.rejected > 0, "rejected_limit") // the forwarded usage can be complete, but the run was cut
	sort.Strings(r.Reasons)
	r.State = "incomplete"
	if len(r.Reasons) == 0 {
		r.State, r.TotalComplete = "complete", true
	}
	l.report = r
	return r
}

// Bounds of the usage reader. A non-streaming body is scanned without being stored: only the
// bytes of its top-level usage value are kept, up to maxUsageValue (a larger value counts as
// missing); a stream line longer than maxSSELine is skipped. Bytes are forwarded unchanged and
// at once; the reader never delays a write.
const (
	maxUsageValue = 64 << 10
	maxSSELine    = 1 << 20
)

// usageRecorder forwards the response unchanged and reads only its usage: from a JSON body or
// from the data lines of a server-sent event stream. Nothing but the numeric components is kept.
type usageRecorder struct {
	http.ResponseWriter
	status   int
	started  bool
	sse      bool
	scan     usageScan // non-streaming body
	line     []byte    // the current stream line, bounded
	skip     bool      // the current stream line is over the bound
	usage    *usageComponents
	conflict bool
	done     bool // the stream sent its [DONE] marker
}

func (w *usageRecorder) WriteHeader(code int) {
	if !w.started {
		w.started, w.status = true, code
		w.sse = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *usageRecorder) Write(b []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b) // forward first, read after
	if w.sse {
		w.feed(b)
	} else {
		w.scan.feed(b)
	}
	return n, err
}

func (w *usageRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *usageRecorder) feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !w.skip {
			if len(w.line)+len(part) > maxSSELine {
				w.skip, w.line = true, nil
			} else {
				w.line = append(w.line, part...)
			}
		}
		if i < 0 {
			return
		}
		if !w.skip {
			w.sseLine(w.line)
		}
		w.line, w.skip = w.line[:0], false
		b = b[i+1:]
	}
}

func (w *usageRecorder) sseLine(l []byte) {
	d, ok := bytes.CutPrefix(bytes.TrimRight(l, "\r"), []byte("data:"))
	if !ok || w.done {
		return // data after [DONE] is not part of the stream
	}
	d = bytes.TrimSpace(d)
	if string(d) == "[DONE]" {
		w.done = true
		return
	}
	u := parseUsage(d)
	switch {
	case u == nil:
	case w.usage == nil:
		w.usage = u
	case !w.usage.equal(u):
		w.conflict = true
	}
}

// result is the entry state and usage of the finished response (the status is checked by the
// caller first).
func (w *usageRecorder) result() (string, *usageComponents) {
	if !w.sse {
		return w.scan.result()
	}
	if len(w.line) > 0 && !w.skip { // a last line without its newline
		w.sseLine(w.line)
	}
	switch {
	case w.conflict:
		return EntryUsageConflict, nil
	case !w.done:
		return EntryTruncated, w.usage
	case w.usage == nil:
		return EntryMissingUsage, nil
	}
	return EntryComplete, w.usage
}

// parseUsage reads the top-level usage object of one JSON document; non-negative integer
// components only, anything else is absent.
func parseUsage(b []byte) *usageComponents {
	var doc struct {
		Usage *struct {
			Prompt     json.RawMessage `json:"prompt_tokens"`
			Completion json.RawMessage `json:"completion_tokens"`
			PromptD    *struct {
				Cached json.RawMessage `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionD *struct {
				Reasoning json.RawMessage `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Usage == nil {
		return nil
	}
	num := func(m json.RawMessage) *int64 {
		var v int64
		if len(m) == 0 || json.Unmarshal(m, &v) != nil || v < 0 {
			return nil
		}
		return &v
	}
	u := &usageComponents{Prompt: num(doc.Usage.Prompt), Completion: num(doc.Usage.Completion)}
	if doc.Usage.PromptD != nil {
		u.Cached = num(doc.Usage.PromptD.Cached)
	}
	if doc.Usage.CompletionD != nil {
		u.Reasoning = num(doc.Usage.CompletionD.Reasoning)
	}
	if u.Prompt == nil && u.Completion == nil {
		return nil
	}
	return u
}

// usageScan reads the top-level "usage" value of a JSON object byte by byte. It keeps only the
// bytes of that value (bounded); every other value, the content included, is scanned and dropped.
type usageScan struct {
	depth                int
	inStr, esc           bool
	expectKey, keyActive bool
	key                  []byte
	lastKey              string
	capturing            bool
	value                []byte
	values               [][]byte
	over, bad            bool
}

func (s *usageScan) feed(b []byte) {
	for _, c := range b {
		if s.bad {
			return
		}
		if s.capturing {
			if len(s.value) >= maxUsageValue {
				s.over = true
			} else {
				s.value = append(s.value, c)
			}
		}
		if s.inStr {
			switch {
			case s.esc:
				s.esc = false
			case c == '\\':
				s.esc = true
			case c == '"':
				s.inStr = false
				if s.keyActive {
					s.keyActive, s.lastKey = false, string(s.key)
					continue
				}
			}
			if s.keyActive && len(s.key) < 64 {
				s.key = append(s.key, c)
			}
			continue
		}
		switch c {
		case '"':
			s.inStr = true
			if s.depth == 1 && s.expectKey {
				s.expectKey, s.keyActive, s.key = false, true, s.key[:0]
			}
		case '{', '[':
			s.depth++
			if s.depth == 1 {
				s.expectKey = c == '{'
				if c != '{' {
					s.bad = true
				}
			}
		case '}', ']':
			s.depth--
			if s.depth == 0 {
				s.end()
			}
		case ',':
			if s.depth == 1 {
				s.end()
				s.expectKey = true
			}
		case ':':
			if s.depth == 1 && s.lastKey == "usage" {
				s.capturing, s.value = true, s.value[:0]
			}
			s.lastKey = ""
		}
	}
}

// end closes a captured value at a top-level ',' or at the closing '}' (the delimiter, the
// last captured byte, is not part of the value).
func (s *usageScan) end() {
	if !s.capturing {
		return
	}
	s.capturing = false
	if !s.over && len(s.value) > 0 {
		s.values = append(s.values, bytes.Clone(bytes.TrimSpace(s.value[:len(s.value)-1])))
	}
	s.over = false
}

func (s *usageScan) result() (string, *usageComponents) {
	if s.bad || s.depth != 0 || len(s.values) == 0 {
		return EntryMissingUsage, nil
	}
	var u *usageComponents
	for _, v := range s.values {
		x := parseUsage(append(append([]byte(`{"usage":`), v...), '}'))
		switch {
		case x == nil:
			return EntryMissingUsage, nil
		case u == nil:
			u = x
		case !u.equal(x):
			return EntryUsageConflict, nil
		}
	}
	return EntryComplete, u
}
