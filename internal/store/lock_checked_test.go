//go:build unix

package store

import (
	"context"
	"testing"
)

func TestUnlockCheckedReportsRealClosedDescriptorAndReleasesOwnership(t *testing.T) {
	s := boundedFixture(t)
	lock, e := s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = lock.f.Close(); e != nil {
		t.Fatal(e)
	}
	if e = lock.UnlockChecked(); e == nil {
		t.Fatal("closed descriptor release error hidden")
	}
	next, e := s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal("in-process ownership retained", e)
	}
	if e = next.UnlockChecked(); e != nil {
		t.Fatal(e)
	}
	if e = next.UnlockChecked(); e != nil {
		t.Fatal("duplicate release changed")
	}
}
