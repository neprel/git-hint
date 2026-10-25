//go:build unix

package store

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestWriteStateContextPreservesBytesAndRefusesCancelledOrNil(t *testing.T) {
	s := boundedFixture(t)
	data := []byte("synthetic pointer bytes\n")
	if e := s.WriteState(data, "state", "ordinary.json"); e != nil {
		t.Fatal(e)
	}
	if e := s.WriteStateContext(context.Background(), data, "state", "context.json"); e != nil {
		t.Fatal(e)
	}
	a, e := os.ReadFile(s.Path("state", "ordinary.json"))
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(s.Path("state", "context.json"))
	if e != nil || !bytes.Equal(a, b) {
		t.Fatal("legacy byte parity", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []context.Context{nil, ctx} {
		if e := s.WriteStateContext(c, []byte("replacement"), "state", "context.json"); e == nil {
			t.Fatal("invalid context wrote bytes")
		}
	}
	b, e = os.ReadFile(s.Path("state", "context.json"))
	if e != nil || !bytes.Equal(b, data) {
		t.Fatal("cancel changed pointer")
	}
}
