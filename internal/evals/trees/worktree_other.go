//go:build !unix

package trees

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
)

// Strict descriptor-relative no-follow capture is currently Unix-only. Legacy retains
// its platform selection and bounded descriptor reads without claiming strict authority.
type worktreeRoot struct{ root string }

func openWorktreeRoot(root string, strict bool) (*worktreeRoot, error) {
	if strict {
		return nil, unsupported("capture_platform_unsupported", "strict capture requires descriptor-relative no-follow opens")
	}
	return &worktreeRoot{root}, nil
}
func (s *worktreeRoot) close()                             {}
func (s *worktreeRoot) closeErr() error                    { return nil }
func (s *worktreeRoot) gitDirectory() (string, []*os.File) { return s.root, nil }
func (s *worktreeRoot) verify() error                      { return nil }
func (s *worktreeRoot) read(ctx context.Context, p string, limit int64, strict bool, hooks readHooks) (worktreeEntry, error) {
	var out worktreeEntry
	name := filepath.Join(s.root, filepath.FromSlash(p))
	before, e := os.Lstat(name)
	if os.IsNotExist(e) {
		out.absent = true
		return out, nil
	}
	if e != nil {
		return out, e
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		target, e := os.Readlink(name)
		if e != nil {
			return out, e
		}
		if int64(len(target)) > limit {
			return out, unsupported("binary_too_large", "a link exceeds its bound")
		}
		out.data, out.mode = []byte(target), "120000"
		return out, nil
	}
	if !before.Mode().IsRegular() {
		out.skip = true
		return out, nil
	}
	if before.Size() > limit {
		return out, unsupported("binary_too_large", "a file exceeds its bound")
	}
	f, e := os.Open(name)
	if e != nil {
		return out, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return out, unsupported("source_changed", "source changed before open")
	}
	data, e := readRegular(ctx, f, limit, hooks)
	if e != nil {
		return out, e
	}
	end, e := f.Stat()
	pathEnd, pe := os.Lstat(name)
	if e != nil || pe != nil || !os.SameFile(end, pathEnd) || end.Size() != opened.Size() || end.Mode() != opened.Mode() || !end.ModTime().Equal(opened.ModTime()) || int64(len(data)) != opened.Size() {
		return out, unsupported("source_changed", "source changed during read")
	}
	out.data, out.mode = data, "100644"
	if end.Mode()&0o111 != 0 {
		out.mode = "100755"
	}
	return out, nil
}

func (s *worktreeRoot) receiptRootIdentity() string { return "" }
