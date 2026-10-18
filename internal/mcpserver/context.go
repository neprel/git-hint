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
	ctxDefaultDepth, ctxMaxDepth       = 1, 2
	ctxDefaultEntities, ctxMaxEntities = 20, 100
	ctxDefaultBytes, ctxMaxBytes       = 16 << 10, 64 << 10
	ctxDefaultMillis, ctxMaxMillis     = 1000, 5000
	ctxMaxFileBytes                    = 4 << 20 // a larger file gives no source
	ctxMaxHashBytes                    = 8 << 20 // freshness checks read at most this much
	ctxRefPrefix                       = "hc1."
	ctxCoverage                        = "Go declarations, file imports of repository packages and test files of the last collected snapshot (syntax level, no call graph); other languages give files only; source is read from the work tree only when its content hash equals the snapshot"
)

var ctxRefPattern = regexp.MustCompile(`^hc1\.[0-9a-f]{32}\.[0-9a-f]{32}$`)

type ContextIn struct {
	Seed        string `json:"seed,omitempty" jsonschema:"start entity: a declaration name (Rate, tax.Rate, Cart.Total), a file path or a package directory; not with ref"`
	Ref         string `json:"ref,omitempty" jsonschema:"a ref from an earlier hint_context answer to expand; not with seed"`
	Repo        string `json:"repository,omitempty" jsonschema:"local repository name; default all"`
	Depth       int    `json:"depth,omitempty" jsonschema:"graph hops from the start (1 or 2; default 1)"`
	MaxEntities int    `json:"max_entities,omitempty" jsonschema:"at most this many items (1-100; default 20)"`
	MaxBytes    int    `json:"max_bytes,omitempty" jsonschema:"at most this many UTF-8 bytes of items (1024-65536; default 16384)"`
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
	Bytes     int `json:"bytes" jsonschema:"UTF-8 bytes of the items as returned"`
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
}

func (h *handlers) registerContext(s *mcp.Server, ro *mcp.ToolAnnotations) {
	add(s, &mcp.Tool{Name: "hint_context", Annotations: ro,
		Description: "Side effect: none (read-only, local index; stores nothing). A small context around one entity: the definition with its source, the other declarations of its file, imports, importers and tests, as items with refs. Call again with ref to expand one item. Limits: depth, items, bytes, time. A ref reads its own snapshot; when the file changed since then, the answer is refused_stale (call hint_collect)."}, h.contextTool)
}

func (o *ContextIn) limits() (ContextLimits, bool) {
	l := ContextLimits{Depth: o.Depth, Entities: o.MaxEntities, Bytes: o.MaxBytes, MaxMillis: o.MaxMillis}
	def := func(v *int, d, lo, hi int) bool {
		if *v == 0 {
			*v = d
		}
		return *v >= lo && *v <= hi
	}
	ok := def(&l.Depth, ctxDefaultDepth, 1, ctxMaxDepth) && def(&l.Entities, ctxDefaultEntities, 1, ctxMaxEntities) &&
		def(&l.Bytes, ctxDefaultBytes, 1024, ctxMaxBytes) && def(&l.MaxMillis, ctxDefaultMillis, 100, ctxMaxMillis)
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

// truncLines keeps whole lines of s within max bytes.
func truncLines(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut < 0 {
		return ""
	}
	return s[:cut+1]
}

func (h *handlers) contextTool(ctx context.Context, _ *mcp.CallToolRequest, in ContextIn) (*mcp.CallToolResult, ContextOut, error) {
	lim, ok := in.limits()
	if !ok || (in.Seed == "") == (in.Ref == "") || len(in.Repo) > 200 || (in.Ref != "" && !ctxRefPattern.MatchString(in.Ref)) ||
		(in.Seed != "" && !validQuery(in.Seed)) {
		return nil, ContextOut{}, h.fail("hint_context", toolErr("invalid_argument"))
	}
	start := time.Now()
	deadline := start.Add(time.Duration(lim.MaxMillis) * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	s, err := h.open()
	if err != nil {
		return nil, ContextOut{}, h.fail("hint_context", err)
	}
	out := ContextOut{SideEffect: effectNone, Action: "deliver", Items: []ContextItem{}, Limits: lim, Coverage: ctxCoverage,
		Omitted: ContextOmitted{Reasons: []string{}}}
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
			for i, x := range hits {
				if i >= lim.Entities {
					out.Omitted.Entities = len(hits) - lim.Entities
					out.Omitted.Reasons = append(out.Omitted.Reasons, "entities")
					break
				}
				out.Items = append(out.Items, x.c.item(x.id, "candidate", 0))
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
	h.ctxWalk(snap, seed, lim, deadline, rd, &out)
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

// ctxWalk adds the start and its neighborhood breadth-first, within the limits. Only the
// start declaration and the tests that name it carry source.
func (h *handlers) ctxWalk(c *ctxSnap, seed string, lim ContextLimits, deadline time.Time, rd *sourceReader, out *ContextOut) {
	used := 0
	seen := map[string]bool{seed: true}
	stop := func(reason string, left int) {
		out.Omitted.Entities += left
		if !slices.Contains(out.Omitted.Reasons, reason) {
			out.Omitted.Reasons = append(out.Omitted.Reasons, reason)
		}
	}
	addItem := func(it ContextItem, withSource bool) bool {
		if withSource && it.Line > 0 {
			st := rd.file(c, c.fileOf(it.idOf()))
			it.SourceState = st.state
			if st.state == "unknown" {
				it.SourceState = "not_returned"
			}
			if st.state == "returned" {
				src := excerpt(st.b, it.Line, it.Lines)
				meta, _ := json.Marshal(it)
				room := lim.Bytes - used - len(meta) - 2
				it.Source = truncLines(src, max(room-len(src)/8-16, 0)) // JSON escaping can grow the text
				for it.Source != "" {
					b, _ := json.Marshal(it)
					if used+len(b)+1 <= lim.Bytes {
						break
					}
					it.Source = truncLines(it.Source, len(it.Source)-1)
				}
				switch {
				case it.Source == "" && src != "":
					it.SourceState = "omitted_budget"
				case len(it.Source) < len(src):
					it.SourceState = "truncated"
				}
				if len(it.Source) < len(src) {
					out.Omitted.SourceBytes += len(src) - len(it.Source)
					if !slices.Contains(out.Omitted.Reasons, "bytes") {
						out.Omitted.Reasons = append(out.Omitted.Reasons, "bytes")
					}
				}
			}
		}
		b, _ := json.Marshal(it)
		if used+len(b)+1 > lim.Bytes {
			return false
		}
		used += len(b) + 1
		out.Items = append(out.Items, it)
		return true
	}
	rel := "start"
	if e := c.ents[seed]; e.Kind != "file" && e.Kind != "directory" && e.Kind != "repository" {
		rel = "definition"
	}
	if !addItem(c.item(seed, rel, 0), rel == "definition") {
		stop("bytes", 1)
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
				if time.Now().After(deadline) {
					stop("time", len(nb)-k)
					for _, rest := range frontier[fi+1:] {
						out.Omitted.Entities += len(c.neighbors(rest))
					}
					return
				}
				if len(out.Items) >= lim.Entities {
					stop("entities", 1)
					seen[e.id] = true
					continue
				}
				seen[e.id] = true
				if !addItem(c.item(e.id, e.rel, hop), e.rel == "test") {
					stop("bytes", 1)
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
				stop("depth", 1)
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

// ctxFinish measures the answer and builds its v0.2 context measurement.
func (h *handlers) ctxFinish(s *store.Store, out *ContextOut, in ContextIn, start time.Time, format string, a ...any) (*mcp.CallToolResult, ContextOut, error) {
	b, _ := json.Marshal(out.Items)
	out.Used = ContextUsed{Entities: len(out.Items), Bytes: len(b), ElapsedMS: int(time.Since(start).Milliseconds())}
	m := ContextMeasurement{Action: out.Action, Stage: "delivered_context", PolicyState: "not_applicable", Freshness: out.Freshness,
		Result: out.Result, Bytes: len(b), Omitted: out.Omitted.Entities,
		Tokens: map[string]any{"value": len(b) / 4, "estimator": "bytes-div-4", "estimator_version": "1", "label": "estimate"}}
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
	if m.Result == "delivered" {
		sum := sha256.Sum256(b)
		snapID := ""
		if out.Snapshot != nil {
			snapID = out.Snapshot.SnapshotID + "\x00" + out.Snapshot.Digest
		}
		out.ArtifactID = hint.DeriveID(s.Key(), "context-artifact", snapID, hex.EncodeToString(sum[:]))
		m.ArtifactID = out.ArtifactID
	}
	out.Measurement = m
	return text(format, a...), *out, nil
}
