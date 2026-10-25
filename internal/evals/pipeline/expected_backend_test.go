package pipeline

import (
	"context"
	"github.com/neprel/git-hint/internal/evals/runner"
	"testing"
	"time"
)

func TestSuiteExpectedBackendRequiresLiveCaptureBeforeStore(t *testing.T) {
	for _, o := range []SuiteOptions{{ExpectedBackend: &runner.CaptureSnapshot{}}, {ExpectedBackend: &runner.CaptureSnapshot{}, Agent: "opencode", Live: &runner.Live{ModelLabel: "m"}}, {ExpectedBackend: &runner.CaptureSnapshot{}, Agent: "opencode", Live: &runner.Live{ModelLabel: "m"}, BackendCapture: &runner.SSHCapture{}}} {
		if _, e := RunSuite(context.Background(), nil, nil, "not-used", o, time.Time{}); e == nil {
			t.Fatal("incomplete guard reached store/runner")
		}
	}
}
