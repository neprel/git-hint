package store

// Ordinary Open may select a project through a filesystem root alias. Resolve
// that selection once; strict state/source readers still reject aliased handles.
import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
)

const projectConfigMaxBytes int64 = 1 << 20

var errStoreSelection = errors.New("project root selection changed or unsafe")

type storeSelection struct {
	logical, canonical   string
	rootInfo, configInfo os.FileInfo
}

func selectStoreRoot(logical string) (*storeSelection, error) {
	root, e := os.Stat(logical)
	if e != nil || !root.IsDir() {
		return nil, errStoreSelection
	}
	canonical, e := filepath.EvalSymlinks(logical)
	if e != nil {
		return nil, errStoreSelection
	}
	canonicalRoot, e := os.Lstat(canonical)
	if e != nil || !canonicalRoot.IsDir() || !os.SameFile(root, canonicalRoot) || root.Mode() != canonicalRoot.Mode() {
		return nil, errStoreSelection
	}
	selected := &storeSelection{logical: logical, canonical: canonical, rootInfo: root}
	config, e := os.Lstat(selected.originalConfig())
	if e != nil || !config.Mode().IsRegular() || config.Size() < 0 || config.Size() > projectConfigMaxBytes {
		return nil, errStoreSelection
	}
	selected.configInfo = config
	if e = selected.verifyPaths(); e != nil {
		return nil, e
	}
	return selected, nil
}

func (s *storeSelection) originalConfig() string {
	return filepath.Join(s.logical, Dir, "state", "project.json")
}
func (s *storeSelection) canonicalConfig() string {
	return filepath.Join(s.canonical, Dir, "state", "project.json")
}

func sameSelectedConfig(before, after os.FileInfo) bool {
	return before != nil && after != nil && after.Mode().IsRegular() && os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func (s *storeSelection) verifyPaths() error {
	for _, path := range []string{s.logical, s.canonical} {
		var info os.FileInfo
		var e error
		if path == s.canonical {
			info, e = os.Lstat(path)
		} else {
			info, e = os.Stat(path)
		}
		if e != nil || !info.IsDir() || !os.SameFile(s.rootInfo, info) || s.rootInfo.Mode() != info.Mode() {
			return errStoreSelection
		}
	}
	for _, path := range []string{s.originalConfig(), s.canonicalConfig()} {
		info, e := os.Lstat(path)
		if e != nil || !sameSelectedConfig(s.configInfo, info) {
			return errStoreSelection
		}
	}
	return nil
}

func (s *storeSelection) read(ctx context.Context) ([]byte, error) {
	if e := s.verifyPaths(); e != nil {
		return nil, e
	}
	// The original/canonical paths must name the same file. Its bytes are read
	// only through the existing bounded, same-descriptor, no-follow core.
	reader := &Store{Root: s.canonical}
	raw, e := reader.ReadStateBounded(ctx, projectConfigMaxBytes, "state", "project.json")
	if e != nil {
		return nil, errors.Join(errStoreSelection, e)
	}
	if e = s.verifyPaths(); e != nil {
		return nil, e
	}
	return raw, nil
}

func (s *storeSelection) verify(ctx context.Context, want []byte) error {
	raw, e := s.read(ctx)
	if e != nil {
		return e
	}
	if !bytes.Equal(raw, want) {
		return errStoreSelection
	}
	return ctx.Err()
}
