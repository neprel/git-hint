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
	EntryComplete      = "complete"       // a 2xx response with one usage object whose prompt and completion counts are known
	EntryUsagePartial  = "usage_partial"  // a usage object without a known prompt or completion count
	EntryMissingUsage  = "missing_usage"  // a 2xx response without a usage object
	EntryUsageConflict = "usage_conflict" // two different usage objects in one response
	EntryUnreadable    = "unreadable"     // not one valid JSON document, an undecodable or skipped stream line, an unusable usage value
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
// request or response is stored. A nil field is unknown (missing, null or invalid).
type usageComponents struct {
	Prompt, Completion, Cached, Reasoning *int64
}

func (u *usageComponents) equal(o *usageComponents) bool {
	eq := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	return eq(u.Prompt, o.Prompt) && eq(u.Completion, o.Completion) && eq(u.Cached, o.Cached) && eq(u.Reasoning, o.Reasoning)
}

// known says whether both main counts are known; the optional cache and reasoning counts stay
// separate and never make a usage complete or incomplete.
func (u *usageComponents) known() bool { return u != nil && u.Prompt != nil && u.Completion != nil }

type ledgerEntry struct {
	model string // verified | mismatch | unverified
	state string
	usage *usageComponents
	done  chan struct{}
}

// ledger is the per-run model ledger of one trial counter.
type ledger struct {
	mu        sync.Mutex
	entries   []*ledgerEntry
	sealed    bool
	finalized chan struct{} // closed when the first Seal has frozen the entries
	bypass    string
	scope     string
	late      int // requests refused because they came after the seal
	rejected  int // requests refused at the limit
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
	if e.state == EntryInFlight { // an entry declared unknown by the seal stays unknown
		e.state, e.usage = state, u
		close(e.done)
	}
}

// LedgerReport is an account of one sealed run: counts per entry state, the summed provider
// usage components of every entry that reported them (failures included), and whether those
// sums are the complete run total. Revision counts the late requests seen after the seal: a
// report with a higher revision supersedes one with a lower revision, so late traffic never
// disappears from the account. Only allowlisted numbers; no prompt, content, header or key.
// Every call returns a new value.
type LedgerReport struct {
	State         string           `json:"state"` // complete | incomplete
	Reasons       []string         `json:"reasons,omitempty"`
	Revision      int              `json:"revision"`
	Counts        map[string]int   `json:"counts"`
	Components    map[string]int64 `json:"components"`     // sums over entries with the component, any state
	ComponentsN   map[string]int   `json:"components_n"`   // entries that reported each component
	TotalComplete bool             `json:"total_complete"` // the sums are the run total
	Normalization string           `json:"normalization"`
	ModelCheck    string           `json:"model_check"` // verified: every admitted request named the selected model
	Bypass        string           `json:"bypass"`
	Scope         string           `json:"scope"`
}

// Seal revokes admission atomically, waits for the already admitted requests until one shared
// deadline (now + drain), freezes them (an open one is unknown) and returns the account. Only
// the first call seals, drains and fixes bypass and scope; a concurrent call waits for it. Every
// call returns the current account (Report).
func (c *Counter) Seal(drain time.Duration, bypass, scope string) *LedgerReport {
	l := &c.ledger
	l.mu.Lock()
	first := l.finalized == nil
	if first {
		l.sealed, l.finalized, l.bypass, l.scope = true, make(chan struct{}), bypass, scope
	}
	pending := append([]*ledgerEntry(nil), l.entries...)
	done := l.finalized
	l.mu.Unlock()
	if !first {
		<-done
		return c.Report()
	}
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
	for _, e := range l.entries {
		if e.state == EntryInFlight {
			e.state = EntryUnknown
			close(e.done)
		}
	}
	close(l.finalized)
	l.mu.Unlock()
	return c.Report()
}

// Report is the current account of a sealed run, or nil before the first seal is final. A
// request that arrives after the seal is refused and counted as late; the account then says so
// (incomplete, higher revision).
func (c *Counter) Report() *LedgerReport {
	l := &c.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finalized == nil {
		return nil
	}
	select {
	case <-l.finalized:
	default:
		return nil
	}
	r := &LedgerReport{Counts: map[string]int{}, Components: map[string]int64{}, ComponentsN: map[string]int{},
		Normalization: UsageNormalization, Bypass: l.bypass, Scope: l.scope, Revision: l.late}
	model := map[string]int{}
	for _, e := range l.entries {
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
	for s := range r.Counts {
		if s != EntryComplete {
			r.Reasons = append(r.Reasons, s)
		}
	}
	if l.late > 0 {
		r.Counts["late"] = l.late
		r.Reasons = append(r.Reasons, "late_requests")
	}
	if l.rejected > 0 {
		r.Counts["rejected_limit"] = l.rejected
		r.Reasons = append(r.Reasons, "rejected_limit") // the forwarded usage can be complete, but the run was cut
	}
	if l.bypass != BypassEnforced {
		r.Reasons = append(r.Reasons, "bypass_not_enforced")
	}
	if l.scope != ScopeIsolatedContainer {
		r.Reasons = append(r.Reasons, "scope_not_isolated")
	}
	if len(l.entries) == 0 {
		r.Reasons = append(r.Reasons, "no_requests")
	}
	if r.ModelCheck != "verified" {
		r.Reasons = append(r.Reasons, "model_"+r.ModelCheck)
	}
	sort.Strings(r.Reasons)
	r.State = "incomplete"
	if len(r.Reasons) == 0 {
		r.State, r.TotalComplete = "complete", true
	}
	return r
}

// Bounds of the usage reader: the bytes of one usage value, one stream line and the nesting
// depth. Bytes are forwarded unchanged and before they are read; the reader stores no content.
const (
	maxUsageValue = 64 << 10
	maxSSELine    = 1 << 20
	maxJSONDepth  = 512
)

// usageRecorder forwards the response unchanged and reads only its usage: from a JSON body (one
// valid document, read by a streaming validator) or from the data lines of a server-sent event
// stream. Nothing but the parsed numeric components and a conflict flag is kept.
type usageRecorder struct {
	http.ResponseWriter
	status  int
	started bool
	sse     bool
	doc     jsonUsage // non-streaming body
	line    []byte    // the current stream line, bounded
	skip    bool      // the current stream line is over the bound
	stream  usageSet  // usage of the stream's data lines
	bad     bool      // a skipped, undecodable or unusable data line before [DONE]
	done    bool      // the stream sent its [DONE] marker
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
		w.doc.feed(b)
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
				w.skip, w.line = true, w.line[:0]
			} else {
				w.line = append(w.line, part...)
			}
		}
		if i < 0 {
			return
		}
		w.endLine()
		b = b[i+1:]
	}
}

func (w *usageRecorder) endLine() {
	if w.skip {
		if !w.done {
			w.bad = true // a line that could have held the usage was not read
		}
	} else {
		w.sseLine(w.line)
	}
	w.line, w.skip = w.line[:0], false
}

func (w *usageRecorder) sseLine(l []byte) {
	d, ok := bytes.CutPrefix(bytes.TrimRight(l, "\r"), []byte("data:"))
	if !ok || w.done {
		return // a comment, event or id line, or data after [DONE] (not part of the stream)
	}
	d = bytes.TrimSpace(d)
	if string(d) == "[DONE]" {
		w.done = true
		return
	}
	var j jsonUsage
	j.feed(d)
	switch st, u := j.result(); st {
	case EntryUnreadable:
		w.bad = true
	case EntryUsageConflict:
		w.stream.conflict = true
	case EntryMissingUsage:
	default:
		w.stream.add(u)
	}
}

// result is the entry state and usage of the finished response (the caller checks the status
// and the cancellation first).
func (w *usageRecorder) result() (string, *usageComponents) {
	if !w.sse {
		return w.doc.result()
	}
	if len(w.line) > 0 || w.skip { // a last line without its newline
		w.endLine()
	}
	u := w.stream.usage
	switch {
	case w.bad:
		return EntryUnreadable, u
	case w.stream.conflict:
		return EntryUsageConflict, nil
	case !w.done:
		return EntryTruncated, u
	case u == nil:
		return EntryMissingUsage, nil
	case !u.known():
		return EntryUsagePartial, u
	}
	return EntryComplete, u
}

// usageSet keeps the first parsed usage and whether a different one followed (bounded state).
type usageSet struct {
	usage    *usageComponents
	conflict bool
}

func (s *usageSet) add(u *usageComponents) {
	switch {
	case u == nil:
	case s.usage == nil:
		s.usage = u
	case !s.usage.equal(u):
		s.conflict = true
	}
}

// jsonUsage validates one JSON document byte by byte (RFC 8259 grammar, one root value, only
// whitespace after it) and extracts the top-level "usage" members of a root object. It keeps the
// bytes of the current usage value only (at most maxUsageValue), parses them when the value ends,
// and then keeps only the parsed components; every other value is validated and dropped.
type jsonUsage struct {
	sc       scanner
	key      []byte // the current top-level key (bounded)
	keyOver  bool
	capture  bool // inside the value of a top-level "usage" member
	value    []byte
	valOver  bool
	set      usageSet
	unusable bool // a usage value over the bound, not an object, or an escaped top-level key
}

func (j *jsonUsage) feed(b []byte) {
	for _, c := range b {
		if j.sc.err {
			return
		}
		ev := j.sc.step(c)
		switch {
		case ev == evTopKeyStart:
			j.key, j.keyOver = j.key[:0], false
		case ev == evTopKeyByte:
			if c == '\\' {
				j.unusable = true // an escaped key could spell "usage"; never guess
			}
			if len(j.key) < 16 {
				j.key = append(j.key, c)
			} else {
				j.keyOver = true
			}
		case ev == evTopValueStart:
			if !j.keyOver && string(j.key) == "usage" {
				j.capture, j.value, j.valOver = true, append(j.value[:0], c), false
			}
		case j.capture:
			if len(j.value) >= maxUsageValue {
				j.valOver = true
			} else {
				j.value = append(j.value, c)
			}
			if ev == evTopValueEnd {
				j.capture = false
				j.endUsage()
			}
		}
	}
}

func (j *jsonUsage) endUsage() {
	if j.valOver {
		j.unusable = true
		return
	}
	v := bytes.TrimSpace(j.value)
	if string(v) == "null" {
		return // no usage in this member
	}
	u, ok := parseUsageValue(v)
	if !ok {
		j.unusable = true
		return
	}
	j.set.add(u)
}

func (j *jsonUsage) result() (string, *usageComponents) {
	if !j.sc.finish() {
		return EntryUnreadable, nil
	}
	u := j.set.usage
	switch {
	case j.unusable:
		return EntryUnreadable, u
	case j.set.conflict:
		return EntryUsageConflict, nil
	case u == nil:
		return EntryMissingUsage, nil
	case !u.known():
		return EntryUsagePartial, u
	}
	return EntryComplete, u
}

// parseUsageValue reads one usage object; a component is known only as a non-negative integer
// (missing, null, fractional, negative or non-numeric values stay unknown). Recognized keys must
// match exactly and occur once: a duplicate or a case alias of a recognized key (which a
// decoder could resolve either way) makes the whole usage unusable. Other keys are ignored.
func parseUsageValue(b []byte) (*usageComponents, bool) {
	top, ok := members(b, "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details")
	if !ok {
		return nil, false
	}
	num := func(m json.RawMessage) *int64 {
		var v int64
		if len(m) == 0 || string(m) == "null" || json.Unmarshal(m, &v) != nil || v < 0 {
			return nil
		}
		return &v
	}
	sub := func(m json.RawMessage, key string) (*int64, bool) {
		if len(m) == 0 || string(m) == "null" {
			return nil, true
		}
		if m[0] != '{' {
			return nil, true // not an object: the detail is unknown
		}
		d, ok := members(m, key)
		return num(d[key]), ok
	}
	cached, ok1 := sub(top["prompt_tokens_details"], "cached_tokens")
	reasoning, ok2 := sub(top["completion_tokens_details"], "reasoning_tokens")
	if !ok1 || !ok2 {
		return nil, false
	}
	return &usageComponents{Prompt: num(top["prompt_tokens"]), Completion: num(top["completion_tokens"]), Cached: cached, Reasoning: reasoning}, true
}

// members walks the members of one JSON object with a decoder (the value is bounded by the
// caller) and returns the raw values of the allowlisted keys. It fails when the input is not one
// object, or when an allowlisted key occurs twice or another key equals one ignoring case.
func members(b []byte, allow ...string) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		for _, a := range allow {
			switch {
			case key == a:
				if _, dup := out[a]; dup {
					return nil, false
				}
				out[a] = v
			case strings.EqualFold(key, a):
				return nil, false // a case alias of a recognized key
			}
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	return out, true
}

// Events of the scanner that the usage extractor needs.
const (
	evNone          = iota
	evTopKeyStart   // the opening quote of a key of the root object
	evTopKeyByte    // a byte inside such a key (escapes unprocessed)
	evTopValueStart // the first byte of a member value of the root object
	evTopValueEnd   // the byte that ends such a value (a number ends at the following delimiter)
)

// Scanner states.
const (
	sValue      = iota // expecting a value
	sValueOrEnd        // after '[': a value or ']'
	sKeyOrEnd          // after '{': a key or '}'
	sKey               // after ',' in an object: a key
	sColon             // after a key
	sAfterValue        // after a value: ',' or the container end
	sString
	sNumber
	sLiteral
)

// scanner is a byte-at-a-time JSON grammar checker (RFC 8259, one root value). It keeps a
// container stack (bounded depth) and a few state fields; it stores no input.
type scanner struct {
	stack     []byte // '{' or '[' per open container
	state     int
	afterRoot bool // the root value is complete
	err       bool
	lit       string // the rest of a literal (true, false, null)
	hex       int    // \u digits still expected
	esc       bool
	isKey     bool // the current string is an object key
	num       int  // number grammar state, see number
	topValue  bool // inside a member value of the root object
}

func ws(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (s *scanner) fail() int { s.err = true; return evNone }

// numberDone says whether the number read so far is complete.
func (s *scanner) numberDone() bool { return s.num == 2 || s.num == 3 || s.num == 5 || s.num == 8 }

// step consumes one byte and returns an event for the usage extractor.
func (s *scanner) step(c byte) int {
	if s.err {
		return evNone
	}
	if s.afterRoot {
		if !ws(c) {
			return s.fail() // trailing data or a second root value
		}
		return evNone
	}
	switch s.state {
	case sString:
		ev := evNone
		if s.isKey && len(s.stack) == 1 {
			ev = evTopKeyByte
		}
		switch {
		case s.hex > 0:
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
				return s.fail()
			}
			s.hex--
		case s.esc:
			s.esc = false
			switch c {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				s.hex = 4
			default:
				return s.fail()
			}
		case c == '\\':
			s.esc = true
		case c == '"':
			if s.isKey {
				s.isKey, s.state = false, sColon
				return evNone
			}
			return s.valueDone()
		case c < 0x20:
			return s.fail()
		}
		return ev
	case sNumber:
		if s.number(c) {
			return evNone
		}
		if !s.numberDone() {
			return s.fail()
		}
		ev := s.valueDone()
		if ev2 := s.step(c); ev2 != evNone { // the delimiter belongs to the container
			return ev2
		}
		return ev
	case sLiteral:
		if c != s.lit[0] {
			return s.fail()
		}
		s.lit = s.lit[1:]
		if s.lit == "" {
			return s.valueDone()
		}
		return evNone
	case sColon:
		if ws(c) {
			return evNone
		}
		if c != ':' {
			return s.fail()
		}
		s.state = sValue
		return evNone
	case sKeyOrEnd, sKey:
		if ws(c) {
			return evNone
		}
		if c == '}' && s.state == sKeyOrEnd {
			return s.closeContainer()
		}
		if c != '"' {
			return s.fail()
		}
		s.state, s.isKey = sString, true
		if len(s.stack) == 1 {
			return evTopKeyStart
		}
		return evNone
	case sAfterValue:
		if ws(c) {
			return evNone
		}
		top := s.stack[len(s.stack)-1]
		switch {
		case c == ',' && top == '{':
			s.state = sKey
		case c == ',' && top == '[':
			s.state = sValue
		case c == '}' && top == '{', c == ']' && top == '[':
			return s.closeContainer()
		default:
			return s.fail()
		}
		return evNone
	}
	// sValue, sValueOrEnd
	if ws(c) {
		return evNone
	}
	if c == ']' && s.state == sValueOrEnd {
		return s.closeContainer()
	}
	ev := evNone
	if len(s.stack) == 1 && s.stack[0] == '{' {
		s.topValue, ev = true, evTopValueStart
	}
	switch {
	case c == '{' || c == '[':
		if len(s.stack) >= maxJSONDepth {
			return s.fail()
		}
		s.stack = append(s.stack, c)
		s.state = sKeyOrEnd
		if c == '[' {
			s.state = sValueOrEnd
		}
	case c == '"':
		s.state, s.isKey = sString, false
	case c == '-' || c >= '0' && c <= '9':
		s.state, s.num = sNumber, 0
		s.number(c)
	case c == 't':
		s.state, s.lit = sLiteral, "rue"
	case c == 'f':
		s.state, s.lit = sLiteral, "alse"
	case c == 'n':
		s.state, s.lit = sLiteral, "ull"
	default:
		return s.fail()
	}
	return ev
}

// number advances the number grammar; false when c does not continue the number. States: 0
// start, 1 after '-', 2 after a leading '0', 3 integer digits, 4 after '.', 5 fraction digits,
// 6 after 'e', 7 after the exponent sign, 8 exponent digits.
func (s *scanner) number(c byte) bool {
	d := c >= '0' && c <= '9'
	switch s.num {
	case 0, 1:
		switch {
		case c == '-' && s.num == 0:
			s.num = 1
		case c == '0':
			s.num = 2
		case d:
			s.num = 3
		default:
			return false
		}
	case 2, 3:
		switch {
		case d && s.num == 3:
		case c == '.':
			s.num = 4
		case c == 'e' || c == 'E':
			s.num = 6
		default:
			return false
		}
	case 4, 5:
		switch {
		case d:
			s.num = 5
		case (c == 'e' || c == 'E') && s.num == 5:
			s.num = 6
		default:
			return false
		}
	case 6, 7:
		switch {
		case (c == '+' || c == '-') && s.num == 6:
			s.num = 7
		case d:
			s.num = 8
		default:
			return false
		}
	case 8:
		if !d {
			return false
		}
	}
	return true
}

// valueDone ends a scalar value at the current level.
func (s *scanner) valueDone() int {
	if len(s.stack) == 0 {
		s.afterRoot = true
		return evNone
	}
	s.state = sAfterValue
	if s.topValue && len(s.stack) == 1 {
		s.topValue = false
		return evTopValueEnd
	}
	return evNone
}

// closeContainer ends the innermost container.
func (s *scanner) closeContainer() int {
	s.stack = s.stack[:len(s.stack)-1]
	if len(s.stack) == 0 {
		s.afterRoot = true
		return evNone
	}
	s.state = sAfterValue
	if s.topValue && len(s.stack) == 1 {
		s.topValue = false
		return evTopValueEnd
	}
	return evNone
}

// finish says whether the input was exactly one valid JSON value (a root number may end at EOF).
func (s *scanner) finish() bool {
	if s.err {
		return false
	}
	if !s.afterRoot && s.state == sNumber && len(s.stack) == 0 {
		return s.numberDone()
	}
	return s.afterRoot
}
