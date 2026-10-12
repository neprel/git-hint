package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// FileEntry is the analysis of one file. It is cached by path and content hash; a hit
// skips parsing (the file is still read and hashed).
type FileEntry struct {
	Size   int64    `json:"size"`
	ModNS  int64    `json:"mod_ns"`
	Hash   string   `json:"hash"`
	Lines  int      `json:"lines"`
	Binary bool     `json:"binary"`
	Flags  []string `json:"flags,omitempty"`
	Go     *GoInfo  `json:"go,omitempty"`
}

// CacheVersion changes when the analysis changes; an old cache is ignored.
const CacheVersion = "scan-0.1.1"

// Cache holds file analyses of one repository. It is rebuildable data (.hint/cache/).
type Cache struct {
	Version string                `json:"version"`
	Files   map[string]*FileEntry `json:"files"`
	next    map[string]*FileEntry
}

// LoadCache reads a cache file; a missing or stale file gives an empty cache.
func LoadCache(p string) *Cache {
	c := &Cache{Version: CacheVersion, Files: map[string]*FileEntry{}}
	b, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	var old Cache
	if json.Unmarshal(b, &old) != nil || old.Version != CacheVersion || old.Files == nil {
		return c
	}
	c.Files = old.Files
	return c
}

func (c *Cache) get(p, hash string) *FileEntry {
	fe := c.Files[p]
	if fe == nil || fe.Hash != hash {
		return nil
	}
	return fe
}

func (c *Cache) put(p string, fe *FileEntry) {
	if c.next == nil {
		c.next = map[string]*FileEntry{}
	}
	c.next[p] = fe
}

// Save writes the entries of the last scan only (removed files drop out).
func (c *Cache) Save(p string) error {
	out := Cache{Version: CacheVersion, Files: c.next}
	if out.Files == nil {
		out.Files = map[string]*FileEntry{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
