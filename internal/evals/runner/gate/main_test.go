package main

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// No sockets, engine or wall-clock waits drive the idle-window controls.
type fakeIdleClock struct {
	mu     sync.Mutex
	at     time.Time
	t      *fakeIdleTimer
	resets chan time.Duration
}
type fakeIdleTimer struct {
	c       chan time.Time
	owner   *fakeIdleClock
	due     time.Time
	stopped bool
}

func newFakeClock() *fakeIdleClock {
	return &fakeIdleClock{at: time.Now(), resets: make(chan time.Duration, 100)}
}
func (c *fakeIdleClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *fakeIdleClock) timer(d time.Duration) idleTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = &fakeIdleTimer{c: make(chan time.Time, 1), owner: c, due: c.at.Add(d)}
	return c.t
}
func (t *fakeIdleTimer) channel() <-chan time.Time { return t.c }
func (t *fakeIdleTimer) reset(d time.Duration) {
	c := t.owner
	c.mu.Lock()
	t.due = c.at.Add(d)
	c.mu.Unlock()
	select {
	case c.resets <- d:
	default:
	}
}
func (t *fakeIdleTimer) stop() { t.owner.mu.Lock(); defer t.owner.mu.Unlock(); t.stopped = true }
func (c *fakeIdleClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	if c.t != nil && !c.t.stopped && !c.at.Before(c.t.due) {
		select {
		case c.t.c <- c.at:
		default:
		}
	}
}
func (c *fakeIdleClock) stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t != nil && c.t.stopped
}
func (c *fakeIdleClock) waitReset(t *testing.T, want time.Duration) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-c.resets:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatal("watchdog did not recheck current activity")
		}
	}
}

type flow struct {
	client, server net.Conn
	done           chan struct{}
}

func openFlow(idle time.Duration, clock idleClock) flow {
	client, c := net.Pipe()
	u, server := net.Pipe()
	done := make(chan struct{})
	go func() { forwardConnected(c, u, idle, clock); close(done) }()
	return flow{client, server, done}
}
func (f flow) close() { f.client.Close(); f.server.Close() }
func joined(t *testing.T, f flow) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("copy workers/watchdog did not join")
	}
}
func transfer(t *testing.T, from, to net.Conn) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, e := from.Write([]byte("x")); done <- e }()
	to.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, e := io.ReadFull(to, b[:]); e != nil {
		t.Fatal("traffic closed", e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

func TestBidirectionalIdleTrafficRenewsEitherDirection(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "request"}[reverse], func(t *testing.T) {
			c := newFakeClock()
			f := openFlow(10*time.Second, c)
			defer f.close()
			// Establish both workers and the timer before advancing virtual time.
			transfer(t, f.client, f.server)
			for i := 0; i < 4; i++ {
				c.advance(9 * time.Second)
				if reverse {
					transfer(t, f.client, f.server)
				} else {
					transfer(t, f.server, f.client)
				}
			}
			select {
			case <-f.done:
				t.Fatal("one quiet direction ended active traffic")
			default:
			}
			f.close()
			joined(t, f)
			if !c.stopped() {
				t.Fatal("watchdog timer retained")
			}
		})
	}
}

// This also exercises the old directional-deadline regression with real net.Pipe
// deadline semantics. Only this short control uses elapsed wall time.
func TestOneWayResponseOutlivesIdleWindow(t *testing.T) {
	f := openFlow(150*time.Millisecond, wallIdleClock{})
	defer f.close()
	transfer(t, f.client, f.server)
	for i := 0; i < 5; i++ {
		time.Sleep(50 * time.Millisecond)
		transfer(t, f.server, f.client)
	}
	f.close()
	joined(t, f)
}

func TestBothDirectionsIdleCloses(t *testing.T) {
	c := newFakeClock()
	f := openFlow(10*time.Second, c)
	defer f.close()
	transfer(t, f.client, f.server)
	c.advance(10 * time.Second)
	joined(t, f)
	if !c.stopped() {
		t.Fatal("watchdog retained")
	}
}

func TestStaleExpiryRechecksNewActivity(t *testing.T) {
	c := newFakeClock()
	f := openFlow(10*time.Second, c)
	defer f.close()
	transfer(t, f.client, f.server)
	c.advance(9 * time.Second)
	transfer(t, f.server, f.client)
	// Even an old queued expiry must compare against the latest shared activity.
	c.advance(2 * time.Second)
	c.mu.Lock()
	select {
	case c.t.c <- c.at:
	default:
	}
	c.mu.Unlock()
	c.waitReset(t, 8*time.Second)
	transfer(t, f.server, f.client)
	f.close()
	joined(t, f)
}

func TestStalledWriteRetainsItsBound(t *testing.T) {
	// Do not advance the watchdog's fake clock: only the real net.Pipe write
	// deadline can release this blocked write, not the shared idle expiry.
	c := newFakeClock()
	f := openFlow(30*time.Millisecond, c)
	defer f.close()
	// Nobody reads the forwarded bytes, so the existing bounded write must end.
	sent := make(chan error, 1)
	go func() { _, e := f.client.Write([]byte("x")); sent <- e }()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("input read blocked")
	}
	joined(t, f)
}

func TestEOFAndExternalCancellationJoin(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "server EOF", true: "client cancellation"}[closeClient], func(t *testing.T) {
			c := newFakeClock()
			f := openFlow(time.Hour, c)
			defer f.close()
			transfer(t, f.client, f.server)
			if closeClient {
				f.client.Close()
			} else {
				f.server.Close()
			}
			joined(t, f)
			if !c.stopped() {
				t.Fatal("watchdog timer retained")
			}
		})
	}
}
