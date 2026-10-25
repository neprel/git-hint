//go:build darwin || linux

package runner

import (
	"os"
	"syscall"
)

func openRetainedPrelaunchFile(name string) (*os.File, error) {
	fd, e := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}
