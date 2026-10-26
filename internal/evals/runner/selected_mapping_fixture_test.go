package runner

// Test-only adapters around the SAME owned fake handler. Nothing here executes
// commands, opens a real engine or forwards a model request.
import (
	"archive/tar"
	"context"
	"testing"
)

type SelectedMappingTestNode struct {
	Deleted          bool
	Path, Body, Link string
	Mode             int64
	Type             byte
}
type SelectedMappingTestRunner struct {
	Before func(string)
	T      *testing.T
	Nodes  []SelectedMappingTestNode
	Starts int
}

func (r *SelectedMappingTestRunner) SupportsStudyLaunchPolicy(p StudyLaunchPolicy) bool {
	return p == StudyLaunchSingleRootV1
}
func (r *SelectedMappingTestRunner) PreflightTarget(context.Context, string) (string, error) {
	return "linux-arm64", nil
}
func (r *SelectedMappingTestRunner) Run(ctx context.Context, task string, opts RunOptions) ([]Trial, error) {
	if r.Before != nil {
		r.Before(task)
	}
	extra := map[string]storedStudyFile{}
	omit := map[string]bool{}
	for _, n := range r.Nodes {
		if n.Deleted {
			omit[n.Path] = true
			continue
		}
		typ := n.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := n.Mode
		if mode == 0 {
			mode = 0644
		}
		extra[n.Path] = storedStudyFile{tar.Header{Typeflag: typ, Mode: mode, Linkname: n.Link, Uid: 65534, Gid: 65534}, []byte(n.Body)}
	}
	_, _, state := studyFake(r.T, studyFakeOptions{parent: ctx, taskDir: task, external: &opts, skipRequest: true, extraApp: extra, omitApp: omit})
	state.mu.Lock()
	r.Starts += state.starts
	state.mu.Unlock()
	reward := 0.0
	return []Trial{{Name: "synthetic-mapping-terminal", Reward: &reward, Checks: map[string]string{}, Exception: "synthetic verifier unavailable"}}, nil
}
func SelectedMappingTestLive(t *testing.T) (*Live, string) {
	t.Helper()
	file := studySourceForTest(t)
	live, _ := fakeModel(t)
	return live, file
}
