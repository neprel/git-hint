package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// hint_context (Stage 11 D05, C11-A decision 1): a bounded neighborhood of one entity of the
// local index, and the expansion of a detail by an immutable reference. It reuses the stored
// snapshot (declarations, imports, test flags); it reads source only to answer and stores
// nothing. A reference names one snapshot and one entity. Source is returned only when the
// file still has the content hash that the snapshot recorded, so an old reference reads the
// same content as its snapshot or is refused as stale; it never returns other source.

const (
	ctxMaxFileBytes = 4 << 20 // a larger file gives no source
	ctxMaxHashBytes = 8 << 20 // freshness checks read at most this much
	ctxRefPrefix    = "hc1."
	ctxCoverage     = "Go declarations, imports and test files of the snapshot (syntax level, no call graph); source only when the file hash equals the snapshot"
)

var ctxRefPattern = regexp.MustCompile(`^hc1\.[0-9a-f]{32}\.[0-9a-f]{32}$`)

type ContextIn struct {
	Seed        string `json:"seed,omitempty" jsonschema:"start entity: a declaration name (Rate, tax.Rate, Cart.Total), a file path or a package directory; not with ref"`
	Ref         string `json:"ref,omitempty" jsonschema:"a ref from an earlier hint_context answer to expand; not with seed"`
	Repo        string `json:"repository,omitempty" jsonschema:"local repository name; default all"`
	Depth       int    `json:"depth,omitempty" jsonschema:"graph hops from the start (1 or 2; default 1)"`
	MaxEntities int    `json:"max_entities,omitempty" jsonschema:"at most this many items (1-100; default 20)"`
	MaxBytes    int    `json:"max_bytes,omitempty" jsonschema:"at most this many UTF-8 bytes of the whole answer (4096-65536; default 16384)"`
	MaxMillis   int    `json:"max_ms,omitempty" jsonschema:"time limit in milliseconds (100-5000; default 1000)"`
}

type ContextItem struct {
	Ref           string `json:"ref"`
	Relation      string `json:"relation" jsonschema:"definition, candidate, start, same_file, defined_in, declares, imports, imported_by, contains, test_file or test (name match)"`
	Hops          int    `json:"hops"`
	Kind          string `json:"kind"`
	Repo          string `json:"repository"`
	Path          string `json:"path"`
	Line          int    `json:"line,omitempty"`
	Lines         int    `json:"lines,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Source        string `json:"source,omitempty"`
	SourceState   string `json:"source_state" jsonschema:"returned, truncated (budget; expand the ref), omitted_budget, stale (the file changed after the snapshot), not_text, not_returned (expand the ref to read it)"`
}

type ContextSnapshot struct {
	SnapshotRef
	Digest string `json:"digest" jsonschema:"index identity: the digest of the stored snapshot"`
	Latest bool   `json:"latest" jsonschema:"whether it is the last collected snapshot"`
}

type ContextLimits struct {
	Depth     int `json:"depth"`
	Entities  int `json:"max_entities"`
	Bytes     int `json:"max_bytes"`
	MaxMillis int `json:"max_ms"`
}

type ContextUsed struct {
	Entities  int `json:"entities"`
	Bytes     int `json:"bytes" jsonschema:"UTF-8 bytes of the whole answer as returned: the structured result and the text summary, before the transport envelope"`
	ItemBytes int `json:"item_bytes" jsonschema:"UTF-8 bytes of the items alone (part of bytes)"`
	ElapsedMS int `json:"elapsed_ms"`
}

type ContextOmitted struct {
	Entities    int      `json:"entities" jsonschema:"items left out by the limits"`
	SourceBytes int      `json:"source_bytes" jsonschema:"source bytes left out by the byte limit"`
	Reasons     []string `json:"reasons" jsonschema:"entities, bytes, time, depth"`
}

// ContextMeasurement is the HINT v0.2 context measurement of this answer (Observation.context
// with opaque IDs only), for an adapter that records the call. Tokens are an estimate.
type ContextMeasurement struct {
	Action      string         `json:"action"`
	Stage       string         `json:"stage"`
	ArtifactID  string         `json:"artifact_id,omitempty"`
	RefID       string         `json:"ref_id,omitempty"`
	SnapshotID  string         `json:"snapshot_id,omitempty"`
	PolicyState string         `json:"policy_state"`
	PolicyRef   string         `json:"policy_ref,omitempty"`
	Freshness   string         `json:"freshness"`
	Result      string         `json:"result"`
	Bytes       int            `json:"bytes"`
	Omitted     int            `json:"omitted"`
	Tokens      map[string]any `json:"tokens"`
}

type ContextOut struct {
	SideEffect  string             `json:"side_effect"`
	Action      string             `json:"action" jsonschema:"deliver (seed) or expand (ref)"`
	Result      string             `json:"result" jsonschema:"delivered, ambiguous (candidates only; call again with a candidate ref), refused_stale, missing"`
	Freshness   string             `json:"freshness" jsonschema:"current, stale or unknown, compared with the work tree"`
	Snapshot    *ContextSnapshot   `json:"snapshot,omitempty"`
	Items       []ContextItem      `json:"items"`
	Omitted     ContextOmitted     `json:"omitted"`
	Limits      ContextLimits      `json:"limits"`
	Used        ContextUsed        `json:"used"`
	ArtifactID  string             `json:"artifact_id,omitempty" jsonschema:"opaque ID of this exact answer (snapshot, items and source)"`
	Measurement ContextMeasurement `json:"measurement"`
	Coverage    string             `json:"coverage"`

	policyRef string // project reference of the effective context policy identity, if configured
	startFile string // the file of the start or expanded entity
}

func (h *handlers) registerContext(s *mcp.Server, ro *mcp.ToolAnnotations) {
	add(s, &mcp.Tool{Name: "hint_context", Annotations: ro,
		Description: "Side effect: none (read-only, local index; stores nothing). A small context around one entity: the definition with its source, the other declarations of its file, imports, importers and tests, as items with refs. Call again with ref to expand one item. Limits: depth, items, bytes, time. A ref reads its own snapshot; when the file changed since then, the answer is refused_stale (call hint_collect)."}, h.contextTool)
}

// limits returns the effective limits of a call: its arguments, else the active local
// context policy, else the defaults.
func (o *ContextIn) limits(pol *store.ContextLimits) (ContextLimits, bool) {
	l := ContextLimits{Depth: o.Depth, Entities: o.MaxEntities, Bytes: o.MaxBytes, MaxMillis: o.MaxMillis}
	def := store.ContextLimits{Depth: store.ContextRanges["depth"][0], MaxEntities: store.ContextRanges["max_entities"][0],
		MaxBytes: store.ContextRanges["max_bytes"][0], MaxMillis: store.ContextRanges["max_ms"][0]}
	if pol != nil {
		def = *pol
	}
	set := func(v *int, d int, key string) bool {
		if *v == 0 {
			*v = d
		}
		r := store.ContextRanges[key]
		return *v >= r[1] && *v <= r[2]
	}
	ok := set(&l.Depth, def.Depth, "depth") && set(&l.Entities, def.MaxEntities, "max_entities") &&
		set(&l.Bytes, def.MaxBytes, "max_bytes") && set(&l.MaxMillis, def.MaxMillis, "max_ms")
	return l, ok
}

// ctxSnap is one loaded snapshot with the views that the neighborhood needs.
type ctxSnap struct {
	repo     store.Repo
	l        *snapshot.Local
	latest   bool
	ents     map[string]*snapshot.EntityRecord
	children map[string][]string // parent -> entities (contains)
	imports  map[string][]string // file -> package directories
	importer map[string][]string // package directory -> files
}

func newCtxSnap(r store.Repo, l *snapshot.Local, latest bool) *ctxSnap {
	c := &ctxSnap{repo: r, l: l, latest: latest, ents: map[string]*snapshot.EntityRecord{}, children: map[string][]string{},
		imports: map[string][]string{}, importer: map[string][]string{}}
	for i := range l.Entities {
		e := &l.Entities[i]
		c.ents[e.ID] = e
		if e.ParentID != nil {
			c.children[*e.ParentID] = append(c.children[*e.ParentID], e.ID)
		}
	}
	for _, r := range l.Relations {
		if r.Kind == "imports" {
			c.imports[r.Source] = append(c.imports[r.Source], r.Target)
			c.importer[r.Target] = append(c.importer[r.Target], r.Source)
		}
	}
	for _, m := range []map[string][]string{c.children, c.imports, c.importer} {
		for k := range m {
			sort.Slice(m[k], func(i, j int) bool { return c.order(m[k][i]) < c.order(m[k][j]) })
		}
	}
	return c
}

// order sorts entities by path and line (deterministic, independent of the stored order).
func (c *ctxSnap) order(id string) string {
	ix := c.l.Index[id]
	return fmt.Sprintf("%s\x00%09d\x00%s\x00%s", ix.Path, ix.Line, ix.Name, id)
}

func (c *ctxSnap) lines(id string) int {
	if q, ok := c.ents[id].Features["lines"]; ok && q.Value != nil {
		return int(*q.Value)
	}
	return 0
}

func (c *ctxSnap) isTest(id string) bool {
	for _, f := range c.ents[id].Flags {
		if f == "test" {
			return true
		}
	}
	return false
}

func (c *ctxSnap) parent(id string) string {
	if p := c.ents[id].ParentID; p != nil {
		return *p
	}
	return ""
}

type edge struct{ id, rel string }

// neighbors are the graph links of one entity, in a fixed order.
func (c *ctxSnap) neighbors(id string) []edge {
	e := c.ents[id]
	var out []edge
	switch e.Kind {
	case "file":
		for _, d := range c.children[id] {
			out = append(out, edge{d, "declares"})
		}
		for _, d := range c.imports[id] {
			out = append(out, edge{d, "imports"})
		}
		if !c.isTest(id) {
			for _, f := range c.children[c.parent(id)] {
				if c.ents[f].Kind == "file" && c.isTest(f) {
					out = append(out, edge{f, "test_file"})
				}
			}
		}
	case "directory":
		for _, f := range c.importer[id] {
			out = append(out, edge{f, "imported_by"})
		}
		for _, f := range c.children[id] {
			if c.ents[f].Kind == "file" {
				out = append(out, edge{f, "contains"})
			}
		}
	case "repository":
		for _, f := range c.children[id] {
			out = append(out, edge{f, "contains"})
		}
	default: // a declaration: the links of its file come first
		file := c.parent(id)
		for _, d := range c.children[file] {
			if d != id {
				out = append(out, edge{d, "same_file"})
			}
		}
		out = append(out, edge{file, "defined_in"})
		name := c.l.Index[id].Name
		short := name[strings.LastIndexAny(name, "./")+1:]
		dir := c.parent(file)
		for _, f := range c.children[dir] {
			if c.ents[f].Kind != "file" || !c.isTest(f) {
				continue
			}
			for _, d := range c.children[f] {
				dn := c.l.Index[d].Name
				if strings.Contains(dn[strings.LastIndexAny(dn, "./")+1:], short) {
					out = append(out, edge{d, "test"})
				}
			}
		}
		for _, d := range c.imports[file] {
			out = append(out, edge{d, "imports"})
		}
		for _, f := range c.importer[dir] {
			out = append(out, edge{f, "imported_by"})
		}
	}
	return out
}

func ctxRef(snapID, entID string) string { return ctxRefPrefix + snapID + "." + entID }

// fileState is a current file read under the repository root: the content only when its
// hash equals the snapshot's hash; state says why there is no content.
type fileState struct {
	b     []byte
	state string // returned, stale, not_text, unknown
}

type sourceReader struct {
	s     *store.Store
	cache map[string]fileState
	read  int
}

func (r *sourceReader) file(c *ctxSnap, fileID string) fileState {
	key := c.repo.Name + "\x00" + fileID + "\x00" + c.l.Index[fileID].Hash
	if st, ok := r.cache[key]; ok {
		return st
	}
	st := r.load(c, fileID)
	r.cache[key] = st
	return st
}

func (r *sourceReader) load(c *ctxSnap, fileID string) fileState {
	ix := c.l.Index[fileID]
	if ix.Hash == "" || ix.Path == "" {
		return fileState{state: "not_text"}
	}
	if r.read > ctxMaxHashBytes {
		return fileState{state: "unknown"}
	}
	root, err := os.OpenRoot(r.s.RepoRoot(c.repo))
	if err != nil {
		return fileState{state: "unknown"}
	}
	defer root.Close()
	p := path.Clean(ix.Path)
	fi, err := root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{state: "stale"} // the snapshot's file is gone
	}
	if err != nil || !fi.Mode().IsRegular() {
		return fileState{state: "stale"}
	}
	if fi.Size() > ctxMaxFileBytes {
		return fileState{state: "not_text"}
	}
	f, err := root.Open(p)
	if err != nil {
		return fileState{state: "stale"}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, ctxMaxFileBytes+1))
	if err != nil {
		return fileState{state: "stale"}
	}
	r.read += len(b)
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != ix.Hash {
		return fileState{state: "stale"}
	}
	if c.ents[fileID].Language == nil {
		return fileState{state: "not_text"}
	}
	return fileState{b: b, state: "returned"}
}

// excerpt returns lines [line, line+n) of b.
func excerpt(b []byte, line, n int) string {
	if line < 1 || n < 1 {
		return ""
	}
	ls := strings.SplitAfter(string(b), "\n")
	if line > len(ls) {
		return ""
	}
	end := min(line-1+n, len(ls))
	return strings.Join(ls[line-1:end], "")
}

func (h *handlers) contextTool(ctx context.Context, _ *mcp.CallToolRequest, in ContextIn) (*mcp.CallToolResult, ContextOut, error) {
	if (in.Seed == "") == (in.Ref == "") || len(in.Repo) > 200 || (in.Ref != "" && !ctxRefPattern.MatchString(in.Ref)) ||
		(in.Seed != "" && !validQuery(in.Seed)) {
		return nil, ContextOut{}, h.fail("hint_context", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, ContextOut{}, h.fail("hint_context", err)
	}
	pol := s.Cfg.Context.Active()
	lim, ok := in.limits(pol)
	if !ok {
		return nil, ContextOut{}, h.fail("hint_context", toolErr("invalid_argument"))
	}
	now := h.o.Now
	start := now()
	deadline := start.Add(time.Duration(lim.MaxMillis) * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	out := ContextOut{SideEffect: effectNone, Action: "deliver", Items: []ContextItem{}, Limits: lim, Coverage: ctxCoverage,
		Omitted: ContextOmitted{Reasons: []string{}}}
	if pol != nil { // the identity of the effective limits, call arguments included
		out.policyRef = store.ContextPolicyRef(s.Key(), store.ContextLimits{Depth: lim.Depth, MaxEntities: lim.Entities, MaxBytes: lim.Bytes, MaxMillis: lim.MaxMillis})
	}
	var snap *ctxSnap
	var seed string
	if in.Ref != "" {
		out.Action = "expand"
		parts := strings.Split(in.Ref, ".")
		snap, err = h.refSnapshot(s, in.Repo, parts[1])
		if err != nil {
			return nil, ContextOut{}, h.fail("hint_context", err)
		}
		if snap == nil || snap.ents[parts[2]] == nil {
			out.Result, out.Freshness = "missing", "unknown"
			return h.ctxFinish(s, &out, in, start, "The ref names no stored snapshot entity.")
		}
		seed = parts[2]
	} else {
		snaps, _, err := h.latest(s, in.Repo)
		if err != nil {
			return nil, ContextOut{}, h.fail("hint_context", err)
		}
		type hit struct {
			c  *ctxSnap
			id string
		}
		var hits []hit
		for _, rs := range snaps {
			var r store.Repo
			for _, x := range s.Cfg.Repos {
				if x.Name == rs.name {
					r = x
				}
			}
			c := newCtxSnap(r, rs.l, true)
			if p, ok := cleanRel(in.Seed); ok { // a file or package directory
				if id := hint.FileID(s.Key(), rs.id, p); c.ents[id] != nil {
					hits = append(hits, hit{c, id})
				}
			}
			for _, e := range rs.l.Entities {
				if n := rs.l.Index[e.ID].Name; n != "" && nameMatches(n, in.Seed) {
					hits = append(hits, hit{c, e.ID})
				}
			}
		}
		if len(hits) == 0 {
			out.Result, out.Freshness = "missing", "unknown"
			return h.ctxFinish(s, &out, in, start, "No entity matches the seed in the last snapshot.")
		}
		sort.Slice(hits, func(i, j int) bool {
			return hits[i].c.repo.Name+"\x00"+hits[i].c.order(hits[i].id) < hits[j].c.repo.Name+"\x00"+hits[j].c.order(hits[j].id)
		})
		if len(hits) > 1 {
			out.Result, out.Freshness = "ambiguous", "unknown"
			one := true
			for _, x := range hits {
				one = one && x.c == hits[0].c
			}
			if one { // the candidates of one snapshot name it; across repositories there is no single one
				c := hits[0].c
				out.Snapshot = &ContextSnapshot{SnapshotRef: SnapshotRef{Repo: c.repo.Name, SnapshotID: c.l.Record.SnapshotID, CapturedAt: c.l.Record.CapturedAt},
					Digest: c.l.Record.Digest, Latest: c.latest}
			}
			add := newCtxAdder(&out, lim, now, deadline)
			for i, x := range hits {
				if add.late() {
					add.omit("time", len(hits)-i)
					break
				}
				if !add.item(x.c.item(x.id, "candidate", 0)) {
					out.Omitted.Entities += len(hits) - i
					break
				}
			}
			return h.ctxFinish(s, &out, in, start, "%d entities match; call again with one candidate ref.", len(hits))
		}
		snap, seed = hits[0].c, hits[0].id
	}
	out.Snapshot = &ContextSnapshot{SnapshotRef: SnapshotRef{Repo: snap.repo.Name, SnapshotID: snap.l.Record.SnapshotID, CapturedAt: snap.l.Record.CapturedAt},
		Digest: snap.l.Record.Digest, Latest: snap.latest}
	rd := &sourceReader{s: s, cache: map[string]fileState{}}
	// an expansion whose own content changed is refused: never other source
	if out.Action == "expand" {
		if f := snap.fileOf(seed); f != "" {
			if st := rd.file(snap, f); st.state == "stale" {
				out.Result, out.Freshness = "refused_stale", "stale"
				return h.ctxFinish(s, &out, in, start, "The file changed after the ref's snapshot; call hint_collect and hint_context with a new seed.")
			}
		}
	}
	out.startFile = snap.fileOf(seed)
	h.ctxWalk(snap, seed, lim, newCtxAdder(&out, lim, now, deadline), rd, &out)
	out.Result = "delivered"
	out.Freshness = "current"
	files := map[string]bool{}
	for _, it := range out.Items {
		files[snap.fileOf(it.idOf())] = true
	}
	for f := range files {
		if f == "" {
			continue
		}
		switch rd.file(snap, f).state {
		case "stale":
			out.Freshness = "stale"
		case "unknown":
			if out.Freshness == "current" {
				out.Freshness = "unknown"
			}
		}
	}
	return h.ctxFinish(s, &out, in, start, "%d items around the start; freshness %s.", len(out.Items), out.Freshness)
}

// idOf recovers the entity ID from an item's ref.
func (it ContextItem) idOf() string { return it.Ref[strings.LastIndexByte(it.Ref, '.')+1:] }

// fileOf is the file of a file or declaration entity ("" for a directory or repository).
func (c *ctxSnap) fileOf(id string) string {
	e := c.ents[id]
	switch {
	case e == nil:
		return ""
	case e.Kind == "file":
		return id
	case e.Kind == "directory" || e.Kind == "repository":
		return ""
	}
	return c.parent(id)
}

func (c *ctxSnap) item(id, rel string, hops int) ContextItem {
	ix := c.l.Index[id]
	it := ContextItem{Ref: ctxRef(c.l.Record.SnapshotID, id), Relation: rel, Hops: hops, Kind: c.ents[id].Kind, Repo: c.repo.Name,
		Path: ix.Path, QualifiedName: ix.Name, SourceState: "not_returned"}
	if ix.Name != "" {
		it.Line, it.Lines = ix.Line, c.lines(id)
	}
	return it
}

// ctxAdder adds items within the limits: entities, the time limit, and the bytes left for
// items after a reserve for the metadata of the answer. ctxFinish then measures the whole
// answer and drops items until it fits.
type ctxAdder struct {
	out      *ContextOut
	lim      ContextLimits
	now      func() time.Time
	deadline time.Time
	room     int // bytes for items
	used     int
}

func newCtxAdder(out *ContextOut, lim ContextLimits, now func() time.Time, deadline time.Time) *ctxAdder {
	// the items are sent twice: in the structured result and in the text view
	return &ctxAdder{out: out, lim: lim, now: now, deadline: deadline, room: (lim.Bytes - metaReserve(out)) / 2}
}

// metaReserve is the size of the answer without items (structured result, text view and
// measurement part), with the longest values that ctxFinish fills in.
func metaReserve(out *ContextOut) int {
	m := *out
	m.Items = []ContextItem{}
	m.Omitted = ContextOmitted{Entities: 1 << 30, SourceBytes: 1 << 30, Reasons: []string{"entities", "bytes", "time", "depth"}}
	m.Used = ContextUsed{Entities: 1 << 30, Bytes: 1 << 30, ItemBytes: 1 << 30, ElapsedMS: 1 << 30}
	id := strings.Repeat("0", 32)
	m.ArtifactID = id
	m.Measurement = ContextMeasurement{Action: "retrieve_original", Stage: "raw_tool_output", ArtifactID: id, RefID: id, SnapshotID: id,
		PolicyState: "not_applicable", PolicyRef: id, Freshness: "unknown", Result: "refused_stale", Bytes: 1 << 30, Omitted: 1 << 30,
		Tokens: map[string]any{"value": 1 << 30, "estimator": "bytes-div-4", "estimator_version": "1", "label": "estimate"}}
	if m.Snapshot == nil {
		m.Snapshot = &ContextSnapshot{SnapshotRef: SnapshotRef{SnapshotID: id, CapturedAt: "2006-01-02T15:04:05Z"}, Digest: "sha256:" + strings.Repeat("0", 64)}
	}
	n, _ := wire(&m, strings.Repeat("x", ctxMaxSummary), []string{id})
	return n
}

const ctxMaxSummary = 160 // the text summary of an answer is shorter

func (a *ctxAdder) omit(reason string, n int) {
	a.out.Omitted.Entities += n
	if !slices.Contains(a.out.Omitted.Reasons, reason) {
		a.out.Omitted.Reasons = append(a.out.Omitted.Reasons, reason)
	}
}

// late reports whether the time limit has passed.
func (a *ctxAdder) late() bool { return a.now().After(a.deadline) }

// item adds one item within the entity and byte limits, or records why not (the caller counts
// the items left out; it checks the time limit).
func (a *ctxAdder) item(it ContextItem) bool {
	if len(a.out.Items) >= a.lim.Entities {
		a.omit("entities", 0)
		return false
	}
	b, _ := json.Marshal(it)
	if a.used+len(b)+1 > a.room {
		a.omit("bytes", 0)
		return false
	}
	a.used += len(b) + 1
	a.out.Items = append(a.out.Items, it)
	return true
}

// withSource fills the item's source from the snapshot's file, cut at whole lines so that the
// item fits the bytes left.
func (a *ctxAdder) withSource(it ContextItem, c *ctxSnap, rd *sourceReader) ContextItem {
	if it.Line <= 0 {
		return it
	}
	st := rd.file(c, c.fileOf(it.idOf()))
	it.SourceState = st.state
	if st.state == "unknown" {
		it.SourceState = "not_returned"
	}
	if st.state != "returned" {
		return it
	}
	src := excerpt(st.b, it.Line, it.Lines)
	lines := strings.SplitAfter(src, "\n")
	fits := func(k int) bool { // the item with the first k lines fits, as JSON with its escaping
		it.Source = strings.Join(lines[:k], "")
		b, _ := json.Marshal(it)
		return a.used+len(b)+1 <= a.room
	}
	k := sort.Search(len(lines)+1, func(k int) bool { return !fits(k) }) - 1 // the most lines that fit
	if k < 0 {
		k = 0
	}
	it.Source = strings.Join(lines[:k], "")
	switch {
	case it.Source == "" && src != "":
		it.SourceState = "omitted_budget"
	case len(it.Source) < len(src):
		it.SourceState = "truncated"
	}
	if len(it.Source) < len(src) {
		a.out.Omitted.SourceBytes += len(src) - len(it.Source)
		a.omit("bytes", 0)
	}
	return it
}

// ctxWalk adds the start and its neighborhood breadth-first, within the limits. Only the
// start declaration and the tests that name it carry source.
func (h *handlers) ctxWalk(c *ctxSnap, seed string, lim ContextLimits, add *ctxAdder, rd *sourceReader, out *ContextOut) {
	seen := map[string]bool{seed: true}
	rel := "start"
	if e := c.ents[seed]; e.Kind != "file" && e.Kind != "directory" && e.Kind != "repository" {
		rel = "definition"
	}
	it := c.item(seed, rel, 0)
	if rel == "definition" {
		it = add.withSource(it, c, rd)
	}
	if !add.item(it) {
		out.Omitted.Entities++
		return
	}
	frontier := []string{seed}
	for hop := 1; hop <= lim.Depth && len(frontier) > 0; hop++ {
		var next []string
		for fi, id := range frontier {
			nb := c.neighbors(id)
			for k, e := range nb {
				if seen[e.id] {
					continue
				}
				if add.late() {
					add.omit("time", len(nb)-k)
					for _, rest := range frontier[fi+1:] {
						out.Omitted.Entities += len(c.neighbors(rest))
					}
					return
				}
				seen[e.id] = true
				it := c.item(e.id, e.rel, hop)
				if e.rel == "test" {
					it = add.withSource(it, c, rd)
				}
				if !add.item(it) {
					out.Omitted.Entities++
					continue
				}
				next = append(next, e.id)
			}
		}
		frontier = next
	}
	// links one hop further that the depth limit left out
	for _, id := range frontier {
		for _, e := range c.neighbors(id) {
			if !seen[e.id] {
				seen[e.id] = true
				add.omit("depth", 1)
			}
		}
	}
}

// refSnapshot loads the snapshot that a ref names, from any repository (or the named one).
func (h *handlers) refSnapshot(s *store.Store, repo, snapID string) (*ctxSnap, error) {
	found := repo == ""
	for _, r := range s.Cfg.Repos {
		if repo != "" && r.Name != repo {
			continue
		}
		found = true
		id := s.RepoID(r)
		l, err := project.LoadSnapshot(s, id, snapID)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		latest, err := project.LoadLatest(s, id)
		if err != nil {
			return nil, err
		}
		return newCtxSnap(r, l, latest != nil && latest.Record.SnapshotID == snapID), nil
	}
	if !found {
		return nil, toolErr("unknown_repository")
	}
	return nil, nil
}

// MeasurementMarker starts the text part that carries the context measurement of a
// hint_context answer for an agent adapter (metadata only: enumerations, numbers, opaque IDs).
const MeasurementMarker = "hint-context-measurement/1 "

// ctxView is the text form of an answer for clients that read text parts only.
type ctxView struct {
	Result     string           `json:"result"`
	Freshness  string           `json:"freshness"`
	Snapshot   *ContextSnapshot `json:"snapshot,omitempty"`
	Items      []ContextItem    `json:"items"`
	Omitted    ContextOmitted   `json:"omitted"`
	ArtifactID string           `json:"artifact_id,omitempty"`
}

// MeasurementPart is the JSON after MeasurementMarker: the measurement and the file IDs it
// is about (for the observation targets).
type MeasurementPart struct {
	Targets []string           `json:"targets"`
	Context ContextMeasurement `json:"context"`
}

// wire returns the size of the whole answer as sent (structured result, text view and
// measurement part) and the text parts.
func wire(out *ContextOut, summary string, targets []string) (int, []string) {
	b, _ := json.Marshal(out)
	view, _ := json.Marshal(ctxView{Result: out.Result, Freshness: out.Freshness, Snapshot: out.Snapshot, Items: out.Items, Omitted: out.Omitted, ArtifactID: out.ArtifactID})
	mp, _ := json.Marshal(MeasurementPart{Targets: targets, Context: out.Measurement})
	parts := []string{summary + "\n" + string(view), MeasurementMarker + string(mp)}
	return len(b) + len(parts[0]) + len(parts[1]), parts
}

// ctxFinish measures the answer and builds its v0.2 context measurement (stage
// raw_tool_output: the tool's whole answer as sent; an adapter measures what it receives as
// delivered_context). When the answer is larger than max_bytes, items are dropped from the
// end and counted as omitted.
func (h *handlers) ctxFinish(s *store.Store, out *ContextOut, in ContextIn, start time.Time, format string, a ...any) (*mcp.CallToolResult, ContextOut, error) {
	summary := fmt.Sprintf(format, a...)
	var targets []string
	if out.Snapshot != nil && len(out.Items) > 0 && out.Result != "ambiguous" {
		if f := h.ctxTarget(out); f != "" {
			targets = []string{f}
		}
	}
	if targets == nil {
		targets = []string{}
	}
	for {
		items, _ := json.Marshal(out.Items)
		m := ContextMeasurement{Action: out.Action, Stage: "raw_tool_output", PolicyState: "not_applicable", Freshness: out.Freshness,
			Result: out.Result, Omitted: out.Omitted.Entities}
		if out.policyRef != "" {
			m.PolicyState, m.PolicyRef = "enabled", out.policyRef
		}
		if m.Result == "ambiguous" {
			// candidates were delivered; candidates of several snapshots have no single snapshot
			m.Result = "delivered"
			if out.Snapshot == nil {
				m.Result = "missing"
			}
		}
		if out.Snapshot != nil {
			m.SnapshotID = out.Snapshot.SnapshotID
		}
		if in.Ref != "" {
			m.RefID = hint.DeriveID(s.Key(), "context-ref", in.Ref)
		}
		out.ArtifactID = ""
		if m.Result == "delivered" {
			sum := sha256.Sum256(items)
			snapID := ""
			if out.Snapshot != nil {
				snapID = out.Snapshot.SnapshotID + "\x00" + out.Snapshot.Digest
			}
			out.ArtifactID = hint.DeriveID(s.Key(), "context-artifact", snapID, hex.EncodeToString(sum[:]))
			m.ArtifactID = out.ArtifactID
		}
		out.Used = ContextUsed{Entities: len(out.Items), ItemBytes: len(items), ElapsedMS: int(h.o.Now().Sub(start).Milliseconds())}
		// the size names itself: repeat until the number of digits is stable
		total := 0
		var parts []string
		for range 6 {
			out.Used.Bytes, m.Bytes = total, total
			m.Tokens = map[string]any{"value": total / 4, "estimator": "bytes-div-4", "estimator_version": "1", "label": "estimate"}
			out.Measurement = m
			n, p := wire(out, summary, targets)
			parts = p
			if n == total {
				break
			}
			total = n
		}
		if total <= out.limitsOrZero() || len(out.Items) == 0 {
			res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: parts[0]}, &mcp.TextContent{Text: parts[1]}}}
			return res, *out, nil
		}
		out.Items = out.Items[:len(out.Items)-1]
		out.Omitted.Entities++
		if !slices.Contains(out.Omitted.Reasons, "bytes") {
			out.Omitted.Reasons = append(out.Omitted.Reasons, "bytes")
		}
	}
}

// ctxTarget is the file of the answer's first item (the start or the expanded entity): the
// observation target of the call.
func (h *handlers) ctxTarget(out *ContextOut) string {
	it := out.Items[0]
	if it.Kind == "directory" || it.Kind == "repository" {
		return ""
	}
	if it.Kind == "file" {
		return it.idOf()
	}
	return out.startFile
}

// limitsOrZero is the effective byte limit of the answer.
func (o ContextOut) limitsOrZero() int { return o.Limits.Bytes }
