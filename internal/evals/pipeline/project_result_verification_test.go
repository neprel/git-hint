package pipeline

import (
	"context"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/project"
	"testing"
)

func TestSelectedResultMissingCustodyRefuses(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, e := CheckSelectedProjectResult(ctx, &SelectedProjectMapping{}, &ObservedProjectResult{}); e == nil {
			t.Fatal("zero custody")
		}
		if _, e := VerifySelectedProjectResult(ctx, &SelectedProjectResult{}, &project.PreparedFinalReference{}, runner.FrozenVerificationPolicyRef{}); e == nil {
			t.Fatal("zero verification")
		}
	}
	var zero *SelectedProjectResult
	if zero.Ready() {
		t.Fatal("nil ready")
	}
}
