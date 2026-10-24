//go:build unix

package trees

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type worktreeRoot struct {
	root     string
	anchor   *os.File
	strict   bool
	identity fileIdentity
}
type fileIdentity struct {
	dev, ino                                       uint64
	mode                                           uint32
	size, mtimeSec, mtimeNsec, ctimeSec, ctimeNsec int64
}

func identity(st *unix.Stat_t) fileIdentity {
	return fileIdentity{uint64(st.Dev), st.Ino, uint32(st.Mode), st.Size, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec}
}
func statFD(fd int) (fileIdentity, error) {
	var st unix.Stat_t
	e := unix.Fstat(fd, &st)
	return identity(&st), e
}
func statAt(fd int, p string) (fileIdentity, error) {
	var st unix.Stat_t
	e := unix.Fstatat(fd, p, &st, unix.AT_SYMLINK_NOFOLLOW)
	return identity(&st), e
}
func sameNode(a, b fileIdentity) bool { return a.dev == b.dev && a.ino == b.ino && a.mode == b.mode }

// Strict roots and all source descendants use atomic no-follow opens. Legacy root aliases
// are resolved once to preserve supported /tmp and user aliases; final link bytes remain legacy.
func openWorktreeRoot(root string, strict bool) (*worktreeRoot, error) {
	var e error
	root, e = filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if !strict {
		root, e = filepath.EvalSymlinks(root)
		if e != nil {
			return nil, e
		}
	}
	fd, e := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for _, part := range strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, unsupported("source_changed", "the repository source ancestry is unsafe")
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), "worktree-root")
	id, e := statFD(fd)
	if e != nil {
		f.Close()
		return nil, e
	}
	return &worktreeRoot{root: root, anchor: f, strict: strict, identity: id}, nil
}
func (s *worktreeRoot) close()                             { _ = s.anchor.Close() }
func (s *worktreeRoot) gitDirectory() (string, []*os.File) { return s.root, nil }
func (s *worktreeRoot) verify() error {
	other, e := openWorktreeRoot(s.root, s.strict)
	if e != nil {
		return e
	}
	defer other.close()
	if !sameNode(s.identity, other.identity) {
		return unsupported("source_changed", "the repository source identity changed")
	}
	return nil
}
func (s *worktreeRoot) boundParent(p string) (*os.File, string, []fileIdentity, error) {
	if !validPath(p) {
		return nil, "", nil, unsupported("baseline_missing", "invalid worktree path")
	}
	fd, e := unix.Dup(int(s.anchor.Fd()))
	if e != nil {
		return nil, "", nil, e
	}
	parts := strings.Split(p, "/")
	chain := []fileIdentity{s.identity}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil, "", nil, err
			}
			return nil, "", nil, unsupported("source_changed", "a worktree ancestor is unavailable or unsafe")
		}
		fd = next
		id, e := statFD(fd)
		if e != nil {
			_ = unix.Close(fd)
			return nil, "", nil, e
		}
		chain = append(chain, id)
	}
	return os.NewFile(uintptr(fd), "worktree-parent"), parts[len(parts)-1], chain, nil
}
func sameParents(a, b []fileIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameNode(a[i], b[i]) {
			return false
		}
	}
	return true
}
func (s *worktreeRoot) boundIdentity(p string) (fileIdentity, []fileIdentity, error) {
	f, name, chain, e := s.boundParent(p)
	if e != nil {
		return fileIdentity{}, nil, e
	}
	defer f.Close()
	id, e := statAt(int(f.Fd()), name)
	return id, chain, e
}

func (s *worktreeRoot) read(ctx context.Context, p string, limit int64, strict bool, hooks readHooks) (worktreeEntry, error) {
	var out worktreeEntry
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	parent, name, parents, e := s.boundParent(p)
	if errors.Is(e, unix.ENOENT) {
		out.absent = true
		return out, nil
	}
	if e != nil {
		return out, e
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	before, e := statAt(pfd, name)
	if errors.Is(e, unix.ENOENT) {
		out.absent = true
		return out, nil
	}
	if e != nil {
		return out, e
	}
	switch before.mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if strict {
			return out, unsupported("symlink_unsupported", "the strict capture rejects symlinks")
		}
		if limit < 0 || before.size > limit {
			return out, unsupported("binary_too_large", "a link exceeds the read bound")
		}
		linkBound := int64(1 << 20)
		if limit < linkBound {
			linkBound = limit + 1
		}
		data := make([]byte, linkBound)
		n, e := unix.Readlinkat(pfd, name, data)
		if e != nil {
			return out, e
		}
		if int64(n) > limit || n == len(data) {
			return out, unsupported("binary_too_large", "a link exceeds the read bound")
		}
		after, endParents, e := s.boundIdentity(p)
		if e != nil || !sameParents(parents, endParents) || after != before {
			return out, unsupported("source_changed", "a symlink changed during capture")
		}
		out.data, out.mode = data[:n], "120000"
		return out, nil
	case unix.S_IFREG:
	default:
		if strict {
			return out, unsupported("special_file_unsupported", "the strict capture rejects special files")
		}
		out.skip = true
		return out, nil
	}
	if before.size > limit || limit < 0 {
		return out, unsupported("binary_too_large", "a file exceeds the read bound")
	}
	if hooks.beforeOpen != nil {
		hooks.beforeOpen()
	}
	fd, e := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return out, unsupported("source_changed", "the file changed before open")
	}
	f := os.NewFile(uintptr(fd), "worktree-file")
	defer f.Close()
	opened, e := statFD(fd)
	if e != nil || opened != before {
		return out, unsupported("source_changed", "the opened file identity differs")
	}
	if opened.mode&unix.S_IFMT != unix.S_IFREG {
		return out, unsupported("source_changed", "the opened file is not regular")
	}
	data, e := readRegular(ctx, f, limit, hooks)
	if e != nil {
		return out, e
	}
	end, e := statFD(fd)
	if e != nil || end != opened || int64(len(data)) != opened.size {
		return out, unsupported("source_changed", "the file changed during its read")
	}
	pathEnd, endParents, e := s.boundIdentity(p)
	if e != nil || !sameParents(parents, endParents) || pathEnd != end {
		return out, unsupported("source_changed", "the source path no longer names the read file")
	}
	if e = s.verify(); e != nil {
		return out, e
	}
	out.mode = "100644"
	if end.mode&0o111 != 0 {
		out.mode = "100755"
	}
	out.data = data
	return out, nil
}
