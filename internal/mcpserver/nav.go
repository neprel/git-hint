package mcpserver

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// Navigation tools: read-only answers from the local index of the last stored snapshot
// (`git hint collect` or hint_collect). They read no file and write nothing. Lines refer
// to the state at collection time.

const maxMatches = 100

type SnapshotRef struct {
	Repo       string `json:"repository"`
	SnapshotID string `json:"snapshot_id"`
	CapturedAt string `json:"captured_at"`
}

type Definition struct {
	Repo          string `json:"repository"`
	Path          string `json:"path"`
	Line          int    `json:"line"`
	Lines         int    `json:"lines"`
	Kind          string `json:"kind"`
	QualifiedName string `json:"qualified_name" jsonschema:"package directory, receiver and name"`
}

type FindDefinitionIn struct {
	Name string `json:"name" jsonschema:"declaration name, optionally qualified: Rate, tax.Rate, Cart.Total or cart.Cart.Total"`
	Repo string `json:"repository,omitempty" jsonschema:"local repository name; default all"`
}

type FindDefinitionOut struct {
	SideEffect  string        `json:"side_effect"`
	Snapshots   []SnapshotRef `json:"snapshots"`
	Definitions []Definition  `json:"definitions"`
	Truncated   bool          `json:"truncated"`
	Coverage    string        `json:"coverage"`
}

type ImportersIn struct {
	Package string `json:"package" jsonschema:"package directory relative to the repository root, for example tax or internal/util00"`
	Repo    string `json:"repository,omitempty" jsonschema:"local repository name; default all"`
}

type Importer struct {
	Repo string `json:"repository"`
	Path string `json:"path"`
}

type ImportersOut struct {
	SideEffect string        `json:"side_effect"`
	Snapshots  []SnapshotRef `json:"snapshots"`
	Found      bool          `json:"package_found"`
	Importers  []Importer    `json:"importers"`
	Truncated  bool          `json:"truncated"`
	Coverage   string        `json:"coverage"`
}

type OutlineIn struct {
	Path string `json:"path" jsonschema:"file path relative to the repository root"`
	Repo string `json:"repository,omitempty" jsonschema:"local repository name; default the first repository that has the file"`
}

type OutlineItem struct {
	Kind          string `json:"kind"`
	QualifiedName string `json:"qualified_name"`
	Line          int    `json:"line"`
	Lines         int    `json:"lines"`
}

type OutlineOut struct {
	SideEffect   string        `json:"side_effect"`
	Snapshots    []SnapshotRef `json:"snapshots"`
	Found        bool          `json:"file_found"`
	Repo         string        `json:"repository,omitempty"`
	Path         string        `json:"path,omitempty"`
	Language     string        `json:"language,omitempty"`
	FileLines    int           `json:"file_lines"`
	Declarations []OutlineItem `json:"declarations"`
	Coverage     string        `json:"coverage"`
}

const navCoverage = "Go declarations and imports of the last collected snapshot (syntax level); other languages have no declarations; lines may be out of date after edits (call hint_collect)"

func (h *handlers) registerNav(s *mcp.Server, ro *mcp.ToolAnnotations) {
	add(s, &mcp.Tool{Name: "hint_find_definition", Annotations: ro,
		Description: "Side effect: none (read-only, local index). Find where a declaration is defined: file and line of each match for a name such as Rate, tax.Rate or Cart.Total."}, h.findDefinition)
	add(s, &mcp.Tool{Name: "hint_importers", Annotations: ro,
		Description: "Side effect: none (read-only, local index). List the files that import a package directory of the repository."}, h.importers)
	add(s, &mcp.Tool{Name: "hint_file_outline", Annotations: ro,
		Description: "Side effect: none (read-only, local index). List the declarations of a file with kinds and line numbers, without reading the file content."}, h.fileOutline)
}

type repoSnap struct {
	name string
	id   string
	l    *snapshot.Local
}

// latest loads the last stored snapshot of the selected repositories.
func (h *handlers) latest(s *store.Store, repo string) ([]repoSnap, []SnapshotRef, error) {
	var out []repoSnap
	refs := []SnapshotRef{}
	found := repo == ""
	for _, r := range s.Cfg.Repos {
		if repo != "" && r.Name != repo {
			continue
		}
		found = true
		id := s.RepoID(r)
		l, err := project.LoadLatest(s, id)
		if err != nil {
			return nil, nil, err
		}
		if l == nil {
			return nil, nil, toolErr("no_snapshot")
		}
		out = append(out, repoSnap{r.Name, id, l})
		refs = append(refs, SnapshotRef{Repo: r.Name, SnapshotID: l.Record.SnapshotID, CapturedAt: l.Record.CapturedAt})
	}
	if !found {
		return nil, nil, toolErr("unknown_repository")
	}
	return out, refs, nil
}

// nameMatches reports whether a qualified name (dir.Name or dir.Recv.Name) matches a
// query: the full name, or a suffix after "." or "/".
func nameMatches(qual, q string) bool {
	return qual == q || strings.HasSuffix(qual, "."+q) || strings.HasSuffix(qual, "/"+q)
}

func validQuery(q string) bool {
	if q == "" || len(q) > 200 {
		return false
	}
	for _, r := range q {
		if !(r == '.' || r == '_' || r == '/' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func cleanRel(p string) (string, bool) {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", false
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

func (h *handlers) findDefinition(ctx context.Context, _ *mcp.CallToolRequest, in FindDefinitionIn) (*mcp.CallToolResult, FindDefinitionOut, error) {
	if !validQuery(in.Name) || len(in.Repo) > 200 {
		return nil, FindDefinitionOut{}, h.fail("hint_find_definition", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, FindDefinitionOut{}, h.fail("hint_find_definition", err)
	}
	snaps, refs, err := h.latest(s, in.Repo)
	if err != nil {
		return nil, FindDefinitionOut{}, h.fail("hint_find_definition", err)
	}
	out := FindDefinitionOut{SideEffect: effectNone, Snapshots: refs, Definitions: []Definition{}, Coverage: navCoverage}
	for _, rs := range snaps {
		for _, e := range rs.l.Entities {
			ix := rs.l.Index[e.ID]
			if ix.Name == "" || !nameMatches(ix.Name, in.Name) {
				continue
			}
			if len(out.Definitions) >= maxMatches {
				out.Truncated = true
				break
			}
			lines := 0
			if q, ok := e.Features["lines"]; ok && q.Value != nil {
				lines = int(*q.Value)
			}
			out.Definitions = append(out.Definitions, Definition{Repo: rs.name, Path: ix.Path, Line: ix.Line, Lines: lines,
				Kind: e.Kind, QualifiedName: ix.Name})
		}
	}
	sort.Slice(out.Definitions, func(i, j int) bool {
		a, b := out.Definitions[i], out.Definitions[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return text("%d definitions of %s.", len(out.Definitions), in.Name), out, nil
}

func (h *handlers) importers(ctx context.Context, _ *mcp.CallToolRequest, in ImportersIn) (*mcp.CallToolResult, ImportersOut, error) {
	dir, ok := cleanRel(in.Package)
	if !ok || len(in.Repo) > 200 {
		return nil, ImportersOut{}, h.fail("hint_importers", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, ImportersOut{}, h.fail("hint_importers", err)
	}
	snaps, refs, err := h.latest(s, in.Repo)
	if err != nil {
		return nil, ImportersOut{}, h.fail("hint_importers", err)
	}
	out := ImportersOut{SideEffect: effectNone, Snapshots: refs, Importers: []Importer{}, Coverage: navCoverage}
	for _, rs := range snaps {
		dirID := hint.FileID(s.Key(), rs.id, dir)
		if _, ok := rs.l.Index[dirID]; !ok {
			continue
		}
		out.Found = true
		for _, r := range rs.l.Relations {
			if r.Kind != "imports" || r.Target != dirID {
				continue
			}
			if len(out.Importers) >= maxMatches {
				out.Truncated = true
				break
			}
			out.Importers = append(out.Importers, Importer{Repo: rs.name, Path: rs.l.Index[r.Source].Path})
		}
	}
	sort.Slice(out.Importers, func(i, j int) bool {
		return out.Importers[i].Repo+"/"+out.Importers[i].Path < out.Importers[j].Repo+"/"+out.Importers[j].Path
	})
	if !out.Found {
		return text("No package directory %s in the last snapshot.", dir), out, nil
	}
	return text("%d files import %s.", len(out.Importers), dir), out, nil
}

func (h *handlers) fileOutline(ctx context.Context, _ *mcp.CallToolRequest, in OutlineIn) (*mcp.CallToolResult, OutlineOut, error) {
	p, ok := cleanRel(in.Path)
	if !ok || len(in.Repo) > 200 {
		return nil, OutlineOut{}, h.fail("hint_file_outline", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, OutlineOut{}, h.fail("hint_file_outline", err)
	}
	snaps, refs, err := h.latest(s, in.Repo)
	if err != nil {
		return nil, OutlineOut{}, h.fail("hint_file_outline", err)
	}
	out := OutlineOut{SideEffect: effectNone, Snapshots: refs, Declarations: []OutlineItem{}, Coverage: navCoverage}
	for _, rs := range snaps {
		fileID := hint.FileID(s.Key(), rs.id, p)
		ix, ok := rs.l.Index[fileID]
		if !ok || ix.Kind != "file" {
			continue
		}
		out.Found, out.Repo, out.Path = true, rs.name, p
		for _, e := range rs.l.Entities {
			if e.ID == fileID {
				if e.Language != nil {
					out.Language = *e.Language
				}
				if q, ok := e.Features["lines"]; ok && q.Value != nil {
					out.FileLines = int(*q.Value)
				}
			}
			if e.ParentID == nil || *e.ParentID != fileID || rs.l.Index[e.ID].Name == "" {
				continue
			}
			lines := 0
			if q, ok := e.Features["lines"]; ok && q.Value != nil {
				lines = int(*q.Value)
			}
			ex := rs.l.Index[e.ID]
			out.Declarations = append(out.Declarations, OutlineItem{Kind: e.Kind, QualifiedName: ex.Name, Line: ex.Line, Lines: lines})
		}
		break
	}
	sort.Slice(out.Declarations, func(i, j int) bool { return out.Declarations[i].Line < out.Declarations[j].Line })
	if !out.Found {
		return text("No file %s in the last snapshot.", p), out, nil
	}
	return text("%s: %d declarations.", p, len(out.Declarations)), out, nil
}
