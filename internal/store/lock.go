package store

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// ErrBusy is returned when another process or caller holds the project lock.
var ErrBusy = errors.New("another HINT operation runs on this project; try again when it ends")

// Lock is an exclusive lock on the project state (`.hint/state/lock`). Collect, export
// marking, sending and plan writing take it, so two callers (CLI or MCP) never write the
// snapshot, mapping or spool state at the same time. The lock file holds no data.
type Lock struct{ f *os.File }

var inProcess sync.Mutex // flock does not exclude goroutines of one process on every system

// LockState takes the project lock. With wait=false it returns ErrBusy when the lock is
// held.
func (s *Store) LockState(wait bool) (*Lock, error) {
	if wait {
		inProcess.Lock()
	} else if !inProcess.TryLock() {
		return nil, ErrBusy
	}
	p := s.Path("state", "lock")
	if err := s.CheckPath(p); err != nil {
		inProcess.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		inProcess.Unlock()
		return nil, err
	}
	if err := lockFile(f, wait); err != nil {
		f.Close()
		inProcess.Unlock()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	l.f.Close()
	l.f = nil
	inProcess.Unlock()
}
