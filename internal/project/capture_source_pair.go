package project

// This is the ONE existing two-pass capture/retained-object core. Callers own
// their original context, invocation, state lock and retention barrier.
import (
	"context"
	"errors"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/readgit"
)

type capturedSourcePair struct {
	selected trees.SelectedReceipt
	source   trees.SourceViewReceipt
	objects  map[string][]byte
	receipts []DefinitionObject
}

func captureSourcePair(ctx context.Context, l *library.Library, root string, limits trees.Limits, invocation *readgit.SourceInvocation, phase func(string, func() error) error) (result *capturedSourcePair, err error) {
	var e error
	var view *trees.SelectedSourceView
	defer func() {
		if view != nil {
			if e := view.Close(); e != nil {
				err = errors.Join(err, ErrCaptureSource)
			}
		}
	}()
	if e = phase("view_open", func() error {
		var e error
		view, e = trees.OpenSelectedSourceView(ctx, l, root, trees.SourceViewOptions{Limits: limits})
		return e
	}); e != nil {
		return nil, captureProblem(ctx, "view_open")
	}
	var a, b *trees.SelectedSourceViewCapture
	if e = phase("capture_a", func() error { var e error; a, e = view.Capture(ctx); return e }); e != nil {
		return nil, captureProblem(ctx, "capture_a")
	}
	if e = phase("capture_b", func() error { var e error; b, e = view.Capture(ctx); return e }); e != nil {
		return nil, captureProblem(ctx, "capture_b")
	}
	if e = phase("view_close", func() error { e := view.Close(); view = nil; return e }); e != nil {
		return nil, captureProblem(ctx, "view_close")
	}
	objects := map[string][]byte{}
	receipts := []DefinitionObject{}
	if e = phase("objects_and_receipts", func() error {
		x, y := a.Sources(), b.Sources()
		if x.InvocationIdentity != invocation.Identity() || y.InvocationIdentity != invocation.Identity() {
			return ErrCaptureSource
		}
		wire, e := trees.EncodeSelectedSourcePair(limits, a.Selected().Receipt(), b.Selected().Receipt(), x, y)
		if e != nil {
			return e
		}
		pair, e := trees.DecodeSelectedSourcePair(limits, wire)
		if e != nil {
			return e
		}
		var used int64
		for _, ref := range pair.ObjectRefs() {
			cap := min(ref.MaxBytes, (128<<20)-used)
			if cap < 1 {
				return ErrCaptureSource
			}
			raw, e := l.ResolveBoundedRead(ctx, "object", ref.Digest, cap)
			if e != nil {
				return e
			}
			used += int64(len(raw))
			objects[ref.Digest] = raw
		}
		if _, e = pair.ValidateObjects(objects); e != nil {
			return e
		}
		for _, raw := range [][]byte{wire.SelectedA, wire.SelectedB, wire.SourceA, wire.SourceB} {
			d, e := l.PutObjectContext(ctx, raw)
			if e != nil {
				return e
			}
			receipts = append(receipts, DefinitionObject{d, int64(len(raw))})
			objects[d] = raw
		}
		return nil
	}); e != nil {
		return nil, e
	}
	return &capturedSourcePair{a.Selected().Receipt(), a.Sources(), objects, receipts}, nil
}
