package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

// queued is the metadata of a package that waits for a retry.
type queued struct {
	CreatedAt       string            `json:"created_at"`
	Files           []string          `json:"files"`
	Observations    []string          `json:"observations"`
	SnapshotDigests map[string]string `json:"snapshot_digests"`
	PolicyID        string            `json:"policy_id"`
}

// ErrQueued means the package could not be sent and is kept for `recommend --retry`.
type ErrQueued struct {
	Dir string
	Err error
}

func (e *ErrQueued) Error() string {
	return fmt.Sprintf("%v; the package is kept in %s; run `git hint recommend --retry` later", e.Err, e.Dir)
}

func (e *ErrQueued) Unwrap() error { return e.Err }

// Send uploads every document of the package. A transport failure keeps the package in
// .hint/state/queue/ (retryable); a rejection by the server is an error with its code.
func Send(ctx context.Context, s *store.Store, c *api.Client, p *Package, now time.Time) error {
	for _, d := range p.Docs {
		rc, err := c.PostDocument(ctx, d.Raw, d.ID)
		if errors.Is(err, api.ErrNetwork) {
			dir, qerr := enqueue(s, p, now)
			if qerr != nil {
				return fmt.Errorf("%v; queueing failed: %v", err, qerr)
			}
			return &ErrQueued{Dir: dir, Err: err}
		}
		if err != nil {
			return err
		}
		if !rc.Accepted {
			return fmt.Errorf("server rejected %s: %s", d.Type, rc.Code)
		}
	}
	return MarkExported(s, p, "api", c.Origin(), now)
}

func enqueue(s *store.Store, p *Package, now time.Time) (string, error) {
	id := hint.DeriveID(s.Key(), "queue", now.UTC().Format(time.RFC3339Nano))
	q := queued{CreatedAt: now.UTC().Format(time.RFC3339), Observations: p.Observations,
		SnapshotDigests: p.SnapshotDigests, PolicyID: p.Report.PolicyID}
	for _, d := range p.Docs {
		if err := s.WriteState(d.Raw, "state", "queue", id, d.File); err != nil {
			return "", err
		}
		q.Files = append(q.Files, d.File)
	}
	if err := s.WriteJSON(q, "state", "queue", id, "queue.json"); err != nil {
		return "", err
	}
	return s.Path("state", "queue", id), nil
}

// Queued lists the queued package directories, oldest first.
func Queued(s *store.Store) ([]string, error) {
	ents, err := os.ReadDir(s.Path("state", "queue"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type item struct {
		dir string
		at  string
	}
	var items []item
	for _, e := range ents {
		var q queued
		if !e.IsDir() || s.ReadJSON(&q, "state", "queue", e.Name(), "queue.json") != nil {
			continue
		}
		items = append(items, item{e.Name(), q.CreatedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at < items[j].at })
	var out []string
	for _, it := range items {
		out = append(out, it.dir)
	}
	return out, nil
}

// LoadQueued reads a queued package. The documents are validated again before sending.
func LoadQueued(s *store.Store, id string) (*Package, error) {
	var q queued
	if err := s.ReadJSON(&q, "state", "queue", id, "queue.json"); err != nil {
		return nil, err
	}
	p := &Package{Observations: q.Observations, SnapshotDigests: q.SnapshotDigests, Report: Report{PolicyID: q.PolicyID}}
	for _, f := range q.Files {
		raw, err := os.ReadFile(s.Path("state", "queue", id, f))
		if err != nil {
			return nil, err
		}
		if err := Validate(raw, nil); err != nil {
			return nil, fmt.Errorf("queued %s: %w", f, err)
		}
		var head struct {
			Type   string `json:"document_type"`
			ID     string `json:"document_id"`
			Digest string `json:"content_digest"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, err
		}
		p.Docs = append(p.Docs, Doc{Type: head.Type, ID: head.ID, Digest: head.Digest, File: f, Raw: raw})
	}
	return p, nil
}

// DropQueued removes a sent package from the queue.
func DropQueued(s *store.Store, id string) error {
	return os.RemoveAll(filepath.Join(s.Path("state", "queue"), id))
}
