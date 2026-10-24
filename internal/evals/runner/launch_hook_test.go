package runner

import (
	"errors"
	"testing"
)

func TestLaunchHookReceivesTheActualFactsOnceBeforeTheStart(t *testing.T) {
	var got []LaunchFacts
	r, c, s := studyFake(t, studyFakeOptions{hook: func(f LaunchFacts) error { got = append(got, f); return nil }})
	receipt := studyReceipt(t, r)
	planned, err := DockerLaunchDigestWithPolicy(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(got) != 1 || s.starts != 1 || receipt.State != "prepared" {
		t.Fatalf("hook calls %d starts %d receipt %+v", len(got), s.starts, receipt)
	}
	want := LaunchFacts{LaunchDigest: planned, ExecutionDigest: receipt.ExecutionDigest, GatePolicyDigest: GatePolicyDigest(), Target: "linux-arm64", Model: "m"}
	if got[0] != want {
		t.Fatalf("facts %+v, want %+v", got[0], want)
	}
	if run := c.Report().AgentRun; run == nil || run.Outcome != AgentExited || run.ExecutionDigest != want.ExecutionDigest {
		t.Fatalf("agent run %+v", run)
	}
}

func TestLaunchHookErrorRefusesBeforeTheStart(t *testing.T) {
	r, c, s := studyFake(t, studyFakeOptions{hook: func(LaunchFacts) error { return errors.New("journal unavailable") }})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.starts != 0 || c.Requests() != 0 || r.ExceptionInfo == nil {
		t.Fatalf("starts %d requests %d exception %+v", s.starts, c.Requests(), r.ExceptionInfo)
	}
	if run := c.Report().AgentRun; run == nil || run.Outcome != AgentRefusedBeforeLaunch {
		t.Fatalf("agent run %+v", run)
	}
}

// The hook records a prestart intent: a failed start request after it is start_unconfirmed.
func TestLaunchHookIsAnIntentNotAConfirmedStart(t *testing.T) {
	calls := 0
	_, c, s := studyFake(t, studyFakeOptions{failStart: true, hook: func(LaunchFacts) error { calls++; return nil }})
	s.mu.Lock()
	defer s.mu.Unlock()
	if calls != 1 || s.starts != 1 {
		t.Fatalf("hook calls %d starts %d", calls, s.starts)
	}
	if run := c.Report().AgentRun; run == nil || run.Outcome != AgentStartUnconfirmed {
		t.Fatalf("agent run %+v", run)
	}
}

func TestLaunchHookRequiresAStudyLaunch(t *testing.T) {
	o := RunOptions{Agent: "opencode", BeforeAgentStart: func(LaunchFacts) error { return nil }}
	if o.checkStudyLaunch() == nil {
		t.Fatal("a hook without a study launch was accepted")
	}
	o.BeforeAgentStart = nil
	if o.checkStudyLaunch() != nil {
		t.Fatal("the nil hook changed the legacy options check")
	}
}

func TestGatePolicyDigestIsTheGateIdentityOfTheLaunchDigest(t *testing.T) {
	d := GatePolicyDigest()
	if !prelaunchDigestPattern.MatchString(d) || d != GatePolicyDigest() {
		t.Fatalf("gate policy digest %q", d)
	}
	a, err := DockerLaunchDigestWithPolicy(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil || a == d {
		t.Fatalf("launch digest %q equals the gate identity", a)
	}
}
