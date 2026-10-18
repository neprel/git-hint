// Package store manages the local HINT store `.hint/` of a project.
//
// Layout (in the root of the primary repository):
//
//	.hint/cache/   rebuildable data (scan cache); removed by `git hint cache clean`
//	.hint/state/   project configuration, snapshots, identity mapping, activity spool,
//	               export history, queued packages, imported recommendations
//
// The identity key is not in the working tree. It lives in the user configuration
// directory (or HINT_KEY_DIR) and `.hint/state/project.json` names it by a random key ID.
// Coding agents read the working tree and can send it to a model provider; the key can
// reverse exported identifiers, the mapping only holds names that the agent already sees.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/neprel/git-hint/internal/evals"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// Dir is the store directory name in the project root.
const Dir = ".hint"

// FormatVersion is the version of the local store layout.
const FormatVersion = 1

// Repo is one repository of the project.
type Repo struct {
	Name string `json:"name"` // local name; input of the repository ID, never exported
	Path string `json:"path"` // relative to the project root, or absolute
}

// Policy is the export allowlist (privacy-v0.1 §5).
type Policy struct {
	Profile               string              `json:"profile"` // detailed, coarse, aggregate
	TimeResolutionSeconds int                 `json:"time_resolution_seconds"`
	RecordTypes           []string            `json:"record_types"`
	Fields                map[string][]string `json:"fields,omitempty"` // empty: every field of the record type
}

// Config is `.hint/state/project.json`.
type Config struct {
	FormatVersion    int    `json:"format_version"`
	KeyID            string `json:"key_id"`
	ProjectNamespace string `json:"project_namespace"`
	Repos            []Repo `json:"repos"`
	ServerURL        string `json:"server_url,omitempty"`
	Policy           Policy `json:"policy"`
	MCP              MCP    `json:"mcp"`
	// Evals is the local eval policy; nil (the default) collects no eval evidence.
	Evals *evals.Policy `json:"evals,omitempty"`
	// Context is the local context policy of hint_context; nil: none (the tool's defaults).
	Context *ContextPolicy `json:"context,omitempty"`
}

// MCP holds the local rules of the MCP server. Only the user changes them (CLI or the
// file); no MCP tool can.
type MCP struct {
	// Upload is "deny" (default) or "allow": whether the hint_recommend tool may send the
	// export package to the configured server. The agent's request alone never sends data.
	Upload string `json:"upload,omitempty"`
	// EvalRun is "deny" (default) or "allow": whether the hint_eval_run tool may run eval
	// trials. It applies only on top of the eval policy's own permissions.
	EvalRun string `json:"eval_run,omitempty"`
}

// MCPUploadAllowed reports the local MCP upload rule (deny by default).
func (c Config) MCPUploadAllowed() bool { return c.MCP.Upload == "allow" }

// MCPEvalRunAllowed reports the local MCP eval run rule (deny by default).
func (c Config) MCPEvalRunAllowed() bool { return c.MCP.EvalRun == "allow" }

// Store is an opened project store.
type Store struct {
	Root string // project root (primary repository root)
	Cfg  Config
	key  []byte
}

// DefaultPolicy exports repository structure, changes and activity with every field.
func DefaultPolicy() Policy {
	return Policy{Profile: "detailed", TimeResolutionSeconds: 0,
		RecordTypes: []string{"Snapshot", "Entity", "Relation", "PolicyConstraint", "Change", "Observation", "Metric"}}
}

// Path joins elements below `.hint/`.
func (s *Store) Path(elem ...string) string {
	return filepath.Join(append([]string{s.Root, Dir}, elem...)...)
}

// Key returns the project HMAC key.
func (s *Store) Key() []byte { return s.key }

// RepoRoot returns the absolute root of a configured repository.
func (s *Store) RepoRoot(r Repo) string {
	if filepath.IsAbs(r.Path) {
		return r.Path
	}
	return filepath.Join(s.Root, r.Path)
}

// RepoID returns the opaque repository ID.
func (s *Store) RepoID(r Repo) string { return hint.RepoID(s.key, r.Name) }

// KeyDir is the directory of project keys: HINT_KEY_DIR, else <user config>/git-hint/keys.
func KeyDir() (string, error) {
	if d := os.Getenv("HINT_KEY_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "git-hint", "keys"), nil
}

func keyPath(id string) (string, error) {
	d, err := KeyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".key"), nil
}

// GitTopLevel returns the root of the Git work tree that contains dir.
func GitTopLevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a Git work tree", dir)
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

// InitOptions are the inputs of `git hint init`.
type InitOptions struct {
	Repos      []Repo // additional repositories; the primary repository is always added
	ServerURL  string
	MCPUpload  string // "", "allow" or "deny"
	MCPEvalRun string // "", "allow" or "deny"
	// ContextLimits changes the local context policy: "" (no change), "off", "restore=N" or
	// "depth=1,items=20,bytes=16384,ms=1000".
	ContextLimits string
	Now           time.Time
}

// Init creates the store in the Git work tree that contains dir. It is idempotent for an
// existing store: it adds new repositories and keeps the key.
func Init(dir string, opt InitOptions) (*Store, bool, error) {
	root, err := GitTopLevel(dir)
	if err != nil {
		return nil, false, err
	}
	if err := ensureExcludeEntry(root); err != nil {
		return nil, false, err
	}
	s := &Store{Root: root}
	created := false
	if b, err := os.ReadFile(s.Path("state", "project.json")); err == nil {
		if err := json.Unmarshal(b, &s.Cfg); err != nil {
			return nil, false, fmt.Errorf("project.json: %w", err)
		}
	} else {
		created = true
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return nil, false, err
		}
		s.Cfg = Config{FormatVersion: FormatVersion, KeyID: hex.EncodeToString(id), Policy: DefaultPolicy(),
			Repos: []Repo{{Name: filepath.Base(root), Path: "."}}}
	}
	for _, r := range opt.Repos {
		if err := s.addRepo(r); err != nil {
			return nil, false, err
		}
	}
	// the key must stay outside every repository of the project, also the new ones
	if err := s.checkKeyLocation(); err != nil {
		return nil, false, err
	}
	if created {
		if s.key, err = newKey(s.Cfg.KeyID); err != nil {
			return nil, false, err
		}
		s.Cfg.ProjectNamespace = hint.NamespaceID(s.key)
	} else if s.key, err = readKey(s.Cfg.KeyID); err != nil {
		return nil, false, err
	}
	if opt.ServerURL != "" {
		s.Cfg.ServerURL = opt.ServerURL
	}
	switch opt.MCPUpload {
	case "":
	case "allow", "deny":
		s.Cfg.MCP.Upload = opt.MCPUpload
	default:
		return nil, false, fmt.Errorf("mcp upload rule %q: use allow or deny", opt.MCPUpload)
	}
	switch opt.MCPEvalRun {
	case "":
	case "allow", "deny":
		s.Cfg.MCP.EvalRun = opt.MCPEvalRun
	default:
		return nil, false, fmt.Errorf("mcp eval run rule %q: use allow or deny", opt.MCPEvalRun)
	}
	if opt.ContextLimits != "" {
		now := opt.Now
		if now.IsZero() {
			now = time.Now()
		}
		if err := s.Cfg.applyContextLimits(opt.ContextLimits, now); err != nil {
			return nil, false, err
		}
	}
	if err := s.SaveConfig(); err != nil {
		return nil, false, err
	}
	return s, created, nil
}

func (s *Store) addRepo(r Repo) error {
	abs := r.Path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.Root, abs)
	}
	top, err := GitTopLevel(abs)
	if err != nil {
		return err
	}
	if r.Name == "" {
		r.Name = filepath.Base(top)
	}
	for _, x := range s.Cfg.Repos {
		if filepath.Clean(s.RepoRoot(x)) == top {
			return nil // already configured
		}
		if x.Name == r.Name {
			return fmt.Errorf("repository name %q is already used; choose another name with --name", r.Name)
		}
	}
	if rel, err := filepath.Rel(s.Root, top); err == nil {
		r.Path = rel
	} else {
		r.Path = top
	}
	s.Cfg.Repos = append(s.Cfg.Repos, r)
	return nil
}

// Open finds the store from dir upwards and loads it.
func Open(dir string) (*Store, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		p := filepath.Join(d, Dir, "state", "project.json")
		if b, err := os.ReadFile(p); err == nil {
			s := &Store{Root: d}
			if err := json.Unmarshal(b, &s.Cfg); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if s.Cfg.FormatVersion != FormatVersion {
				return nil, fmt.Errorf("store format %d is not supported (want %d)", s.Cfg.FormatVersion, FormatVersion)
			}
			if err := s.checkKeyLocation(); err != nil {
				return nil, err
			}
			if s.key, err = readKey(s.Cfg.KeyID); err != nil {
				return nil, err
			}
			return s, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return nil, errors.New("no HINT project here; run `git hint init` in the repository root")
		}
		d = parent
	}
}

func newKey(id string) ([]byte, error) {
	p, err := keyPath(id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(k); err != nil {
		return nil, err
	}
	return k, f.Sync()
}

func readKey(id string) ([]byte, error) {
	p, err := keyPath(id)
	if err != nil {
		return nil, err
	}
	k, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("project key %s not found (%v); the key is not in the working tree, see `git hint doctor`", id, err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("project key %s has %d bytes, want 32", p, len(k))
	}
	return k, nil
}

// ErrKeyInWorkTree is returned when the key directory or key file lies inside a
// repository of the project.
var ErrKeyInWorkTree = errors.New("the project key must not be inside a repository of the project")

// resolve returns the absolute, symlink-free form of p. For a path that does not exist
// yet, the longest existing prefix is resolved and the rest is appended.
func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	rest := ""
	cur := abs
	for {
		if ev, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(ev, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// checkKeyLocation rejects a key directory or key file inside any repository of the
// project. Relative paths (HINT_KEY_DIR) and symbolic links are resolved first.
func (s *Store) checkKeyLocation() error {
	dir, err := KeyDir()
	if err != nil {
		return err
	}
	kf, _ := keyPath(s.Cfg.KeyID)
	cands := []string{resolve(dir), resolve(kf), filepath.Clean(dir), filepath.Clean(kf)}
	for _, r := range s.Cfg.Repos {
		roots := []string{filepath.Clean(s.RepoRoot(r)), resolve(s.RepoRoot(r))}
		for _, root := range roots {
			for _, c := range cands {
				if filepath.IsAbs(c) && within(root, c) {
					return fmt.Errorf("%w: %s is inside %s; set HINT_KEY_DIR to a directory outside the repositories", ErrKeyInWorkTree, c, root)
				}
			}
		}
	}
	return nil
}

// KeyFile returns the path of the project key file.
func (s *Store) KeyFile() (string, error) { return keyPath(s.Cfg.KeyID) }

// SaveConfig writes project.json.
func (s *Store) SaveConfig() error {
	b, err := json.MarshalIndent(s.Cfg, "", "  ")
	if err != nil {
		return err
	}
	return s.WriteState(append(b, '\n'), "state", "project.json")
}

// ---------------------------------------------------------------------------
// Git exclusion: `.hint/` must never be committed.
// ---------------------------------------------------------------------------

// probePaths are representative paths of every area that the client writes.
var probePaths = []string{
	"state/project.json", "state/snapshots/r/s.json", "state/snapshots/r/latest.json",
	"state/activity/spool.jsonl", "state/activity/sources.json", "state/activity/adapter-stats.json",
	"state/exports/state.json", "state/exports/history.jsonl", "state/exports/t/d.hint.json",
	"state/queue/q/queue.json", "state/recommendations/latest.json", "state/recommendations/p.md",
	"state/lock", "state/mcp.log",
	"cache/scan-r.json",
}

// ensureExcludeEntry adds `/.hint/` to .git/info/exclude unless Git already ignores every
// area of the store.
func ensureExcludeEntry(root string) error {
	if checkPaths(root, storePaths(root, probePaths)...) == nil {
		return nil
	}
	if err := tracked(root); err != nil {
		return err
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return fmt.Errorf("git: %w", err)
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	old, _ := os.ReadFile(p)
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		old = append(old, '\n')
	}
	if err := os.WriteFile(p, append(old, []byte("# HINT local store\n/"+Dir+"/\n")...), 0o644); err != nil {
		return err
	}
	return checkPaths(root, storePaths(root, probePaths)...)
}

func storePaths(root string, rel []string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = filepath.Join(root, Dir, filepath.FromSlash(r))
	}
	return out
}

// ErrNotExcluded is returned when a write would put `.hint/` at risk of a commit.
var ErrNotExcluded = errors.New(".hint/ is not ignored by Git; run `git hint init` again or add /.hint/ to .gitignore")

// tracked fails when Git already tracks a file below `.hint/`: ignore rules do not apply
// to tracked files, so their content would reach the next commit.
func tracked(root string) error {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", Dir).Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %w", err)
	}
	if len(bytes.Trim(out, "\x00")) > 0 {
		first := strings.SplitN(string(out), "\x00", 2)[0]
		return fmt.Errorf("%w: Git tracks %s; remove it from the index (git rm --cached -r .hint)", ErrNotExcluded, first)
	}
	return nil
}

// checkPaths fails unless no file below `.hint/` is tracked and Git ignores every given
// path (absolute paths below the root).
func checkPaths(root string, paths ...string) error {
	if err := tracked(root); err != nil {
		return err
	}
	args := []string{"-C", root, "check-ignore", "--no-index", "-v", "-n", "--"}
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil || !strings.HasPrefix(filepath.ToSlash(rel), Dir+"/") {
			return fmt.Errorf("%w: %s is not below %s", ErrNotExcluded, p, Dir)
		}
		args = append(args, filepath.ToSlash(rel))
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil && len(out) == 0 {
		return fmt.Errorf("git check-ignore: %w", err)
	}
	// -v -n prints "<source>:<line>:<pattern>\t<path>" per path; "::\t<path>" when no
	// pattern matches. A matching negated pattern ("!...") means the path is NOT ignored.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(paths) {
		return fmt.Errorf("%w: unexpected git check-ignore output", ErrNotExcluded)
	}
	for _, line := range lines {
		left, path, _ := strings.Cut(line, "\t")
		m := ignoreLine.FindStringSubmatch(left)
		if m == nil || strings.HasPrefix(m[1], "!") {
			return fmt.Errorf("%w (%s)", ErrNotExcluded, path)
		}
	}
	return nil
}

var ignoreLine = regexp.MustCompile(`^.*:[0-9]+:(.+)$`)

// CheckExcluded fails closed unless Git ignores every area of the store and tracks no
// file below it.
func (s *Store) CheckExcluded() error {
	return checkPaths(s.Root, storePaths(s.Root, probePaths)...)
}

// CheckPath fails closed unless Git ignores this path below `.hint/`, every other area of
// the store, and tracks no file below the store. Every write calls it with its actual
// path; a partly ignored store stops all writes.
func (s *Store) CheckPath(p string) error {
	return checkPaths(s.Root, append(storePaths(s.Root, probePaths), p)...)
}

// WriteState writes a file below `.hint/` atomically with owner-only permissions. It
// checks the Git exclusion first.
func (s *Store) WriteState(data []byte, elem ...string) error {
	p := s.Path(elem...)
	if err := s.CheckPath(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// WriteJSON writes v as indented JSON below `.hint/`.
func (s *Store) WriteJSON(v any, elem ...string) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.WriteState(append(b, '\n'), elem...)
}

// ReadJSON reads a JSON file below `.hint/`.
func (s *Store) ReadJSON(v any, elem ...string) error {
	b, err := os.ReadFile(s.Path(elem...))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// AppendLine appends one line to a file below `.hint/` (owner-only permissions).
func (s *Store) AppendLine(line []byte, elem ...string) error {
	p := s.Path(elem...)
	if err := s.CheckPath(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Appender appends lines to one file below `.hint/` (owner-only permissions). It checks
// the Git exclusion before every line, because the rules or the index can change during a
// long session. After the first failed check it stops for good: no later line is written.
type Appender struct {
	f       *os.File
	s       *Store
	path    string
	stopped error
}

// OpenAppend opens a file below `.hint/` for appending.
func (s *Store) OpenAppend(elem ...string) (*Appender, error) {
	p := s.Path(elem...)
	if err := s.CheckPath(p); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Appender{f: f, s: s, path: p}, nil
}

// Append checks the exclusion, then writes one line in one write call (complete lines
// only; no partial record). An exclusion error stops the appender permanently.
func (a *Appender) Append(line []byte) error {
	if a.stopped != nil {
		return a.stopped
	}
	if err := a.s.CheckPath(a.path); err != nil {
		a.stopped = err
		a.f.Close()
		return err
	}
	_, err := a.f.Write(append(line, '\n'))
	return err
}

// Stopped returns the exclusion error that stopped the appender, or nil.
func (a *Appender) Stopped() error { return a.stopped }

// Close closes the file.
func (a *Appender) Close() error {
	if a.stopped != nil {
		return nil // already closed
	}
	return a.f.Close()
}

// CleanCache removes `.hint/cache/` only. Mapping, keys and state stay.
func (s *Store) CleanCache() error {
	return os.RemoveAll(s.Path("cache"))
}
