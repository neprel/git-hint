package store

// Bounded store primitives serve exact input/record readers. They establish local
// byte consistency only, never source provenance, capture or prospective authority.
import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrBoundedState = errors.New("bounded store state refused")

type boundedStateSource struct {
	file   *os.File
	verify func() error
}

func stateParts(elem []string) ([]string, error) {
	if len(elem) == 0 || len(elem) > 32 {
		return nil, ErrBoundedState
	}
	parts := []string{Dir}
	for _, v := range elem {
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00") || filepath.IsAbs(v) {
			return nil, ErrBoundedState
		}
		parts = append(parts, v)
	}
	return parts, nil
}

// ReadStateBounded reads one same regular descriptor below the trusted canonical
// Store.Root. Every ancestor and the final target refuse symlinks; reads have a
// caller bound and context. Missing paths return fs.ErrNotExist, never empty data.
func (s *Store) ReadStateBounded(ctx context.Context, limit int64, elem ...string) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, ErrBoundedState
	}
	if limit < 1 {
		return nil, ErrBoundedState
	}
	parts, e := stateParts(elem)
	if e != nil {
		return nil, e
	}
	src, e := openBoundedState(s.Root, parts, false)
	if e != nil {
		return nil, e
	}
	defer src.file.Close()
	fi, e := src.file.Stat()
	if e != nil || !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, ErrBoundedState
	}
	var out []byte
	buf := make([]byte, 32<<10)
	for {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		nmax := int64(len(buf))
		if left := limit - int64(len(out)); left < nmax {
			nmax = left + 1
		}
		n, err := src.file.Read(buf[:int(nmax)])
		if int64(len(out)+n) > limit {
			return nil, ErrBoundedState
		}
		out = append(out, buf[:n]...)
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrBoundedState
		}
		if n == 0 {
			return nil, ErrBoundedState
		}
	}
	if int64(len(out)) != fi.Size() || src.verify() != nil {
		return nil, ErrBoundedState
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return out, nil
}

// ReadStateDirBounded inventories one explicit state directory, not a worktree.
// It bounds entries before allocation and checks that the directory stayed stable.
func (s *Store) ReadStateDirBounded(ctx context.Context, max int, elem ...string) ([]fs.DirEntry, error) {
	if ctx == nil || max < 1 || max == int(^uint(0)>>1) {
		return nil, ErrBoundedState
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	parts, e := stateParts(elem)
	if e != nil {
		return nil, e
	}
	src, e := openBoundedState(s.Root, parts, true)
	if e != nil {
		return nil, e
	}
	defer src.file.Close()
	entries, e := src.file.ReadDir(max + 1)
	if e != nil && e != io.EOF {
		return nil, ErrBoundedState
	}
	if len(entries) > max || src.verify() != nil {
		return nil, ErrBoundedState
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return entries, nil
}

// WriteStateImmutableContext creates a bounded canonical record once, with no
// replacement. Its caller owns validation, replay comparison and the state lock.
func (s *Store) WriteStateImmutableContext(ctx context.Context, data []byte, limit int64, elem ...string) error {
	if ctx == nil || limit < 1 || int64(len(data)) > limit {
		return ErrBoundedState
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	parts, e := stateParts(elem)
	if e != nil {
		return e
	}
	if e = s.CheckPathContext(ctx, s.Path(elem...)); e != nil {
		return e
	}
	return writeBoundedState(ctx, s.Root, parts, data)
}

// StateRootIdentity identifies this observed canonical root node, not a trusted origin.
func (s *Store) StateRootIdentity(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", ErrBoundedState
	}
	if e := ctx.Err(); e != nil {
		return "", e
	}
	return boundedRootIdentity(s.Root)
}

// EnsureStateDirContext creates only the named protected state directory. The
// caller owns serialization; this is not a source/retention authority handle.
func (s *Store) EnsureStateDirContext(ctx context.Context, elem ...string) error {
	if ctx == nil {
		return ErrBoundedState
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	parts, e := stateParts(elem)
	if e != nil {
		return e
	}
	if e = s.CheckPathContext(ctx, s.Path(append(elem, ".guard")...)); e != nil {
		return e
	}
	return ensureBoundedStateDir(ctx, s.Root, parts)
}
