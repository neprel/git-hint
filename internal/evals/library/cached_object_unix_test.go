//go:build unix

package library

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"testing"
	"time"
)

func TestCachedFIFORefusesPromptlyInValidStore(t *testing.T) {
	l := Open(project(t), 0)
	data := []byte("synthetic cache probe")
	p := cachedPath(t, l, data)
	if e := unix.Mkfifo(p, 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.Lstat(p)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := l.PutObjectContext(ctx, data); done <- e }()
	select {
	case e = <-done:
		if !errors.Is(e, ErrCorrupt) {
			t.Fatalf("valid store did not reach cache type refusal: %v", e)
		}
	case <-time.After(2 * time.Second):
		// Permit the original broken reader to join before reporting its failure.
		fd, e := unix.Open(p, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if e == nil {
			unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("cached FIFO blocked bounded persistence")
	}
	after, e := os.Lstat(p)
	if e != nil || !os.SameFile(before, after) || after.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("refusal changed FIFO", e)
	}
}

func TestCachedReplacementByFIFOAfterMetadataRefuses(t *testing.T) {
	l := Open(project(t), 0)
	data := []byte("cache")
	p := cachedPath(t, l, data)
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	// Replacement after open must not turn stable regular-copy repair into unlink.
	_, e := l.checkCachedObject(context.Background(), p, data, func() {
		if e := os.Rename(p, p+".original"); e != nil {
			t.Fatal(e)
		}
		if e := unix.Mkfifo(p, 0600); e != nil {
			t.Fatal(e)
		}
	})
	if e == nil {
		t.Fatal("replacement FIFO accepted")
	}
	fi, e := os.Lstat(p)
	if e != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("replacement FIFO changed", e)
	}
}
