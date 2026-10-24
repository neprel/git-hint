package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// fakeTransport answers each Inspect in order on the fake clock: step advances the monotonic
// clock during the round trip, block waits for the context, lateBy replies after a real delay.
type fakeTransport struct {
	mu      sync.Mutex
	clock   *fakeClock
	answers []fakeAnswer
	calls   int
	counter *Counter // when set, each call records whether the counter's seal had started
	sealed  []bool
}

type fakeAnswer struct {
	raw    rawCapture
	err    error
	step   time.Duration
	block  bool
	lateBy time.Duration
}

func (f *fakeTransport) Inspect(ctx context.Context) (rawCapture, error) {
	f.mu.Lock()
	a := f.answers[f.calls]
	f.calls++
	if f.counter != nil {
		f.sealed = append(f.sealed, f.counter.sealStarted())
	}
	f.mu.Unlock()
	if a.block {
		<-ctx.Done()
		return rawCapture{}, ctx.Err()
	}
	if a.lateBy > 0 {
		time.Sleep(a.lateBy)
	}
	if f.clock != nil {
		f.clock.step(0, a.step)
	}
	return a.raw, a.err
}

func raw() rawCapture {
	t, n, l := true, int64(0), int64(262144)
	return rawCapture{ImageID: "sha256:" + strings.Repeat("a", 64), Host: "capture-host.example.invalid", Container: "example-model-container-0f1e2d", ServedModel: "qwen3.6-27b",
		StartedAt: "2026-10-24T09:00:00Z", RestartCount: &n, PromptTokensDetails: &t, PrefixCaching: &t, MaxModelLen: &l,
		ReasoningParser: "qwen3", ToolCallParser: "qwen3_coder"}
}

func testRef(kind, raw string) string { return hint.DeriveID([]byte("k"), kind, raw) }

// checkedAll runs the public agent_run and backend_capture rules on the account's wire form.
func checkedAll(t *testing.T, r *LedgerReport) string {
	t.Helper()
	if p := checked(t, r); p != "" {
		return p
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return hint.LedgerBackendCaptureProblem(m)
}

// The deferred mode exposes no account before the one final assembly: Seal and Report return nil,
// an assembly before the seal is refused, the first assembly after the drain wins, a later one is
// refused and changes nothing; the mode cannot be chosen once a seal started.
func TestDeferredAssembly(t *testing.T) {
	c, f := agentCounter()
	if !c.deferAssembly() {
		t.Fatal("the deferred mode is refused before any seal")
	}
	start := captureSide{status: CaptureNotAttempted}
	if c.assemble(start, start) == nil {
		t.Fatal("an assembly before the seal passed")
	}
	admitDone(c, f, 10*time.Millisecond)
	if r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer); r != nil || c.Report() != nil {
		t.Fatal("an account is exposed before the final assembly")
	}
	if c.deferAssembly() {
		t.Fatal("the deferred mode was chosen after the seal")
	}
	if err := c.assemble(start, captureSide{status: CaptureTimeout}); err != nil {
		t.Fatal(err)
	}
	first := c.Report()
	if first == nil || first.BackendCapture == nil || first.BackendCapture.End.Status != CaptureTimeout {
		t.Fatalf("assembled account %+v", first)
	}
	if c.assemble(captureSide{status: CaptureError}, start) == nil || c.Report().BackendCapture.Start.Status != CaptureNotAttempted {
		t.Fatal("a second assembly changed the account")
	}
}

// Without the deferred mode Seal and Report behave as before and an account already returned
// can never acquire captures.
func TestLegacyAccountNeverAcquiresCaptures(t *testing.T) {
	c, f := agentCounter()
	admitDone(c, f, 10*time.Millisecond)
	r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
	if r == nil || r.BackendCapture != nil || c.Report() == nil {
		t.Fatal("the legacy seal no longer returns its account")
	}
	if c.assemble(captureSide{status: CaptureComplete}, captureSide{status: CaptureComplete}) == nil || c.Report().BackendCapture != nil {
		t.Fatal("a legacy account acquired captures")
	}
}

// A complete capture brackets the round trip on the counter's clock; equal floored readings are
// valid; the snapshot carries references only (no raw host, container or model name), an
// unlisted parser as other and unreported values as null.
func TestCaptureBracketAndSanitizing(t *testing.T) {
	c, f := agentCounter()
	f.step(0, 100*time.Millisecond)
	unreported := raw()
	unreported.RestartCount, unreported.PromptTokensDetails, unreported.PrefixCaching, unreported.MaxModelLen = nil, nil, nil, nil
	unreported.StartedAt, unreported.ServedModel, unreported.ReasoningParser, unreported.ToolCallParser = "0001-01-01T00:00:00Z", "", "", "qwen9_coder"
	tr := &fakeTransport{clock: f, answers: []fakeAnswer{{raw: raw(), step: 380 * time.Millisecond}, {raw: unreported}}}
	k := newCapturer(tr, testRef)
	a := k.capture(context.Background(), c)
	b := k.capture(context.Background(), c)
	if a.status != CaptureComplete || *a.send != 100*time.Millisecond || *a.recv != 480*time.Millisecond {
		t.Fatalf("bracket %+v", a)
	}
	if b.status != CaptureComplete || *b.send != *b.recv { // no step: the same floored reading
		t.Fatalf("equal readings %+v", b)
	}
	js, _ := json.Marshal([]any{a.snap, b.snap})
	for _, secret := range []string{"capture-host.example.invalid", "example-model-cont", "example-model-name"} {
		if strings.Contains(string(js), secret) {
			t.Fatalf("a raw identifier is recorded: %s", js)
		}
	}
	if a.snap.HostRef != testRef("capture-host", "capture-host.example.invalid") || *a.snap.Flags.ServedModelRef != testRef("capture-served-model", "qwen3.6-27b") {
		t.Fatalf("references %+v", a.snap)
	}
	fl := b.snap.Flags
	if b.snap.StartedAt != nil || b.snap.RestartCount != nil || fl.PromptTokensDetails != nil || fl.PrefixCaching != nil || fl.MaxModelLen != nil ||
		fl.ReasoningParser != nil || fl.ServedModelRef != nil || *fl.ToolCallParser != "other" {
		t.Fatalf("unreported values are not null: %s", js)
	}
}

func TestUnusableResponsesAreErrors(t *testing.T) {
	long := raw()
	long.Host = strings.Repeat("h", maxCaptureField+1)
	neg := int64(-1)
	zero := int64(0)
	for name, r := range map[string]rawCapture{
		"image tag":         func() rawCapture { x := raw(); x.ImageID = "ghcr.io/x/y:latest"; return x }(),
		"no host":           func() rawCapture { x := raw(); x.Host = ""; return x }(),
		"no container":      func() rawCapture { x := raw(); x.Container = ""; return x }(),
		"negative restarts": func() rawCapture { x := raw(); x.RestartCount = &neg; return x }(),
		"zero length":       func() rawCapture { x := raw(); x.MaxModelLen = &zero; return x }(),
		"over-long value":   long,
	} {
		c, f := agentCounter()
		s := newCapturer(&fakeTransport{clock: f, answers: []fakeAnswer{{raw: r}}}, testRef).capture(context.Background(), c)
		if s.status != CaptureError || s.send == nil || s.recv != nil || s.snap != nil {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

// Bounds: a capture that does not answer within its bound is a timeout; a reply after the bound
// is not used; a cancellation is an error; a spent budget or a cancelled trial is not attempted.
// No capture is repeated.
func TestCaptureBoundsAndCancellation(t *testing.T) {
	run := func(a fakeAnswer, ctx context.Context, budget time.Duration) (captureSide, int) {
		c, f := agentCounter()
		tr := &fakeTransport{clock: f, answers: []fakeAnswer{a, a}}
		k := newCapturer(tr, testRef)
		k.bound, k.budget = 30*time.Millisecond, budget
		return k.capture(ctx, c), tr.calls
	}
	if s, n := run(fakeAnswer{block: true}, context.Background(), time.Minute); s.status != CaptureTimeout || s.send == nil || s.recv != nil || n != 1 {
		t.Fatalf("timeout %+v calls %d", s, n)
	}
	if s, _ := run(fakeAnswer{raw: raw(), lateBy: 60 * time.Millisecond}, context.Background(), time.Minute); s.status != CaptureTimeout || s.snap != nil {
		t.Fatalf("late reply %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if s, _ := run(fakeAnswer{block: true}, ctx, time.Minute); s.status != CaptureError || s.send == nil {
		t.Fatalf("cancelled %+v", s)
	}
	if s, n := run(fakeAnswer{raw: raw()}, context.Background(), 0); s.status != CaptureNotAttempted || s.send != nil || n != 0 {
		t.Fatalf("spent budget %+v calls %d", s, n)
	}
	if s, n := run(fakeAnswer{raw: raw()}, ctx, time.Minute); s.status != CaptureNotAttempted || n != 0 {
		t.Fatalf("cancelled trial %+v calls %d", s, n)
	}
	if s, _ := run(fakeAnswer{err: errors.New("connection refused")}, context.Background(), time.Minute); s.status != CaptureError {
		t.Fatalf("transport error %+v", s)
	}
}

// The assembled account passes the public rules, its offsets appear only with entries (never for
// zero admissions or over the entry bound), and a returned account shares no memory with the next.
func TestAssembledCapturesInTheAccount(t *testing.T) {
	c, f := agentCounter()
	c.deferAssembly()
	k := newCapturer(&fakeTransport{clock: f, answers: []fakeAnswer{{raw: raw(), step: 300 * time.Millisecond}, {raw: raw(), step: 400 * time.Millisecond}}}, testRef)
	f.step(0, 100*time.Millisecond)
	start := k.capture(context.Background(), c)
	eng := &agentFakeEngine{onStart: func() { admitDone(c, f, 10*time.Millisecond) }, waits: wl(exitAfter(f, time.Second, 0, ""))}
	_ = runAgentEnclosed(context.Background(), eng, "id", time.Minute, c, testDigest)
	c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
	end := k.capture(context.Background(), c)
	c.assemble(start, end)
	r := c.Report()
	bc := r.BackendCapture
	if *bc.Start.SendMs != 100 || *bc.Start.RecvMs != 400 || *bc.Start.RecvMs > *r.AgentRun.EnclosureStartMs || *bc.End.SendMs < *r.FinalizedMs {
		t.Fatalf("order: start %v..%v enclosure %v end %v finalized %v", *bc.Start.SendMs, *bc.Start.RecvMs, *r.AgentRun.EnclosureStartMs, *bc.End.SendMs, *r.FinalizedMs)
	}
	if p := checkedAll(t, r); p != "" {
		t.Fatalf("the assembled account fails the public rules: %s", p)
	}
	*bc.Start.Snapshot.Flags.MaxModelLen, *bc.End.SendMs = 1, 0
	again := c.Report()
	if *again.BackendCapture.Start.Snapshot.Flags.MaxModelLen != 262144 || *again.BackendCapture.End.SendMs == 0 {
		t.Fatal("a returned account shares memory with the ledger")
	}
	c.admit() // late: a new revision repeats the captures
	if late := c.Report(); late.Revision != 1 || *late.BackendCapture.End.RecvMs != *again.BackendCapture.End.RecvMs {
		t.Fatal("a late request changed the captures")
	}
	// zero admissions: snapshots and statuses, no offsets
	z, zf := agentCounter()
	z.deferAssembly()
	zk := newCapturer(&fakeTransport{clock: zf, answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}, testRef)
	zs := zk.capture(context.Background(), z)
	z.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	z.assemble(zs, zk.capture(context.Background(), z))
	if zr := z.Report(); zr.BackendCapture.Start.SendMs != nil || zr.BackendCapture.End.RecvMs != nil || zr.BackendCapture.Start.Snapshot == nil || checkedAll(t, zr) != "" {
		t.Fatalf("zero admissions: %+v", zr.BackendCapture)
	}
	// over the entry bound: aggregate-only, no offsets
	o, of := agentCounter()
	o.limit = 2000
	o.deferAssembly()
	ok := newCapturer(&fakeTransport{clock: of, answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}, testRef)
	ostart := ok.capture(context.Background(), o)
	for i := 0; i < 1001; i++ {
		admitDone(o, of, 0)
	}
	o.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	o.assemble(ostart, ok.capture(context.Background(), o))
	if orep := o.Report(); orep.Entries != nil || orep.BackendCapture.Start.SendMs != nil || orep.BackendCapture.End.SendMs != nil {
		t.Fatalf("over the bound: %+v", orep.BackendCapture)
	}
}

// CB-1: the assembly takes owned copies: changing the caller's snapshot, flags or offsets after
// the assembly changes neither the account nor a later revision.
func TestAssemblyOwnsItsCaptures(t *testing.T) {
	c, f := agentCounter()
	c.deferAssembly()
	admitDone(c, f, 10*time.Millisecond)
	c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
	snap, err := sanitize(raw(), testRef)
	if err != nil {
		t.Fatal(err)
	}
	d := time.Duration(0)
	side := captureSide{status: CaptureComplete, send: &d, recv: &d, snap: snap}
	if err := c.assemble(side, side); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(c.Report().BackendCapture)
	snap.ImageID, snap.HostRef = "sha256:"+strings.Repeat("f", 64), strings.Repeat("9", 32)
	*snap.StartedAt, *snap.RestartCount, *snap.Flags.MaxModelLen, *snap.Flags.PrefixCaching = "2030-01-01T00:00:00Z", 7, 1, false
	*snap.Flags.ToolCallParser, *snap.Flags.ServedModelRef = "hermes", strings.Repeat("8", 32)
	snap.Flags.PromptTokensDetails = nil
	d = 99 * time.Millisecond
	after, _ := json.Marshal(c.Report().BackendCapture)
	c.admit() // late: a new revision
	late, _ := json.Marshal(c.Report().BackendCapture)
	if string(before) != string(after) || string(before) != string(late) {
		t.Fatalf("the stored captures changed through the caller's values:\n%s\n%s\n%s", before, after, late)
	}
}

// manualNow is an injected clock of the active capture time.
type manualNow struct {
	mu sync.Mutex
	t  time.Time
}

func (m *manualNow) now() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.t }
func (m *manualNow) add(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}

// stepTransport advances the injected active-time clock by its cost on every inspect.
type stepTransport struct {
	clock *manualNow
	cost  time.Duration
	calls int
}

func (s *stepTransport) Inspect(context.Context) (rawCapture, error) {
	s.calls++
	s.clock.add(s.cost)
	return raw(), nil
}

// CB-2: the budget is the combined active capture time. A long agent run between the captures
// (176 s here, or 300 s) consumes none of it: the end capture is still attempted; once the
// captures themselves used the budget, a further capture is not attempted.
func TestCaptureBudgetCountsOnlyActiveTime(t *testing.T) {
	for _, between := range []time.Duration{176 * time.Second, 300 * time.Second} {
		c, _ := agentCounter()
		m := &manualNow{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
		tr := &stepTransport{clock: m, cost: time.Millisecond}
		k := newCapturer(tr, testRef)
		k.now = m.now
		if s := k.capture(context.Background(), c); s.status != CaptureComplete {
			t.Fatalf("start capture %+v", s)
		}
		m.add(between) // the agent's run, setup and drain
		if s := k.capture(context.Background(), c); s.status != CaptureComplete || tr.calls != 2 || k.spent != 2*time.Millisecond {
			t.Fatalf("%v between: end capture %+v calls %d spent %v", between, s, tr.calls, k.spent)
		}
	}
	// exhaustion by active time only: two captures of 70 s each spend the 120 s budget
	c, _ := agentCounter()
	m := &manualNow{t: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
	tr := &stepTransport{clock: m, cost: 70 * time.Second}
	k := newCapturer(tr, testRef)
	k.now = m.now
	k.capture(context.Background(), c)
	k.capture(context.Background(), c)
	if s := k.capture(context.Background(), c); s.status != CaptureNotAttempted || tr.calls != 2 {
		t.Fatalf("after the active budget: %+v calls %d", s, tr.calls)
	}
	// on the real clock: a hanging capture uses its bound, the next one only the rest, then none
	c2, f2 := agentCounter()
	hang := &fakeTransport{clock: f2, answers: []fakeAnswer{{block: true}, {block: true}, {block: true}}}
	k2 := newCapturer(hang, testRef)
	k2.bound, k2.budget = 30*time.Millisecond, 50*time.Millisecond
	a, b, z := k2.capture(context.Background(), c2), k2.capture(context.Background(), c2), k2.capture(context.Background(), c2)
	if a.status != CaptureTimeout || b.status != CaptureTimeout || z.status != CaptureNotAttempted || hang.calls != 2 {
		t.Fatalf("real-clock budget: %s %s %s calls %d", a.status, b.status, z.status, hang.calls)
	}
}
