//go:build !unix

package trees

import (
	"context"
	"testing"
)

func TestSelectedReceiptUnsupportedPlatformDoesNotFallback(t *testing.T) {
	if s, e := OpenSelectedSourceSession(context.Background(), lib(t), t.TempDir(), SourceSessionOptions{}); s != nil || !isCode(e, "capture_platform_unsupported") {
		t.Fatal("strict source session fell back", e)
	}
}
