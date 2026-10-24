//go:build unix

package library

import (
	"golang.org/x/sys/unix"
	"os"
)

func openCachedRegular(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}
