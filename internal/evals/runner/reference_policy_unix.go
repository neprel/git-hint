//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func referenceSocketPinAt(ctx context.Context, path string) (referenceSocketPin, error) {
	var zero referenceSocketPin
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || strings.IndexFunc(path, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return zero, referencePolicyFailure("socket_invalid")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return zero, referencePolicyFailure("socket_invalid")
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return zero, referencePolicyFailure("socket_invalid")
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts[:len(parts)-1] {
		if ctx.Err() != nil {
			return zero, referencePolicyFailure("socket_context")
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return zero, referencePolicyFailure("socket_invalid")
		}
		_ = unix.Close(fd)
		fd = next
	}
	var st unix.Stat_t
	if e = unix.Fstatat(fd, parts[len(parts)-1], &st, unix.AT_SYMLINK_NOFOLLOW); e != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Getuid()) || ctx.Err() != nil {
		return zero, referencePolicyFailure("socket_invalid")
	}
	return referenceSocketPin{Device: uint64(st.Dev), Inode: st.Ino, Mode: uint32(st.Mode), UID: st.Uid, GID: st.Gid, Size: st.Size, MTimeSec: st.Mtim.Sec, MTimeNS: st.Mtim.Nsec, CTimeSec: st.Ctim.Sec, CTimeNS: st.Ctim.Nsec}, nil
}
