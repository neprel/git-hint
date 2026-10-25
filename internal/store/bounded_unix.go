//go:build unix

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type stateNode struct {
	dev, ino                       uint64
	mode                           uint32
	size, mtsec, mtns, ctsec, ctns int64
}

func stateNodeFD(fd int) (stateNode, error) {
	var st unix.Stat_t
	e := unix.Fstat(fd, &st)
	return stateNode{uint64(st.Dev), st.Ino, uint32(st.Mode), st.Size, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec}, e
}
func stateSameNode(a, b stateNode) bool { return a.dev == b.dev && a.ino == b.ino && a.mode == b.mode }
func stateError(e error) error {
	if errors.Is(e, fs.ErrNotExist) {
		return fs.ErrNotExist
	}
	if errors.Is(e, fs.ErrExist) {
		return fs.ErrExist
	}
	return ErrBoundedState
}

func openStateParent(root string, parts []string, create bool) (int, []stateNode, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || len(parts) < 1 {
		return -1, nil, ErrBoundedState
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return -1, nil, ErrBoundedState
	}
	var nodes []stateNode
	walk := func(part string, mkdir bool) error {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && mkdir {
			if err = unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return stateError(err)
			}
			if err == nil {
				if e := unix.Fsync(fd); e != nil {
					return ErrBoundedState
				}
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return stateError(err)
		}
		_ = unix.Close(fd)
		fd = next
		n, err := stateNodeFD(fd)
		if err != nil {
			return ErrBoundedState
		}
		nodes = append(nodes, n)
		return nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if part != "" {
			if e = walk(part, false); e != nil {
				unix.Close(fd)
				return -1, nil, e
			}
		}
	}
	for _, part := range parts[:len(parts)-1] {
		if e = walk(part, create); e != nil {
			unix.Close(fd)
			return -1, nil, e
		}
	}
	return fd, nodes, nil
}
func verifyStateParents(root string, parts []string, before []stateNode) error {
	fd, after, e := openStateParent(root, parts, false)
	if e != nil {
		return e
	}
	defer unix.Close(fd)
	if len(before) != len(after) {
		return ErrBoundedState
	}
	for i := range before {
		if !stateSameNode(before[i], after[i]) {
			return ErrBoundedState
		}
	}
	return nil
}
func openBoundedState(root string, parts []string, dir bool) (*boundedStateSource, error) {
	parent, chain, e := openStateParent(root, parts, false)
	if e != nil {
		return nil, e
	}
	defer unix.Close(parent)
	flags := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if dir {
		flags |= unix.O_DIRECTORY
	}
	fd, e := unix.Openat(parent, parts[len(parts)-1], flags, 0)
	if e != nil {
		return nil, stateError(e)
	}
	f := os.NewFile(uintptr(fd), "bounded-store-source")
	before, e := stateNodeFD(fd)
	if e != nil {
		f.Close()
		return nil, ErrBoundedState
	}
	if (dir && before.mode&unix.S_IFMT != unix.S_IFDIR) || (!dir && before.mode&unix.S_IFMT != unix.S_IFREG) {
		f.Close()
		return nil, ErrBoundedState
	}
	verify := func() error {
		after, e := stateNodeFD(fd)
		if e != nil || after != before {
			return ErrBoundedState
		}
		if e = verifyStateParents(root, parts, chain); e != nil {
			return e
		}
		p, _, e := openStateParent(root, parts, false)
		if e != nil {
			return e
		}
		defer unix.Close(p)
		next, e := unix.Openat(p, parts[len(parts)-1], flags, 0)
		if e != nil {
			return stateError(e)
		}
		defer unix.Close(next)
		named, e := stateNodeFD(next)
		if e != nil || named != after {
			return ErrBoundedState
		}
		return nil
	}
	return &boundedStateSource{f, verify}, nil
}
func writeBoundedState(ctx context.Context, root string, parts []string, data []byte) error {
	parent, chain, e := openStateParent(root, parts, true)
	if e != nil {
		return e
	}
	defer unix.Close(parent)
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return ErrBoundedState
	}
	tmp := ".tmp-" + hex.EncodeToString(random[:])
	fd, e := unix.Openat(parent, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if e != nil {
		return stateError(e)
	}
	f := os.NewFile(uintptr(fd), "bounded-store-write")
	defer f.Close()
	defer unix.Unlinkat(parent, tmp, 0)
	for len(data) > 0 {
		if e = ctx.Err(); e != nil {
			return e
		}
		n := min(len(data), 32<<10)
		written, err := f.Write(data[:n])
		if err != nil || written != n {
			return ErrBoundedState
		}
		data = data[n:]
	}
	if e = f.Sync(); e != nil {
		return ErrBoundedState
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = verifyStateParents(root, parts, chain); e != nil {
		return e
	}
	if e = unix.Linkat(parent, tmp, parent, parts[len(parts)-1], 0); e != nil {
		return stateError(e)
	}
	if e = unix.Fsync(parent); e != nil {
		return ErrBoundedState
	}
	if e = verifyStateParents(root, parts, chain); e != nil {
		return e
	}
	written, e := stateNodeFD(fd)
	if e != nil {
		return ErrBoundedState
	}
	named, e := unix.Openat(parent, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return stateError(e)
	}
	actual, e := stateNodeFD(named)
	_ = unix.Close(named)
	if e != nil || actual != written {
		return ErrBoundedState
	}
	return ctx.Err()
}
func openBoundedLock(s *Store) (*os.File, error) {
	parts := []string{Dir, "state", "lock"}
	parent, _, e := openStateParent(s.Root, parts, false)
	if e != nil {
		return nil, e
	}
	defer unix.Close(parent)
	fd, e := unix.Openat(parent, "lock", unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, stateError(e)
	}
	f := os.NewFile(uintptr(fd), "bounded-store-lock")
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, ErrBoundedState
	}
	return f, nil
}

func boundedRootIdentity(root string) (string, error) {
	fd, chain, e := openStateParent(root, []string{"unused"}, false)
	if e != nil {
		return "", e
	}
	defer unix.Close(fd)
	if len(chain) == 0 {
		return "", ErrBoundedState
	}
	n := chain[len(chain)-1]
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", n.dev, n.ino, n.mode)))
	return fmt.Sprintf("sha256:%x", h), nil
}

func ensureBoundedStateDir(ctx context.Context, root string, parts []string) error {
	fd, chain, e := openStateParent(root, append(parts, "unused"), true)
	if e != nil {
		return e
	}
	defer unix.Close(fd)
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = unix.Fsync(fd); e != nil {
		return ErrBoundedState
	}
	return verifyStateParents(root, append(parts, "unused"), chain)
}
