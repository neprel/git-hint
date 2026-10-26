package project

import "testing"

func TestCaptureSourceExactLoadedRefCopy(t *testing.T) {
	ref := CaptureSourceRef{"opaque-record", "sha256:opaque-digest"}
	h := &CaptureSourceInputs{ref: ref}
	got := h.Ref()
	got.RecordRef = "changed"
	if h.Ref() != ref {
		t.Fatal("Ref aliases handle")
	}
	if (&CaptureSourceInputs{}).Ref() != (CaptureSourceRef{}) || (*CaptureSourceInputs)(nil).Ref() != (CaptureSourceRef{}) {
		t.Fatal("zero source invents a reference")
	}
}
