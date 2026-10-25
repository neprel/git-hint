//go:build darwin || linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRipgrepFIFORefusesWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if e := syscall.Mkfifo(p, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if b, e := readRipgrepBinary(ctx, p, 1, "unused"); e == nil || b != nil {
		t.Fatal("FIFO accepted")
	}
	if ctx.Err() != nil {
		t.Fatal("FIFO waited for a writer")
	}
}

func TestRipgrepDeviceRefuses(t *testing.T) {
	if b, e := readRipgrepBinary(context.Background(), os.DevNull, 1, "unused"); e == nil || b != nil {
		t.Fatal("device admitted")
	}
}
