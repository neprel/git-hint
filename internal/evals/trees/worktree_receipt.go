package trees

// This opt-in seam records only observations of the existing selected-file core.
// It supplies no config/ignore resolution, pre-work, capture-policy or authority proof.
import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/hint"
)

const SelectedReceiptV1 = "git-selected-reader-receipt-v1"

type SourceSessionOptions struct{ Limits Limits }
type SelectedPathState string

const (
	SelectedRegularPresent SelectedPathState = "regular_present"
	SelectedAbsent         SelectedPathState = "selected_absent"
)

// Raw paths and local node identities stay local. Indexed means observed in the
// SAME capture's --stage output; it is not inferred from the final tree.
type SelectedPathFact struct {
	Path                  string
	State                 SelectedPathState
	Mode                  string
	ObjectDigest          string
	Size                  int64
	Indexed               bool
	PathIdentity          string
	AbsenceParentIdentity string
}
type IndexStageFact struct {
	Path, Mode, ObjectID string
	Stage                uint8
}

// SelectedReceipt describes one pass, not an atomic/repeated view or config source proof.
// RootIdentity records the root node opened for this session; it is not a full root
// directory census. Unix identities omit uid/gid. Git environment remains inherited.
// Stage tuples have a separate fixed 10,000 bound; effective Limits bound selected paths.
type SelectedReceipt struct {
	Version                                                   string
	Pass                                                      uint8
	RootIdentity                                              string
	Selected                                                  []SelectedPathFact
	Stages                                                    []IndexStageFact
	PresentCount, IndexedAbsentCount, UnindexedAbsentCount    uint32
	SelectedInventoryDigest, StageInventoryDigest, TreeDigest string
	BeginOffsetNS, EndOffsetNS                                int64
	EffectiveLimits                                           Limits
}

// SelectedCapture owns a private immutable receipt. Accessors return no aliases.
type SelectedCapture struct{ receipt SelectedReceipt }

func (c *SelectedCapture) TreeDigest() string {
	if c == nil {
		return ""
	}
	return c.receipt.TreeDigest
}
func (c *SelectedCapture) Receipt() SelectedReceipt {
	if c == nil {
		return SelectedReceipt{}
	}
	r := c.receipt
	r.Selected = append([]SelectedPathFact{}, r.Selected...)
	r.Stages = append([]IndexStageFact{}, r.Stages...)
	return r
}

type SelectedSourceSession struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	source         *worktreeRoot
	library        *library.Library
	limits         Limits
	anchor         time.Time
	attempts       uint8
	closed, failed bool
	// Unexported scheduling seams exercise this same production capture core.
	hooks receiptCapture
}

// OpenSelectedSourceSession opens the existing strict source root once, under one
// original 30-second bound. Content maxima apply per pass (128 MiB across two default
// passes). No partial receipt returns after a failed pass; stored orphan objects may remain.
func OpenSelectedSourceSession(parent context.Context, l *library.Library, root string, opts SourceSessionOptions) (*SelectedSourceSession, error) {
	if parent == nil || l == nil {
		return nil, receiptProblem("receipt_request_invalid")
	}
	anchor := time.Now()
	ctx, cancel := context.WithDeadline(parent, anchor.Add(strictCaptureSeconds))
	if e := ctx.Err(); e != nil {
		cancel()
		return nil, e
	}
	limits, e := strictEffectiveLimits(opts.Limits)
	if e != nil {
		cancel()
		return nil, e
	}
	source, e := openWorktreeRoot(root, true)
	if e != nil {
		cancel()
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		source.close()
		cancel()
		return nil, e
	}
	return &SelectedSourceSession{ctx: ctx, cancel: cancel, source: source, library: l, limits: limits, anchor: anchor}, nil
}

// Capture always derives from the original session context. Caller cancellation
// is an additional tightening bound; Go context ancestry is neither inspected nor assumed.
func (s *SelectedSourceSession) Capture(caller context.Context) (*SelectedCapture, error) {
	if s == nil || s.cancel == nil || caller == nil {
		return nil, receiptProblem("receipt_request_invalid")
	}
	if !s.mu.TryLock() {
		s.cancel()
		return nil, receiptProblem("receipt_session_busy")
	}
	defer s.mu.Unlock()
	if s.closed || s.failed || s.attempts >= 2 {
		return nil, receiptProblem("receipt_session_closed")
	}
	s.attempts++
	ctx, cancel := context.WithCancel(s.ctx)
	if deadline, ok := caller.Deadline(); ok {
		if original, _ := s.ctx.Deadline(); deadline.Before(original) {
			cancel()
			ctx, cancel = context.WithDeadline(s.ctx, deadline)
		}
	}
	defer cancel()
	stop := context.AfterFunc(caller, cancel)
	defer stop()
	fail := func(e error) (*SelectedCapture, error) { s.failed = true; s.cancel(); return nil, e }
	if e := caller.Err(); e != nil {
		return fail(e)
	}
	if e := ctx.Err(); e != nil {
		return fail(e)
	}
	r := s.hooks
	r.value = SelectedReceipt{Version: SelectedReceiptV1, Pass: s.attempts, RootIdentity: s.source.receiptRootIdentity(), Selected: []SelectedPathFact{}, Stages: []IndexStageFact{}, EffectiveLimits: s.limits, BeginOffsetNS: time.Since(s.anchor).Nanoseconds()}
	r.indexed = map[string]bool{}
	digest, e := captureWorktreeSource(ctx, s.library, s.source, s.limits, true, &r)
	if e != nil {
		return fail(e)
	}
	if e = ctx.Err(); e != nil {
		return fail(e)
	}
	if e = caller.Err(); e != nil {
		return fail(e)
	}
	if e = s.source.verify(); e != nil {
		return fail(e)
	}
	r.value.TreeDigest = digest
	sort.Slice(r.value.Selected, func(i, j int) bool { return r.value.Selected[i].Path < r.value.Selected[j].Path })
	facts := make([]selectedContentFact, len(r.value.Selected))
	for i, v := range r.value.Selected {
		facts[i] = selectedContentFact{v.Path, v.State, v.Mode, v.ObjectDigest, v.Size, v.Indexed}
	}
	r.value.SelectedInventoryDigest = receiptDigest(struct {
		Domain string
		Facts  []selectedContentFact
	}{"git-selected-inventory-v1", facts})
	r.value.StageInventoryDigest = receiptDigest(struct {
		Domain string
		Facts  []IndexStageFact
	}{"git-selected-stage-inventory-v1", r.value.Stages})
	if e = ctx.Err(); e != nil {
		return fail(e)
	}
	if e = caller.Err(); e != nil {
		return fail(e)
	}
	r.value.EndOffsetNS = time.Since(s.anchor).Nanoseconds()
	if r.value.EndOffsetNS > int64(strictCaptureSeconds) {
		return fail(context.DeadlineExceeded)
	}
	return &SelectedCapture{receipt: r.value}, nil
}
func (s *SelectedSourceSession) Close() error {
	if s == nil || s.cancel == nil {
		return nil
	}
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if e := s.source.closeErr(); e != nil {
			return receiptProblem("receipt_close_failed")
		}
	}
	return nil
}
func receiptProblem(code string) error {
	return unsupported(code, "the selected-source observation is unavailable")
}
func receiptDigest(v any) string {
	d, e := hint.Digest(v)
	if e != nil {
		return ""
	}
	return d
}

type selectedContentFact struct {
	Path               string
	State              SelectedPathState
	Mode, ObjectDigest string
	Size               int64
	Indexed            bool
}
type captureSourceObserver interface {
	beforeInventory(context.Context) error
	afterInventory(context.Context, []string, []IndexStageFact) error
	afterContent(context.Context, SelectedReceipt) error
}
type receiptCapture struct {
	observer       captureSourceObserver
	value          SelectedReceipt
	indexed        map[string]bool
	afterInventory func()
	readHooks      readHooks
}

// Bound record count/framing before the legacy core allocates its split inventory.
func (r *receiptCapture) selection(raw []byte, limit int) error {
	count := 0
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, 0)
		if end <= 0 {
			return receiptProblem("receipt_selection_invalid")
		}
		count++
		if count > limit {
			return receiptProblem("receipt_selection_invalid")
		}
		raw = raw[end+1:]
	}
	return nil
}
func (r *receiptCapture) stages(raw []byte) error {
	seen := map[string]bool{}
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, 0)
		if end <= 0 {
			return receiptProblem("receipt_stage_invalid")
		}
		line := string(raw[:end])
		raw = raw[end+1:]
		if len(r.value.Stages) >= 10000 || !validStageRecord([]byte(line)) {
			return receiptProblem("receipt_stage_invalid")
		}
		head, p, _ := strings.Cut(line, "\t")
		if !utf8.ValidString(p) {
			return receiptProblem("receipt_path_invalid")
		}
		fields := strings.Fields(head)
		stage, _ := strconv.Atoi(fields[2])
		key := p + "\x00" + fields[2]
		if seen[key] {
			return receiptProblem("receipt_stage_invalid")
		}
		seen[key] = true
		r.indexed[p] = true
		r.value.Stages = append(r.value.Stages, IndexStageFact{p, fields[0], fields[1], uint8(stage)})
	}
	sort.Slice(r.value.Stages, func(i, j int) bool {
		a, b := r.value.Stages[i], r.value.Stages[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Stage < b.Stage
	})
	return nil
}
func (r *receiptCapture) absent(p, id string) {
	r.value.Selected = append(r.value.Selected, SelectedPathFact{Path: p, State: SelectedAbsent, Indexed: r.indexed[p], AbsenceParentIdentity: id})
	if r.indexed[p] {
		r.value.IndexedAbsentCount++
	} else {
		r.value.UnindexedAbsentCount++
	}
}
func (r *receiptCapture) present(p string, e worktreeEntry, digest string) {
	r.value.Selected = append(r.value.Selected, SelectedPathFact{Path: p, State: SelectedRegularPresent, Mode: e.mode, ObjectDigest: digest, Size: int64(len(e.data)), Indexed: r.indexed[p], PathIdentity: e.pathIdentity})
	r.value.PresentCount++
}

// No source/config resolver or public serialization is implied by these local values.
