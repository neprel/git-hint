// Package library is the durable local store of eval evidence: content-addressed objects,
// immutable manifests, the job journal, pins and the inventory of runner artifacts.
//
// It lives in `.hint/state/evals/` inside the Git-excluded project store, with owner-only
// permissions. Every write checks the Git exclusion first and refuses paths that a symbolic
// link would move outside the store. Rebuildable data belongs in `.hint/cache/evals/`;
// cache cleanup never touches this library.
package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/store"
)

// Errors of the library.
var (
	ErrQuota     = errors.New("the eval library quota is full")
	ErrCorrupt   = errors.New("stored object does not match its digest")
	ErrMissing   = errors.New("stored object is unavailable")
	ErrImmutable = errors.New("a different record with this ID is already stored")
	ErrEscape    = errors.New("the eval library path leaves the project store")
)

// Library is the eval evidence store of one project.
type Library struct {
	s     *store.Store
	quota int64
	mu    sync.Mutex
}

// Open returns the library of the project store with the quota of the policy.
func Open(s *store.Store, quotaBytes int64) *Library {
	if quotaBytes <= 0 {
		quotaBytes = int64(evals.DefaultQuotaMB) << 20
	}
	return &Library{s: s, quota: quotaBytes}
}

func (l *Library) path(elem ...string) string {
	return l.s.Path(append([]string{"state", "evals"}, elem...)...)
}

// checkTarget refuses a path whose existing part resolves outside `.hint/` (a symbolic
// link planted inside the store) and checks the Git exclusion of the path.
func (l *Library) checkTarget(p string) error {
	root, err := filepath.EvalSymlinks(l.s.Path())
	if err != nil {
		return err
	}
	cur := filepath.Dir(p)
	for {
		if ev, err := filepath.EvalSymlinks(cur); err == nil {
			rel, err := filepath.Rel(root, ev)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: %s", ErrEscape, cur)
			}
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("%w: %s", ErrEscape, p)
		}
		cur = parent
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symbolic link", ErrEscape, p)
	}
	return l.s.CheckPath(p)
}

// writeNew writes data to p atomically. With exclusive, an existing p is never replaced
// (os.Link fails); the caller decides whether the existing content is acceptable.
func (l *Library) writeNew(p string, data []byte, exclusive bool) error {
	if err := l.checkTarget(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
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
	if exclusive {
		return os.Link(name, p)
	}
	return os.Rename(name, p)
}

// Digest returns the content digest of data.
func Digest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

func (l *Library) objectPath(digest string) (string, error) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	return l.path("objects", h[:2], h[2:]), nil
}

// Usage returns the bytes stored in the library (temporary files included).
func (l *Library) Usage() (int64, error) {
	var n int64
	err := filepath.WalkDir(l.path(), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}

func (l *Library) reserve(n int) error {
	used, err := l.Usage()
	if err != nil {
		return err
	}
	if used+int64(n) > l.quota {
		return fmt.Errorf("%w (%d of %d bytes used)", ErrQuota, used, l.quota)
	}
	return nil
}

// PutObject stores data and returns its digest. Storing the same content again writes
// nothing; a damaged copy is replaced.
func (l *Library) PutObject(data []byte) (string, error) {
	d := Digest(data)
	p, err := l.objectPath(d)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if old, err := os.ReadFile(p); err == nil {
		if Digest(old) == d {
			return d, nil
		}
		if err := l.checkTarget(p); err != nil {
			return "", err
		}
		if err := os.Remove(p); err != nil {
			return "", err
		}
	}
	if err := l.reserve(len(data)); err != nil {
		return "", err
	}
	if err := l.writeNew(p, data, true); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return d, nil
}

// GetObject reads an object and checks its digest.
func (l *Library) GetObject(digest string) ([]byte, error) {
	p, err := l.objectPath(digest)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrMissing, digest)
	}
	if err != nil {
		return nil, err
	}
	if Digest(b) != digest {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, digest)
	}
	return b, nil
}

// manifestName is the file name of a record: <id>.json, or <id>@<version>.json for an
// eval version.
func manifestName(kind string, rec map[string]any) (string, error) {
	key := map[string]string{"episode": "episode_id", "eval": "eval_id", "suite": "suite_id", "trial": "trial_id"}[kind]
	if key == "" {
		return "", fmt.Errorf("record kind %q has no manifest", kind)
	}
	id, _ := rec[key].(string)
	if id == "" {
		return "", fmt.Errorf("%s record without %s", kind, key)
	}
	if kind == "eval" {
		v, _ := rec["version"].(float64)
		return fmt.Sprintf("%s@%d.json", id, int(v)), nil
	}
	return id + ".json", nil
}

// PutManifest validates and stores an immutable record. The same bytes again are
// accepted; other content under the same ID (or eval version) is ErrImmutable.
func (l *Library) PutManifest(kind string, record any) (string, error) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := evals.Validate(kind, data); err != nil {
		return "", err
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	name, err := manifestName(kind, m)
	if err != nil {
		return "", err
	}
	p := l.path("manifests", kind, name)
	l.mu.Lock()
	defer l.mu.Unlock()
	if old, err := os.ReadFile(p); err == nil {
		if bytes.Equal(old, data) {
			return strings.TrimSuffix(name, ".json"), nil
		}
		return "", fmt.Errorf("%w: %s %s", ErrImmutable, kind, name)
	}
	if err := l.reserve(len(data)); err != nil {
		return "", err
	}
	if err := l.writeNew(p, data, true); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		// another writer linked the file first (complete, never partial): same bytes are fine
		if old, rerr := os.ReadFile(p); rerr != nil || !bytes.Equal(old, data) {
			return "", fmt.Errorf("%w: %s %s", ErrImmutable, kind, name)
		}
	}
	return strings.TrimSuffix(name, ".json"), nil
}

// Manifest reads one record by kind and name (as returned by PutManifest).
func (l *Library) Manifest(kind, name string, v any) error {
	if strings.ContainsAny(name, `/\`) || name == "" || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid record name %q", name)
	}
	b, err := os.ReadFile(l.path("manifests", kind, name+".json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Manifests lists the record names of a kind, sorted. Temporary files are skipped.
func (l *Library) Manifests(kind string) ([]string, error) {
	ents, err := os.ReadDir(l.path("manifests", kind))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			out = append(out, strings.TrimSuffix(n, ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Recover removes temporary files that an interrupted write left behind. It never removes
// a complete object or manifest.
func (l *Library) Recover() (int, error) {
	n := 0
	err := filepath.WalkDir(l.path(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() && strings.HasPrefix(d.Name(), ".tmp-") {
			if err := os.Remove(p); err == nil {
				n++
			}
		}
		return nil
	})
	return n, err
}
