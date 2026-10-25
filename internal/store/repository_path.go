package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrRepositoryPath = errors.New("path is not a usable repository target")

// RepositoryPathResult describes one local path resolution, not captured source
// or observed work. Absent paths are declarations only; Mode is zero for them.
type RepositoryPathResult struct {
	Resolved string
	Relative string
	Present  bool
	Mode     fs.FileMode
}

// RepositoryPath resolves root aliases through the existing path resolver. A
// present final leaf must not be a symlink and must name the same physical file
// through its original and resolved paths. Missing suffixes remain usable for
// event declarations, but never establish that a file exists. Callers that read
// content must require Present and the required file type. This is no capture or
// atomic read guarantee.
func RepositoryPath(root, path string) (RepositoryPathResult, error) {
	refuse := func() (RepositoryPathResult, error) { return RepositoryPathResult{}, ErrRepositoryPath }
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || strings.EqualFold(part, Dir) || strings.EqualFold(part, ".git") {
			return refuse()
		}
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return refuse()
	}
	rootAbs, e := filepath.Abs(root)
	if e != nil {
		return refuse()
	}
	resolvedRoot, e := resolveExistingPrefix(rootAbs)
	if e != nil {
		return refuse()
	}
	rootInfo, e := os.Stat(rootAbs)
	if e != nil || !rootInfo.IsDir() {
		return refuse()
	}
	canonicalRoot, e := os.Lstat(resolvedRoot)
	if e != nil || !canonicalRoot.IsDir() || !os.SameFile(rootInfo, canonicalRoot) {
		return refuse()
	}
	leaf, e := os.Lstat(abs)
	present := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return refuse()
	}
	if present && leaf.Mode()&os.ModeSymlink != 0 {
		return refuse()
	}
	resolved, e := resolveExistingPrefix(abs)
	if e != nil {
		return refuse()
	}
	rel, e := filepath.Rel(resolvedRoot, resolved)
	if e != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return refuse()
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(part, Dir) || strings.EqualFold(part, ".git") {
			return refuse()
		}
	}
	result := RepositoryPathResult{Resolved: resolved, Relative: filepath.ToSlash(rel), Present: present}
	if present {
		canonical, e := os.Lstat(resolved)
		if e != nil || canonical.Mode()&os.ModeSymlink != 0 || !os.SameFile(leaf, canonical) || leaf.Mode() != canonical.Mode() {
			return refuse()
		}
		result.Mode = leaf.Mode()
	} else if _, e = os.Lstat(resolved); !errors.Is(e, os.ErrNotExist) {
		return refuse()
	}
	return result, nil
}
