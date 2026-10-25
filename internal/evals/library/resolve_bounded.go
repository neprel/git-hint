package library

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// ResolveBoundedRead shares the store's descriptor-rooted bounded reader. It
// resolves exact eval/object bytes only, never source or execution authority.
func (l *Library) ResolveBoundedRead(ctx context.Context, kind, id string, limit int64) ([]byte, error) {
	var p string
	switch kind {
	case "eval":
		if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
			return nil, errors.New("resolve_record_name")
		}
		p = l.path("manifests", "eval", id+".json")
	case "object":
		var e error
		p, e = l.objectPath(id)
		if e != nil {
			return nil, errors.New("resolve_object_id")
		}
	default:
		return nil, errors.New("resolve_read_kind")
	}
	rel, e := filepath.Rel(l.s.Path(), p)
	if e != nil {
		return nil, errors.New("resolve_read_path")
	}
	b, e := l.s.ReadStateBounded(ctx, limit, strings.Split(rel, string(filepath.Separator))...)
	if e != nil {
		return nil, e
	}
	if kind == "object" && Digest(b) != id {
		return nil, ErrCorrupt
	}
	return b, nil
}
