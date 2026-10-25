package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// checkCachedObject handles only the exact content-addressed cache slot. It never
// inventories a tree. A stable damaged REGULAR copy is repairable; unsafe targets
// and changing sources refuse without removal. afterOpen is a deterministic test
// seam, not an admission option; the ordinary writer always passes nil.
func (l *Library) checkCachedObject(ctx context.Context, path string, want []byte, afterOpen func()) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(l.s.Root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	rel, err := filepath.Rel(l.s.Root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return false, ErrEscape
	}
	// Keep every existing ancestor identity. Root also prevents a concurrent link
	// from redirecting a descriptor outside the project. No cached link is followed.
	parts := strings.Split(rel, string(filepath.Separator))
	names, infos := []string{}, []fs.FileInfo{}
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		info, e := root.Lstat(name)
		if errors.Is(e, fs.ErrNotExist) {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return false, ErrEscape
		}
		if i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return false, fmt.Errorf("%w: cached object path is not regular", ErrCorrupt)
		}
		names, infos = append(names, name), append(infos, info)
	}
	before := infos[len(infos)-1]
	f, err := openCachedRegular(root, rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !sameCachedInfo(before, opened) || !opened.Mode().IsRegular() {
		return false, ErrCorrupt
	}
	if afterOpen != nil {
		afterOpen()
	}
	// Read at most expected length + one byte. Oversized stable regular copies
	// need no complete read to prove damage, including sparse or growing files.
	old, err := readCachedPrefix(ctx, f, int64(len(want)))
	if err != nil {
		return false, err
	}
	end, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !sameCachedInfo(opened, end) {
		return false, ErrCorrupt
	}
	readSize := min(opened.Size(), int64(len(want))+1)
	if int64(len(old)) != readSize {
		return false, ErrCorrupt
	}
	verify := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i, name := range names {
			current, e := root.Lstat(name)
			if e != nil {
				return ErrCorrupt
			}
			stable := sameCachedInfo(infos[i], current)
			if i < len(names)-1 {
				// Another writer may add a sibling without replacing this
				// directory. Its identity/type/mode must still match; the
				// immutable leaf retains every strict content-metadata check.
				stable = sameCachedAncestor(infos[i], current)
			}
			if !stable {
				return ErrCorrupt
			}
		}
		return nil
	}
	if err = verify(); err != nil {
		return false, err
	}
	if int64(len(old)) == opened.Size() && bytes.Equal(old, want) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return true, nil
	}
	// Preserve damaged regular-copy repair only after the same descriptor and its
	// named ancestry stay stable. Never remove an observed special file or link.
	if err = verify(); err != nil {
		return false, err
	}
	if err = root.Remove(rel); err != nil {
		return false, err
	}
	return false, nil
}

func sameCachedAncestor(a, b fs.FileInfo) bool {
	return a != nil && b != nil && a.IsDir() && b.IsDir() && os.SameFile(a, b) && a.Mode() == b.Mode()
}

func sameCachedInfo(a, b fs.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func readCachedPrefix(ctx context.Context, r io.Reader, expected int64) ([]byte, error) {
	var data []byte
	buf := make([]byte, 32<<10)
	for int64(len(data)) <= expected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nmax := int64(len(buf))
		if left := expected - int64(len(data)); left < nmax {
			nmax = left + 1
		}
		n, err := r.Read(buf[:int(nmax)])
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		data = append(data, buf[:n]...)
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return data, nil
}
