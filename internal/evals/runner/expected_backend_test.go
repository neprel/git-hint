package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func expectedFixture(t *testing.T) *CaptureSnapshot {
	t.Helper()
	s, e := sanitize(raw(), testRef)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestExpectedBackendCompleteSnapshotAndCopy(t *testing.T) {
	good := expectedFixture(t)
	if !completeExpectedBackend(good) {
		t.Fatal("complete fixture rejected")
	}
	copied := good.Copy()
	*good.StartedAt = "changed"
	*good.Flags.PrefixCaching = false
	if *copied.StartedAt == "changed" || !*copied.Flags.PrefixCaching {
		t.Fatal("snapshot aliases input")
	}
	if (*CaptureSnapshot)(nil).Copy() != nil {
		t.Fatal("nil copy changed")
	}
	for _, field := range []string{"image", "host", "container", "started", "restart", "details", "prefix", "length", "reasoning", "tool", "served", "restart-bound", "length-bound", "reasoning-other", "tool-other", "invalid-time", "invalid-ref"} {
		t.Run(field, func(t *testing.T) {
			s := copied.Copy()
			switch field {
			case "image":
				s.ImageID = ""
			case "host":
				s.HostRef = ""
			case "container":
				s.ContainerRef = ""
			case "started":
				s.StartedAt = nil
			case "restart":
				s.RestartCount = nil
			case "details":
				s.Flags.PromptTokensDetails = nil
			case "prefix":
				s.Flags.PrefixCaching = nil
			case "length":
				s.Flags.MaxModelLen = nil
			case "reasoning":
				s.Flags.ReasoningParser = nil
			case "tool":
				s.Flags.ToolCallParser = nil
			case "served":
				s.Flags.ServedModelRef = nil
			case "restart-bound":
				*s.RestartCount = -1
			case "length-bound":
				*s.Flags.MaxModelLen = 0
			case "reasoning-other":
				*s.Flags.ReasoningParser = "other"
			case "tool-other":
				*s.Flags.ToolCallParser = "other"
			case "invalid-time":
				*s.StartedAt = "invalid"
			case "invalid-ref":
				*s.Flags.ServedModelRef = "raw-name"
			}
			if completeExpectedBackend(s) {
				t.Fatal("missing/unknown fact admitted")
			}
		})
	}
}

func TestExpectedBackendGuardUsesOneStartCaptureAfterSetup(t *testing.T) {
	f := &fakeTransport{answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}
	expected := expectedFixture(t)
	starts := 0
	capturedAfterSetup := false
	r, c := liveFake(t, liveFakeOpts{prelaunch: true, digest: true, requestInWait: true, capture: f, expectedBackend: expected, engineEvent: func(e string) {
		if e == "GET archive" && !capturedAfterSetup {
			if f.calls != 0 {
				t.Error("capture occurred before setup")
			}
			capturedAfterSetup = true
		}
		if e == "agent start" {
			starts++
			if f.calls != 1 {
				t.Error("capture did not precede start")
			}
		}
	}})
	l := persisted(t, r)
	if !capturedAfterSetup || starts != 1 || c.Requests() != 1 || f.calls != 2 || l.BackendCapture.Start.Status != CaptureComplete || l.AgentRun.Outcome != AgentExited {
		t.Fatal("guarded fake positive", starts, c.Requests(), f.calls, r.ExceptionInfo)
	}
}

func TestExpectedBackendMismatchRefusesBeforeAgentStart(t *testing.T) {
	for _, field := range []string{"image", "host", "container", "started", "restart", "details", "prefix", "length", "reasoning", "tool", "served", "missing"} {
		t.Run(field, func(t *testing.T) {
			actual := raw()
			switch field {
			case "image":
				actual.ImageID = "sha256:" + strings.Repeat("b", 64)
			case "host":
				actual.Host += "other"
			case "container":
				actual.Container += "other"
			case "started":
				actual.StartedAt = "2026-10-24T09:00:01Z"
			case "restart":
				v := int64(1)
				actual.RestartCount = &v
			case "details":
				v := false
				actual.PromptTokensDetails = &v
			case "prefix":
				v := false
				actual.PrefixCaching = &v
			case "length":
				v := int64(8192)
				actual.MaxModelLen = &v
			case "reasoning":
				actual.ReasoningParser = "deepseek_r1"
			case "tool":
				actual.ToolCallParser = "hermes"
			case "served":
				actual.ServedModel += "other"
			case "missing":
				actual.StartedAt = ""
			}
			f := &fakeTransport{answers: []fakeAnswer{{raw: actual}, {raw: actual}}}
			starts := 0
			r, c := liveFake(t, liveFakeOpts{digest: true, requestInWait: true, capture: f, expectedBackend: expectedFixture(t), engineEvent: func(e string) {
				if e == "agent start" {
					starts++
				}
			}})
			if starts != 0 || c.Requests() != 0 || r.ExceptionInfo == nil || f.calls != 2 {
				t.Fatal("mismatching backend reached agent", starts, c.Requests(), f.calls)
			}
			l := persisted(t, r)
			if l.AgentRun.Outcome != AgentRefusedBeforeLaunch || l.AgentRun.EnclosureStartMs != nil {
				t.Fatal("guard refusal enclosure")
			}
		})
	}
}

func TestExpectedBackendErrorAndParentCancellationRefuse(t *testing.T) {
	for _, name := range []string{"error", "cancel", "late", "missing-transport", "invalid-expected"} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTransport{answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}
			o := liveFakeOpts{digest: true, requestInWait: true, capture: f, expectedBackend: expectedFixture(t)}
			switch name {
			case "error":
				f.answers[0].err = errors.New("synthetic inspect error")
			case "cancel":
				f.answers[0].block = true
				o.cancelAfter = 30 * time.Millisecond
			case "late":
				f.answers[0].lateBy = 60 * time.Millisecond
				o.cancelAfter = 30 * time.Millisecond
			case "missing-transport":
				o.capture = nil
			case "invalid-expected":
				o.expectedBackend.StartedAt = nil
			}
			starts := 0
			o.engineEvent = func(e string) {
				if e == "agent start" {
					starts++
				}
			}
			r, c := liveFake(t, o)
			if starts != 0 || c.Requests() != 0 || r.ExceptionInfo == nil {
				t.Fatal("unavailable capture reached agent", starts, c.Requests())
			}
		})
	}
}

func TestExpectedBackendFreezesBeforeSetup(t *testing.T) {
	expected := expectedFixture(t)
	f := &fakeTransport{answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}
	r, c := liveFake(t, liveFakeOpts{prelaunch: true, digest: true, requestInWait: true, capture: f, expectedBackend: expected, engineEvent: func(e string) {
		if e == "GET archive" {
			*expected.RestartCount = 99
		}
	}})
	if c.Requests() != 1 || persisted(t, r).AgentRun.Outcome != AgentExited {
		t.Fatal("caller mutation changed frozen expected value")
	}
}

func TestExpectedBackendNilPreservesExistingCaptureTiming(t *testing.T) {
	f := &fakeTransport{answers: []fakeAnswer{{raw: raw()}, {raw: raw()}}}
	sawSetup := false
	_, _ = liveFake(t, liveFakeOpts{digest: true, requestInWait: true, capture: f, engineEvent: func(e string) {
		if e == "GET archive" && !sawSetup {
			sawSetup = true
			if f.calls != 1 {
				t.Error("nil changed legacy start capture order")
			}
		}
	}})
	if !sawSetup || f.calls != 2 {
		t.Fatal("legacy capture count changed")
	}
}

func TestExpectedBackendMissingTransportRefusesBeforeEngine(t *testing.T) {
	d := &Docker{}
	_, e := d.Run(context.Background(), "not-read", RunOptions{ExpectedBackend: expectedFixture(t), Agent: "opencode"})
	if e == nil {
		t.Fatal("missing capture admitted")
	}
}

func TestExpectedBackendFalseIsObservedButNilIsUnknown(t *testing.T) {
	actual := raw()
	v := false
	actual.PromptTokensDetails = &v
	actual.PrefixCaching = &v
	s, e := sanitize(actual, testRef)
	if e != nil {
		t.Fatal(e)
	}
	if !expectedBackendMatches(s.Copy(), captureSide{status: CaptureComplete, snap: s}) {
		t.Fatal("observed false lost")
	}
	missing := s.Copy()
	missing.Flags.PrefixCaching = nil
	if expectedBackendMatches(s, captureSide{status: CaptureComplete, snap: missing}) {
		t.Fatal("missing actual became false")
	}
}

func TestExpectedBackendSetupFailureKeepsStartNotAttempted(t *testing.T) {
	f := &fakeTransport{answers: []fakeAnswer{{raw: raw()}}}
	starts := 0
	r, c := liveFake(t, liveFakeOpts{prelaunch: true, cutArchive: true, digest: true, requestInWait: true, capture: f, expectedBackend: expectedFixture(t), engineEvent: func(e string) {
		if e == "agent start" {
			starts++
		}
	}})
	l := persisted(t, r)
	if r.ExceptionInfo == nil || starts != 0 || c.Requests() != 0 || f.calls != 1 || l.BackendCapture.Start.Status != CaptureNotAttempted || l.BackendCapture.Start.Snapshot != nil {
		t.Fatal("setup failure fabricated a start capture")
	}
}
