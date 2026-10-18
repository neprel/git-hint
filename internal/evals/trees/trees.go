// Package trees stores exact repository states as content-addressed HINT trees.
//
// A tree is a sorted JSON manifest of files (path, mode, size, content digest); its digest
// is the digest of the manifest bytes. Trees come from a work tree (read-only Git
// commands) or from OpenCode's snapshot repository (read-only, every blob checked against
// its Git hash). Nothing here writes to a repository or to its Git metadata.
package trees

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/safefs"
)

// Limits of one tree.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
}

// DefaultLimits are used when a field is zero.
var DefaultLimits = Limits{MaxFileBytes: 8 << 20, MaxTotalBytes: 256 << 20, MaxFiles: 50000}

func (l Limits) filled() Limits {
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultLimits.MaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultLimits.MaxTotalBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultLimits.MaxFiles
	}
	return l
}

// File is one entry of a tree. Mode is "100644", "100755" or "120000" (symbolic link; the
// object holds the link target).
type File struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// Tree is the manifest.
type Tree struct {
	TreeVersion int    `json:"tree_version"`
	Files       []File `json:"files"`
}

// Unsupported explains why a state cannot be stored exactly. Code is an eval reason code.
type Unsupported struct {
	Code   string // submodule_unsupported, lfs_unsupported, binary_too_large, baseline_missing
	Detail string // never file content
}

func (u *Unsupported) Error() string { return u.Code + ": " + u.Detail }

func unsupported(code, f string, a ...any) error {
	return &Unsupported{Code: code, Detail: fmt.Sprintf(f, a...)}
}

var lfsPointer = []byte("version https://git-lfs.github.com/spec/v1")

// validPath accepts a relative slash path without empty, "." or ".." parts.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") || strings.Contains(p, "\\") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
	}
	return true
}

// putTree writes the manifest and returns the tree digest.
func putTree(l *library.Library, files []File) (string, error) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	b, err := json.Marshal(Tree{TreeVersion: 1, Files: files})
	if err != nil {
		return "", err
	}
	return l.PutObject(b)
}

// Load reads a tree manifest.
func Load(l *library.Library, digest string) (Tree, error) {
	var t Tree
	b, err := l.GetObject(digest)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(b, &t); err != nil || t.TreeVersion != 1 {
		return t, fmt.Errorf("%s is not a HINT tree", digest)
	}
	return t, nil
}

// FromWorktree stores the current state of a Git work tree: tracked and untracked files
// that Git does not ignore. Symbolic links are recorded, never followed.
func FromWorktree(ctx context.Context, l *library.Library, root string, lim Limits) (string, error) {
	lim = lim.filled()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate").Output()
	if err != nil {
		return "", fmt.Errorf("git ls-files failed in the repository")
	}
	sub, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--stage").Output()
	if err == nil {
		for _, rec := range bytes.Split(sub, []byte{0}) {
			if bytes.HasPrefix(rec, []byte("160000 ")) {
				return "", unsupported("submodule_unsupported", "the repository has a submodule")
			}
		}
	}
	var files []File
	var total int64
	for _, raw := range bytes.Split(out, []byte{0}) {
		p := string(raw)
		if p == "" {
			continue
		}
		if !validPath(p) {
			return "", unsupported("baseline_missing", "unexpected path form in the work tree")
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		fi, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the work tree but still in the index: absent from this state
		}
		if err != nil {
			return "", err
		}
		var data []byte
		mode := "100644"
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			data, mode = []byte(t), "120000"
		case fi.Mode().IsRegular():
			if fi.Size() > lim.MaxFileBytes {
				return "", unsupported("binary_too_large", "a file exceeds %d bytes", lim.MaxFileBytes)
			}
			if data, err = os.ReadFile(full); err != nil {
				return "", err
			}
			if fi.Mode()&0o111 != 0 {
				mode = "100755"
			}
		default:
			continue
		}
		if bytes.HasPrefix(data, lfsPointer) {
			return "", unsupported("lfs_unsupported", "the repository holds Git LFS pointers")
		}
		total += int64(len(data))
		if total > lim.MaxTotalBytes || len(files) >= lim.MaxFiles {
			return "", unsupported("binary_too_large", "the state exceeds the tree limits")
		}
		d, err := l.PutObject(data)
		if err != nil {
			return "", err
		}
		files = append(files, File{Path: p, Mode: mode, Size: int64(len(data)), Digest: d})
	}
	return putTree(l, files)
}

var gitHash = regexp.MustCompile(`^[0-9a-f]{40}$`)

// OpenCodeSnapshotDirs returns the snapshot repositories of an OpenCode project:
// <data>/opencode/snapshot/<project>/*/ with a HEAD file. dataHome is XDG_DATA_HOME or
// ~/.local/share.
func OpenCodeSnapshotDirs(dataHome, project string) []string {
	if project == "" || strings.ContainsAny(project, `/\.`) {
		return nil
	}
	ms, _ := filepath.Glob(filepath.Join(dataHome, "opencode", "snapshot", project, "*", "HEAD"))
	var out []string
	for _, m := range ms {
		out = append(out, filepath.Dir(m))
	}
	sort.Strings(out)
	return out
}

// FromGitTree stores the Git tree object id of a bare repository (OpenCode snapshot
// repository) as a HINT tree. It reads with `git cat-file` and `git ls-tree` only and
// checks every blob against its Git hash.
func FromGitTree(ctx context.Context, l *library.Library, gitDirs []string, id string, lim Limits) (string, error) {
	lim = lim.filled()
	if !gitHash.MatchString(id) {
		return "", unsupported("baseline_missing", "not a tree id")
	}
	dir := ""
	for _, d := range gitDirs {
		if exec.CommandContext(ctx, "git", "--git-dir", d, "cat-file", "-e", id+"^{tree}").Run() == nil {
			dir = d
			break
		}
	}
	if dir == "" {
		return "", unsupported("baseline_missing", "the snapshot tree is no longer available")
	}
	ls, err := exec.CommandContext(ctx, "git", "--git-dir", dir, "ls-tree", "-r", "-z", "--full-tree", id).Output()
	if err != nil {
		return "", fmt.Errorf("git ls-tree failed")
	}
	type ent struct{ mode, hash, path string }
	var ents []ent
	for _, rec := range bytes.Split(ls, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 {
			return "", fmt.Errorf("unexpected git ls-tree output")
		}
		if f[1] == "commit" {
			return "", unsupported("submodule_unsupported", "the snapshot has a submodule")
		}
		if f[1] != "blob" || !validPath(string(p)) || !gitHash.MatchString(f[2]) {
			return "", unsupported("baseline_missing", "unexpected snapshot entry")
		}
		ents = append(ents, ent{f[0], f[2], string(p)})
	}
	if len(ents) > lim.MaxFiles {
		return "", unsupported("binary_too_large", "the state exceeds the tree limits")
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", dir, "cat-file", "--batch")
	var in bytes.Buffer
	for _, e := range ents {
		in.WriteString(e.hash + "\n")
	}
	cmd.Stdin = &in
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer cmd.Wait()
	r := bufio.NewReader(stdout)
	var files []File
	var total int64
	for _, e := range ents {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("git cat-file ended early")
		}
		f := strings.Fields(hdr)
		if len(f) != 3 || f[0] != e.hash || f[1] != "blob" {
			return "", unsupported("baseline_missing", "a snapshot object is missing")
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		if size > lim.MaxFileBytes {
			return "", unsupported("binary_too_large", "a file exceeds %d bytes", lim.MaxFileBytes)
		}
		total += size
		if total > lim.MaxTotalBytes {
			return "", unsupported("binary_too_large", "the state exceeds the tree limits")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return "", err
		}
		if _, err := r.ReadByte(); err != nil { // newline after the content
			return "", err
		}
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00", size)
		h.Write(data)
		if hex.EncodeToString(h.Sum(nil)) != e.hash {
			return "", fmt.Errorf("snapshot blob does not match its hash")
		}
		if bytes.HasPrefix(data, lfsPointer) {
			return "", unsupported("lfs_unsupported", "the snapshot holds Git LFS pointers")
		}
		mode := e.mode
		if mode != "100755" && mode != "120000" {
			mode = "100644"
		}
		d, err := l.PutObject(data)
		if err != nil {
			return "", err
		}
		files = append(files, File{Path: e.path, Mode: mode, Size: size, Digest: d})
	}
	return putTree(l, files)
}

// Materialize writes a tree into an empty directory. Paths are checked, no write passes
// through a link, and every symbolic link must resolve inside the directory.
func Materialize(l *library.Library, digest, dir string) error {
	t, err := Load(l, digest)
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(ents) != 0 {
		return fmt.Errorf("materialize needs an empty directory")
	}
	d, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	for _, f := range t.Files {
		if !validPath(f.Path) {
			return fmt.Errorf("unsafe path in tree")
		}
		data, err := l.GetObject(f.Digest)
		if err != nil {
			return err
		}
		switch f.Mode {
		case "120000":
			if err := d.Symlink(f.Path, string(data)); err != nil {
				return unsupported("baseline_missing", "a symbolic link points outside the repository")
			}
		default:
			perm := fs.FileMode(0o644)
			if f.Mode == "100755" {
				perm = 0o755
			}
			if err := d.WriteFile(f.Path, data, perm); err != nil {
				return fmt.Errorf("unsafe path in tree: %w", err)
			}
		}
	}
	if err := d.Check(); err != nil {
		return unsupported("baseline_missing", "a symbolic link points outside the repository")
	}
	return nil
}

// Diff lists the paths that differ between two trees (added, removed or changed).
func Diff(a, b Tree) (added, removed, changed []string) {
	am, bm := map[string]File{}, map[string]File{}
	for _, f := range a.Files {
		am[f.Path] = f
	}
	for _, f := range b.Files {
		bm[f.Path] = f
		if old, ok := am[f.Path]; !ok {
			added = append(added, f.Path)
		} else if old.Digest != f.Digest || old.Mode != f.Mode {
			changed = append(changed, f.Path)
		}
	}
	for p := range am {
		if _, ok := bm[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return
}
