// Package safefs writes untrusted file trees (archives, stored repository trees) into a
// directory without leaving it.
//
// Writes never pass through a symbolic link: every parent component must be a real
// directory, and files are created exclusively. Links are created as given only when
// their lexical target stays inside; Check then resolves every link inside the
// directory, so a chain of links that together leave it (d -> ., x -> d/..) is refused.
package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrUnsafe marks an entry that would leave the directory or pass through a link.
var ErrUnsafe = errors.New("unsafe path")

// Dir is an open target directory.
type Dir struct {
	root *os.Root
	path string
}

// Open opens dir (created when missing) as the target of later writes.
func Open(dir string) (*Dir, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Dir{root: r, path: dir}, nil
}

// Close releases the directory.
func (d *Dir) Close() error { return d.root.Close() }

// Clean checks a slash-separated relative name and returns it cleaned.
func Clean(name string) (string, error) {
	if name == "" || strings.Contains(name, "\x00") || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", fmt.Errorf("%w: %q", ErrUnsafe, name)
	}
	c := path.Clean(name)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: %q", ErrUnsafe, name)
	}
	return c, nil
}

// mkdirs creates the parent directories of name; every existing component must be a
// real directory.
func (d *Dir) mkdirs(name string) error {
	parts := strings.Split(name, "/")
	cur := ""
	for _, p := range parts[:len(parts)-1] {
		cur = path.Join(cur, p)
		fi, err := d.root.Lstat(filepath.FromSlash(cur))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := d.root.Mkdir(filepath.FromSlash(cur), 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%w: %q passes through a link or file", ErrUnsafe, name)
		}
	}
	return nil
}

// Mkdir creates directory name and its parents.
func (d *Dir) Mkdir(name string) error {
	c, err := Clean(name)
	if err != nil {
		return err
	}
	return d.mkdirs(c + "/x")
}

// WriteFile creates file name with data; an existing entry of that name is refused.
func (d *Dir) WriteFile(name string, data []byte, perm fs.FileMode) error {
	c, err := Clean(name)
	if err != nil {
		return err
	}
	if err := d.mkdirs(c); err != nil {
		return err
	}
	f, err := d.root.OpenFile(filepath.FromSlash(c), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm&0o777)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafe, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Symlink creates link name with target. An absolute target or one that leaves the
// directory lexically is refused.
func (d *Dir) Symlink(name, target string) error {
	c, err := Clean(name)
	if err != nil {
		return err
	}
	if target == "" || path.IsAbs(target) || strings.Contains(target, "\\") || strings.Contains(target, "\x00") {
		return fmt.Errorf("%w: link %q", ErrUnsafe, name)
	}
	if r := path.Clean(path.Join(path.Dir(c), target)); r == ".." || strings.HasPrefix(r, "../") {
		return fmt.Errorf("%w: link %q", ErrUnsafe, name)
	}
	if err := d.mkdirs(c); err != nil {
		return err
	}
	return d.root.Symlink(filepath.FromSlash(target), filepath.FromSlash(c))
}

// Check resolves every link in the directory. A link that leaves the directory or loops
// is an error; a link to a missing name inside is allowed.
func (d *Dir) Check() error {
	return filepath.WalkDir(d.path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(d.path, p)
		if _, err := d.root.Stat(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: link %q: %v", ErrUnsafe, filepath.ToSlash(rel), err)
		}
		return nil
	})
}
