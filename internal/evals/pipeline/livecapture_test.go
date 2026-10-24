package pipeline

import "testing"

func TestLiveCaptureOptIn(t *testing.T) {
	for _, c := range []struct {
		target, container string
		on, fail          bool
	}{{"", "", false, false}, {"192.168.1.111", "", false, true}, {"", "ai-qwen38-27b", false, true},
		{"192.168.1.111", "ai-qwen38-27b", true, false}, {"-oProxyCommand=x", "ai-qwen38-27b", false, true}, {"192.168.1.111", "c;id", false, true}} {
		t.Setenv("HINT_LIVE_CAPTURE_SSH", c.target)
		t.Setenv("HINT_LIVE_CAPTURE_CONTAINER", c.container)
		got, err := liveCapture()
		if (err != nil) != c.fail || (got != nil) != c.on {
			t.Errorf("%+v: %v %v", c, got, err)
		}
	}
}
