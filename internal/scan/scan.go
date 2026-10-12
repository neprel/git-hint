// Package scan builds the structure of one repository: directories, files and, for Go,
// packages' declarations and imports. It reads files on the local machine only. Names,
// paths and content hashes stay in the local index; the exported records hold opaque IDs.
//
// Coverage: the file inventory comes from Git (tracked and untracked, not ignored files).
// Go sources are parsed with the Go standard library parser (go/parser): resolution
// "syntax", no type checking. Other languages are covered at file level. A missing
// optional indexer reduces coverage; it never stops the scan.
package scan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/hint"
)

// Version is the scanner version written into snapshots.
const Version = "0.1.0"

// Quantity is a HINT Quantity (hint-data §5.1).
type Quantity struct {
	Unit      string   `json:"unit"`
	State     string   `json:"state"`
	Value     *float64 `json:"value,omitempty"`
	Precision string   `json:"precision,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Method    string   `json:"method,omitempty"`
}

// Known returns an exact known quantity.
func Known(unit string, v float64, method string) Quantity {
	return Quantity{Unit: unit, State: "known", Value: &v, Precision: "exact", Method: method}
}

// Entity is one structural element. The json-tagged fields are the HINT record; the
// other fields form the local index and are never exported.
type Entity struct {
	ID         string              `json:"entity_id"`
	Kind       string              `json:"kind"`
	ParentID   *string             `json:"parent_id"`
	Language   *string             `json:"language"`
	Flags      []string            `json:"flags,omitempty"`
	Features   map[string]Quantity `json:"features,omitempty"`
	Resolution string              `json:"resolution"`
	Stability  string              `json:"id_stability"`

	Path string `json:"-"` // repository-relative path with "/"
	Name string `json:"-"` // qualified name of a symbol ("" for directories and files)
	Line int    `json:"-"` // first line of a symbol
	Hash string `json:"-"` // content hash of a file or a symbol body (local only)
	// localKey is set for an entity whose stable ID collided; the snapshot builder
	// assigns a snapshot-local ID.
	LocalKey string `json:"-"`
}

// Relation is a directed relation between two entities of the same snapshot.
type Relation struct {
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Resolution string `json:"resolution"`
	Evidence   string `json:"evidence"`
}

// Result is the scan of one repository.
type Result struct {
	Entities  []Entity
	Relations []Relation
	Languages []string
	// Ambiguous maps a stable symbol ID to the local names that collided on it. Such
	// IDs never resolve to one local entity (exchange §4, ambiguous_id).
	Ambiguous map[string][]string
	Stats     Stats
}

// Stats counts the work of one scan.
type Stats struct {
	Files    int `json:"files"`
	Parsed   int `json:"parsed"`
	Reused   int `json:"reused"`
	Skipped  int `json:"skipped"`
	GoFiles  int `json:"go_files"`
	Symbols  int `json:"symbols"`
	Unparsed int `json:"go_parse_errors"`
}

// Options control one scan.
type Options struct {
	Root   string // repository root
	RepoID string
	Key    []byte
	Cache  *Cache // nil: full scan without cache
}

var extLang = map[string]string{
	".go": "go", ".py": "python", ".ts": "typescript", ".tsx": "typescript", ".mts": "typescript",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".java": "java",
	".kt": "kotlin", ".kts": "kotlin", ".rs": "rust", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp",
	".cxx": "cpp", ".hpp": "cpp", ".cs": "csharp", ".rb": "ruby", ".php": "php", ".swift": "swift",
	".scala": "scala", ".sh": "shell", ".bash": "shell", ".zsh": "shell", ".sql": "sql", ".html": "html",
	".htm": "html", ".css": "css", ".scss": "scss", ".md": "markdown", ".markdown": "markdown",
	".yaml": "yaml", ".yml": "yaml", ".json": "json", ".toml": "toml", ".xml": "xml", ".proto": "proto",
	".graphql": "graphql", ".gql": "graphql",
}

// Language returns the HINT language of a repository path, or "" if unknown.
func Language(p string) string {
	base := path.Base(p)
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") {
		return "dockerfile"
	}
	return extLang[strings.ToLower(path.Ext(p))]
}

// ListFiles returns the repository-relative paths that Git tracks or that are untracked
// and not ignored, sorted. The `.hint/` store is never listed.
func ListFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || seen[p] || p == ".hint" || strings.HasPrefix(p, ".hint/") {
			continue
		}
		seen[p] = true
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() {
			continue // deleted in the work tree, a symlink or a submodule
		}
		files = append(files, p)
	}
	sort.Strings(files)
	return files, nil
}

// Scan scans one repository.
func Scan(opt Options) (*Result, error) {
	files, err := ListFiles(opt.Root)
	if err != nil {
		return nil, err
	}
	res := &Result{Ambiguous: map[string][]string{}}
	k, repo := opt.Key, opt.RepoID
	repoEnt := Entity{ID: repo, Kind: "repository", Resolution: "declared", Stability: "stable"}
	ents := []Entity{repoEnt}
	dirs := map[string]string{"": repo} // dir path -> entity ID
	var rels []Relation
	contains := func(parent, child, res, ev string) {
		rels = append(rels, Relation{Kind: "contains", Source: parent, Target: child, Resolution: res, Evidence: ev})
	}
	var ensureDir func(d string) string
	ensureDir = func(d string) string {
		if id, ok := dirs[d]; ok {
			return id
		}
		parent := ensureDir(parentDir(d))
		id := hint.FileID(k, repo, d)
		dirs[d] = id
		ents = append(ents, Entity{ID: id, Kind: "directory", ParentID: &parent, Resolution: "declared",
			Stability: "stable", Path: d, Flags: pathFlags(d, true)})
		contains(parent, id, "declared", "git")
		return id
	}
	langs := map[string]bool{}
	modules := goModules(opt.Root, files)
	var goFiles []goFile
	for _, p := range files {
		res.Stats.Files++
		parent := ensureDir(parentDir(p))
		fe, cached, err := fileEntry(opt, p)
		if err != nil {
			res.Stats.Skipped++
			continue
		}
		if cached {
			res.Stats.Reused++
		} else {
			res.Stats.Parsed++
		}
		id := hint.FileID(k, repo, p)
		e := Entity{ID: id, Kind: "file", ParentID: &parent, Resolution: "declared", Stability: "stable",
			Path: p, Hash: fe.Hash, Flags: append(pathFlags(p, false), fe.Flags...),
			Features: map[string]Quantity{"size_bytes": Known("bytes", float64(fe.Size), "file_size")}}
		if fe.Lines >= 0 {
			e.Features["lines"] = Known("lines", float64(fe.Lines), "scanner")
		}
		if l := Language(p); l != "" && !fe.Binary {
			e.Language = &l
			langs[l] = true
		}
		e.Flags = uniq(e.Flags)
		ents = append(ents, e)
		contains(parent, id, "declared", "git")
		if fe.Go != nil {
			res.Stats.GoFiles++
			if fe.Go.ParseError {
				res.Stats.Unparsed++
			}
			goFiles = append(goFiles, goFile{path: p, id: id, info: fe.Go})
		}
		if opt.Cache != nil {
			opt.Cache.put(p, fe)
		}
	}
	// Go declarations: one entity per declaration; a stable ID per (package dir, name).
	type sym struct {
		e     Entity
		count int
	}
	byID := map[string][]Entity{}
	var order []string
	golang := "go"
	for _, gf := range goFiles {
		dir := parentDir(gf.path)
		for _, d := range gf.info.Decls {
			q := qualname(dir, d)
			sid := hint.SymbolID(k, repo, "go", q)
			parent := gf.id
			e := Entity{ID: sid, Kind: d.Kind, ParentID: &parent, Language: &golang, Resolution: "syntax",
				Stability: "stable", Path: gf.path, Name: q, Line: d.Line, Hash: d.Hash, Flags: d.Flags,
				Features: map[string]Quantity{"lines": Known("lines", float64(d.Lines), "scanner")}}
			if d.Kind == "function" || d.Kind == "method" {
				e.Features["parameters"] = Known("count", float64(d.Params), "scanner")
				e.Features["cyclomatic"] = Known("count", float64(d.Cyclomatic), "scanner")
			}
			if _, ok := byID[sid]; !ok {
				order = append(order, sid)
			}
			byID[sid] = append(byID[sid], e)
		}
	}
	for _, sid := range order {
		group := byID[sid]
		if len(group) == 1 {
			ents = append(ents, group[0])
			contains(*group[0].ParentID, sid, "syntax", "compiler")
			continue
		}
		// Two declarations with one qualified name (init functions, build-tag variants):
		// each gets a snapshot-local ID; the stable ID is ambiguous.
		for i, e := range group {
			res.Ambiguous[sid] = append(res.Ambiguous[sid], e.Path+"#"+e.Name)
			e.LocalKey = fmt.Sprintf("%s#%d", sid, i)
			e.Stability = "snapshot_local"
			ents = append(ents, e)
		}
	}
	res.Stats.Symbols = len(order)
	// Go imports of packages inside the repository: file -> package directory.
	for _, gf := range goFiles {
		seen := map[string]bool{}
		for _, imp := range gf.info.Imports {
			d, ok := modules.dirOf(imp)
			if !ok || seen[d] {
				continue
			}
			if target, ok := dirs[d]; ok && d != "" {
				seen[d] = true
				rels = append(rels, Relation{Kind: "imports", Source: gf.id, Target: target, Resolution: "syntax", Evidence: "compiler"})
			}
		}
	}
	for l := range langs {
		res.Languages = append(res.Languages, l)
	}
	sort.Strings(res.Languages)
	res.Entities, res.Relations = ents, rels
	return res, nil
}

type goFile struct {
	path string
	id   string
	info *GoInfo
}

func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

func qualname(dir string, d GoDecl) string {
	if dir == "" {
		dir = "."
	}
	if d.Recv != "" {
		return dir + "." + d.Recv + "." + d.Name
	}
	return dir + "." + d.Name
}

func pathFlags(p string, isDir bool) []string {
	var f []string
	parts := strings.Split(p, "/")
	for _, s := range parts {
		switch s {
		case "vendor", "node_modules", "third_party":
			f = append(f, "vendored")
		case "test", "tests", "testdata", "__tests__":
			f = append(f, "test")
		}
	}
	if !isDir {
		base := parts[len(parts)-1]
		switch {
		case strings.HasSuffix(base, "_test.go"), strings.Contains(base, ".test."), strings.Contains(base, ".spec."),
			strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
			f = append(f, "test")
		}
		switch Language(base) {
		case "yaml", "json", "toml":
			f = append(f, "config")
		}
	}
	return uniq(f)
}

func uniq(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// fileEntry reads and analyzes one file. Every file is read and hashed on every scan; the
// cached analysis (parse result) is reused only when the content hash is the same. Size and
// modification time alone never decide: a content change that keeps both must be found.
func fileEntry(opt Options, p string) (*FileEntry, bool, error) {
	full := filepath.Join(opt.Root, filepath.FromSlash(p))
	fi, err := os.Stat(full)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, false, err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if opt.Cache != nil {
		if fe := opt.Cache.get(p, hash); fe != nil {
			return fe, true, nil
		}
	}
	fe := &FileEntry{Size: int64(len(b)), ModNS: fi.ModTime().UnixNano(), Hash: hash, Lines: -1}
	probe := b
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	fe.Binary = bytes.IndexByte(probe, 0) >= 0
	if !fe.Binary {
		fe.Lines = countLines(b)
		if Language(p) == "go" {
			fe.Go = parseGo(p, b)
			if fe.Go.Generated {
				fe.Flags = append(fe.Flags, "generated")
			}
		}
	}
	return fe, false, nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// ---------------------------------------------------------------------------
// Go module paths: import path -> repository directory
// ---------------------------------------------------------------------------

type goMods []struct{ dir, module string }

func goModules(root string, files []string) goMods {
	var m goMods
	for _, p := range files {
		if path.Base(p) != "go.mod" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "module" {
				m = append(m, struct{ dir, module string }{parentDir(p), strings.Trim(f[1], `"`)})
				break
			}
		}
	}
	// longest module path first
	sort.Slice(m, func(i, j int) bool { return len(m[i].module) > len(m[j].module) })
	return m
}

func (m goMods) dirOf(importPath string) (string, bool) {
	for _, x := range m {
		if importPath == x.module {
			return x.dir, true
		}
		if rest, ok := strings.CutPrefix(importPath, x.module+"/"); ok {
			return path.Join(x.dir, rest), true
		}
	}
	return "", false
}
