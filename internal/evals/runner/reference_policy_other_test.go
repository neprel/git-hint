//go:build !unix

package runner

import (
	"context"
	"errors"
	"testing"
)

func TestReferencePolicyUnsupportedSocketPlatform(t *testing.T) {
	if _, e := referenceSocketPinAt(context.Background(), "unused"); !errors.Is(e, ErrReferencePreparationPolicy) || e.Error() != ErrReferencePreparationPolicy.Error()+": platform_unsupported" {
		t.Fatal(e)
	}
}
