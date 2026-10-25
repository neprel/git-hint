package runner

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestIOTranscriptLiteralGoldenAndOrder(t *testing.T) {
	cases := []IOTranscriptCase{{ID: "alpha", Status: 0, Output: []byte("A\x00B")}, {ID: "beta", Status: 0, Output: []byte("z\n")}}
	// Literal old framing, independent of the production helper's formatter/loop.
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("alpha\x000\x003\x00A\x00Bbeta\x000\x002\x00z\n")))
	got, e := IOTranscriptDigest(cases)
	if e != nil || got != expected {
		t.Fatal(got, expected, e)
	}
	cases[0], cases[1] = cases[1], cases[0]
	other, e := IOTranscriptDigest(cases)
	if e != nil || other == got {
		t.Fatal("lost declared order", e)
	}
	for _, bad := range [][]IOTranscriptCase{nil, {{ID: ""}}, {{ID: "x"}, {ID: "x"}}, {{ID: "x", Output: []byte(strings.Repeat("x", maxIOBytes+1))}}} {
		if _, e = IOTranscriptDigest(bad); e == nil {
			t.Fatal("invalid transcript")
		}
	}
}
