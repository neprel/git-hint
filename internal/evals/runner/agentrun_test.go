package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/neprel/git-hint/internal/hint"
)

// agentFakeEngine plays the engine calls of the agent phase on the fake clock: onStart runs inside
// ContainerStart before it returns (an agent can send requests then), waits answers each
// ContainerWait call in order.
type agentFakeEngine struct {
	onStart  func()
	startErr error
	waits    []func(ctx context.Context) client.ContainerWaitResult
	stops    int
	state    *container.State
	calls    []string
}

func (f *agentFakeEngine) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.calls = append(f.calls, "start")
	if f.onStart != nil {
		f.onStart()
	}
	return client.ContainerStartResult{}, f.startErr
}

func (f *agentFakeEngine) ContainerWait(ctx context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	f.calls = append(f.calls, "wait")
	w := f.waits[0]
	f.waits = f.waits[1:]
	return w(ctx)
}

func (f *agentFakeEngine) ContainerStop(ctx context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.calls = append(f.calls, "stop")
	f.stops++
	return client.ContainerStopResult{}, errors.New("stop reply lost") // only the confirming wait decides
}

func (f *agentFakeEngine) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.calls = append(f.calls, "inspect")
	if f.state == nil {
		return client.ContainerInspectResult{}, errors.New("no such container")
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{State: f.state}}, nil
}

// exitAfter answers the wait with an exit status after the fake clock advanced by d.
func exitAfter(f *fakeClock, d time.Duration, code int64, msg string) func(context.Context) client.ContainerWaitResult {
	return func(context.Context) client.ContainerWaitResult {
		f.step(0, d)
		r, e := make(chan container.WaitResponse, 1), make(chan error, 1)
		resp := container.WaitResponse{StatusCode: code}
		if msg != "" {
			resp.Error = &container.WaitExitError{Message: msg}
		}
		r <- resp
		return client.ContainerWaitResult{Result: r, Error: e}
	}
}

// untilDone answers the wait only with the context's error (timeout or cancellation).
func untilDone(ctx context.Context) client.ContainerWaitResult {
	r, e := make(chan container.WaitResponse), make(chan error, 1)
	go func() { <-ctx.Done(); e <- ctx.Err() }()
	return client.ContainerWaitResult{Result: r, Error: e}
}

func waitErr(err error) func(context.Context) client.ContainerWaitResult {
	return func(context.Context) client.ContainerWaitResult {
		r, e := make(chan container.WaitResponse), make(chan error, 1)
		e <- err
		return client.ContainerWaitResult{Result: r, Error: e}
	}
}

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// agentCounter is a counter on the fake clock, anchored at 0 like NewTrial.
func agentCounter() (*Counter, *fakeClock) {
	f := &fakeClock{wall: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
	c := &Counter{token: "t", limit: 10}
	c.ledger.clock = f.clock()
	c.ledger.anchor()
	return c, f
}

// admitDone admits one request and finishes it complete after d.
func admitDone(c *Counter, f *fakeClock, d time.Duration) {
	e, _ := c.admit()
	f.step(0, d)
	v := int64(4)
	c.finish(e, EntryComplete, &usageComponents{Prompt: &v, Completion: &v}, entryUsage())
}

// checked runs the public agent_run rule on the account as the wire form carries it.
func checked(t *testing.T, r *LedgerReport) string {
	t.Helper()
	b, _ := json.Marshal(r)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	counts := m["counts"].(map[string]any)
	admitted := 0.0
	for k, v := range counts {
		if k != "late" && k != "rejected_limit" {
			admitted += v.(float64)
		}
	}
	counts["admitted"] = admitted
	return hint.LedgerAgentRunProblem(m)
}

// A request admitted while ContainerStart has not yet returned lies inside the enclosure: the
// start is read before the call. An exit with status 0 gives the whole enclosure, ordered
// start <= admissions <= end <= seal, and the engine times are kept as UTC descriptions.
func TestAgentRunExitedEnclosesEarlyRequests(t *testing.T) {
	c, f := agentCounter()
	f.step(0, 500*time.Millisecond) // image, gate and container setup after the anchor
	eng := &agentFakeEngine{state: &container.State{StartedAt: "2026-10-24T12:00:00.5+02:00", FinishedAt: "2026-10-24T10:05:02Z"}}
	eng.onStart = func() {
		f.step(0, 30*time.Millisecond)
		admitDone(c, f, 40*time.Millisecond) // before the engine's reply returns
		f.step(0, 30*time.Millisecond)
	}
	eng.waits = append(eng.waits, exitAfter(f, 2*time.Second, 0, ""))
	if err := runAgentEnclosed(context.Background(), eng, "id", time.Minute, c, testDigest); err != nil {
		t.Fatal(err)
	}
	f.step(0, 100*time.Millisecond)
	r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
	a := r.AgentRun
	if a == nil || a.Outcome != AgentExited || *a.ExitCode != 0 || *a.EnclosureStartMs != 500 || *a.EnclosureEndMs != 2600 || a.ExecutionDigest != testDigest {
		t.Fatalf("agent run %+v", a)
	}
	if r.Entries[0].AdmittedMs != 530 || *r.SealedMs != 2700 {
		t.Fatalf("admitted %d sealed %d", r.Entries[0].AdmittedMs, *r.SealedMs)
	}
	if *a.EngineStartedAt != "2026-10-24T10:00:00.5Z" || *a.EngineFinishedAt != "2026-10-24T10:05:02Z" {
		t.Fatalf("engine times %v %v", *a.EngineStartedAt, *a.EngineFinishedAt)
	}
	if p := checked(t, r); p != "" {
		t.Fatalf("the produced enclosure fails the public rule: %s", p)
	}
	// a start read after the reply would leave the admission outside: the rule refuses it
	late := int64(531)
	r.AgentRun.EnclosureStartMs = &late
	if checked(t, r) == "" {
		t.Fatal("an enclosure that starts after an admission passed")
	}
}

func TestAgentRunOutcomes(t *testing.T) {
	cases := map[string]struct {
		eng      func(f *fakeClock) *agentFakeEngine
		ctx      func() context.Context
		outcome  string
		code     *int
		end      bool
		stops    int
		errCheck func(error) bool
	}{
		"nonzero exit": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(exitAfter(f, time.Second, 3, ""))}
			},
			outcome: AgentExitedNonzero, code: ip(3), end: true,
			errCheck: func(err error) bool { var e exitError; return errors.As(err, &e) && e.code == 3 },
		},
		"timeout, stop confirmed": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(untilDone, exitAfter(f, time.Second, 137, ""))}
			},
			outcome: AgentStoppedAfterTimeout, end: true, stops: 1,
			errCheck: func(err error) bool { var e timeoutError; return errors.As(err, &e) },
		},
		"cancelled, stop confirmed": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(untilDone, exitAfter(f, time.Second, 137, ""))}
			},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(20*time.Millisecond, cancel)
				return ctx
			},
			outcome: AgentStoppedAfterCancel, end: true, stops: 1,
			errCheck: func(err error) bool { return err != nil },
		},
		"wait error, stop confirmed": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(waitErr(errors.New("engine gone")), exitAfter(f, time.Second, 0, ""))}
			},
			outcome: AgentStoppedAfterWaitError, end: true, stops: 1,
			errCheck: func(err error) bool { return err != nil && err.Error() == "engine gone" },
		},
		"wait result error, stop confirmed": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(exitAfter(f, time.Second, 0, "removal failed"), exitAfter(f, time.Second, 0, ""))}
			},
			outcome: AgentStoppedAfterWaitError, end: true, stops: 1,
			errCheck: func(err error) bool { return err != nil && err.Error() == "agent container: removal failed" },
		},
		"timeout, stop not confirmed": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(untilDone, waitErr(errors.New("no reply")))}
			},
			outcome: AgentStopUnconfirmed, stops: 1,
			errCheck: func(err error) bool { var e timeoutError; return errors.As(err, &e) },
		},
		"stop confirmation with a wait error message": {
			eng: func(f *fakeClock) *agentFakeEngine {
				return &agentFakeEngine{waits: wl(waitErr(errors.New("engine gone")), exitAfter(f, time.Second, 0, "still running"))}
			},
			outcome: AgentStopUnconfirmed, stops: 1,
			errCheck: func(err error) bool { return err != nil },
		},
		"start error": {
			eng:      func(f *fakeClock) *agentFakeEngine { return &agentFakeEngine{startErr: errors.New("reply lost")} },
			outcome:  AgentStartUnconfirmed,
			errCheck: func(err error) bool { return err != nil && err.Error() == "reply lost" },
		},
	}
	for name, tc := range cases {
		c, f := agentCounter()
		f.step(0, 200*time.Millisecond)
		eng := tc.eng(f)
		eng.onStart = func() { admitDone(c, f, 10*time.Millisecond) }
		ctx := context.Background()
		if tc.ctx != nil {
			ctx = tc.ctx()
		}
		err := runAgentEnclosed(ctx, eng, "id", 30*time.Millisecond, c, testDigest)
		if !tc.errCheck(err) {
			t.Errorf("%s: phase error %v", name, err)
		}
		f.step(0, 50*time.Millisecond)
		r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
		a := r.AgentRun
		if a == nil || a.Outcome != tc.outcome || (a.EnclosureEndMs != nil) != tc.end || *a.EnclosureStartMs != 200 || eng.stops != tc.stops {
			t.Errorf("%s: agent run %+v stops %d", name, a, eng.stops)
			continue
		}
		if (tc.code == nil) != (a.ExitCode == nil) || (tc.code != nil && *tc.code != *a.ExitCode) {
			t.Errorf("%s: exit code %v", name, a.ExitCode)
		}
		if a.EngineStartedAt != nil || eng.calls[len(eng.calls)-1] != "inspect" {
			t.Errorf("%s: engine times without a readable inspect, calls %v", name, eng.calls)
		}
		if p := checked(t, r); p != "" {
			t.Errorf("%s: the produced record fails the public rule: %s", name, p)
		}
	}
}

func wl(w ...func(context.Context) client.ContainerWaitResult) []func(context.Context) client.ContainerWaitResult {
	return w
}

func ip(v int) *int { return &v }

// Without an admitted request the account has no entries and no timing: the outcome stays, the
// offsets are null (the run interval is unknown); a refusal before the launch has none at all.
func TestAgentRunWithoutEntries(t *testing.T) {
	c, f := agentCounter()
	eng := &agentFakeEngine{waits: wl(exitAfter(f, time.Second, 0, ""))}
	if err := runAgentEnclosed(context.Background(), eng, "id", time.Minute, c, testDigest); err != nil {
		t.Fatal(err)
	}
	r := c.Seal(0, BypassEnforced, ScopeIsolatedContainer)
	if a := r.AgentRun; a.Outcome != AgentExited || a.EnclosureStartMs != nil || a.EnclosureEndMs != nil || r.Entries != nil || checked(t, r) != "" {
		t.Fatalf("zero admissions: %+v", a)
	}
	refused, _ := agentCounter()
	refused.recordAgentRun(agentRun{outcome: AgentRefusedBeforeLaunch, digest: testDigest})
	r = refused.Seal(0, BypassUnverified, ScopeIsolatedContainer)
	if a := r.AgentRun; a.Outcome != AgentRefusedBeforeLaunch || a.ExitCode != nil || a.EnclosureStartMs != nil || checked(t, r) != "" {
		t.Fatalf("refused before launch: %+v", a)
	}
	// without an execution digest no record is made (lifecycle unknown)
	none, f2 := agentCounter()
	_ = runAgentEnclosed(context.Background(), &agentFakeEngine{waits: wl(exitAfter(f2, time.Second, 0, ""))}, "id", time.Minute, none, "")
	if r := none.Seal(0, BypassEnforced, ScopeIsolatedContainer); r.AgentRun != nil {
		t.Fatalf("a record without a digest: %+v", r.AgentRun)
	}
}

// The record is frozen with the account: a record after the seal or a second record changes
// nothing, a late request adds a revision with the same agent_run, and the report never shares
// memory with the ledger.
func TestAgentRunFrozen(t *testing.T) {
	c, f := agentCounter()
	eng := &agentFakeEngine{onStart: func() { admitDone(c, f, 10*time.Millisecond) }, waits: wl(exitAfter(f, time.Second, 0, ""))}
	_ = runAgentEnclosed(context.Background(), eng, "id", time.Minute, c, testDigest)
	c.recordAgentRun(agentRun{outcome: AgentExitedNonzero, digest: testDigest})
	first := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
	c.recordAgentRun(agentRun{outcome: AgentStartUnconfirmed, digest: testDigest})
	*first.AgentRun.ExitCode = 9
	c.admit() // late
	again := c.Report()
	if again.Revision != 1 || again.AgentRun.Outcome != AgentExited || *again.AgentRun.ExitCode != 0 || *again.AgentRun.EnclosureEndMs != *first.AgentRun.EnclosureEndMs {
		t.Fatalf("the record changed after the seal: %+v", again.AgentRun)
	}
}

// A wall-clock step during the agent changes only clock_step_ms; the enclosure and the
// admissions stay on the monotonic offsets, so no false refusal.
func TestAgentRunUnderWallStep(t *testing.T) {
	for _, jump := range []time.Duration{-90 * time.Minute, 3 * time.Hour} {
		c, f := agentCounter()
		eng := &agentFakeEngine{onStart: func() { admitDone(c, f, 10*time.Millisecond); f.step(jump, 0) }, waits: wl(exitAfter(f, time.Second, 0, ""))}
		_ = runAgentEnclosed(context.Background(), eng, "id", time.Minute, c, testDigest)
		r := c.Seal(time.Second, BypassEnforced, ScopeIsolatedContainer)
		if *r.ClockStepMs != jump.Milliseconds() || *r.AgentRun.EnclosureStartMs != 0 || *r.AgentRun.EnclosureEndMs != 1010 || checked(t, r) != "" {
			t.Fatalf("jump %v: step %d run %+v", jump, *r.ClockStepMs, r.AgentRun)
		}
	}
}

// NewTrial anchors the ledger before any container exists; an engine time of year 1 is unset.
func TestNewTrialAnchorsAndEngineTimes(t *testing.T) {
	lv := &Live{trials: map[string]*Counter{}}
	if c := lv.NewTrial(3); c.ledger.anchorWall.IsZero() || c.ledger.clock == nil {
		t.Fatal("the trial's ledger is not anchored at creation")
	}
	if engineTime("0001-01-01T00:00:00Z") != nil || engineTime("") != nil || engineTime("yesterday") != nil {
		t.Fatal("an unset engine time is recorded")
	}
}
