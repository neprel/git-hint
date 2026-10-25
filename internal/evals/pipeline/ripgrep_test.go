package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/runner"
)

func TestSuiteRipgrepPlanningUsesCommonRuntime(t *testing.T) {
	o := SuiteOptions{Agent: "opencode", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, Live: &runner.Live{ModelLabel: "m"}}
	old, e := suiteLaunchDigest(o, "linux-arm64")
	if e != nil {
		t.Fatal(e)
	}
	want, e := runner.DockerLaunchDigestWithPolicy(runner.DockerOpenCodeVersion, "m", "linux-arm64", o.StudyLaunchPolicy)
	if e != nil || old != want {
		t.Fatal("old suite recipe changed")
	}
	o.RipgrepBinary = "explicit-local-artifact"
	got, e := suiteLaunchDigest(o, "linux-arm64")
	if e != nil || got == old {
		t.Fatal("suite omits runtime identity", e)
	}
	want, e = runner.DockerLaunchDigestWithRipgrep(runner.DockerOpenCodeVersion, "m", "linux-arm64", o.StudyLaunchPolicy)
	if e != nil || got != want {
		t.Fatal("suite uses a second recipe")
	}
	if _, e = suiteLaunchDigest(o, "linux-arm64-musl"); e == nil {
		t.Fatal("unsupported target admitted")
	}
}

func TestSuiteRipgrepOptInRefusesBeforeStoreOrRunner(t *testing.T) {
	for _, o := range []SuiteOptions{{RipgrepBinary: "rg"}, {RipgrepBinary: "rg", Agent: "opencode", Live: &runner.Live{ModelLabel: "m"}}, {RipgrepBinary: "rg", Agent: "oracle", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, Live: &runner.Live{ModelLabel: "m"}}} {
		if _, e := RunSuite(context.Background(), nil, nil, "test", o, time.Time{}); e == nil {
			t.Fatal("invalid opt-in reached store/runner")
		}
	}
}
