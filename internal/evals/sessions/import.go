package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"

	"github.com/neprel/git-hint/internal/evals/library"
)

// IndexEntry is the bookkeeping of one imported session: which sources were seen and
// which normalized form is current. It holds no text.
type IndexEntry struct {
	Agent      string   `json:"agent"`
	Session    string   `json:"session"`
	Sources    []string `json:"sources"`    // every imported source digest, oldest first
	Normalized string   `json:"normalized"` // digest of the current normalized session
	Items      int      `json:"items"`
	Truncated  bool     `json:"truncated"`
}

// Outcome of one import.
type Outcome struct {
	Agent      string `json:"agent"`
	Session    string `json:"session"`
	State      string `json:"state"` // new, updated, unchanged
	Items      int    `json:"items"`
	Normalized string `json:"normalized"`
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Import stores a parsed session in the library: the source bytes, every referenced text
// and the normalized session, then the index entry (last, so an interrupted import is
// redone in full). Importing the same source again changes nothing. A later export of the
// same session (continuation) replaces the current normalized form; earlier sources stay.
func Import(l *library.Library, src []byte, r Result) (Outcome, error) {
	s := r.Session
	out := Outcome{Agent: s.Agent, Session: s.Session, Items: len(s.Items)}
	name := s.Agent + "/" + safeName.ReplaceAllString(s.Session, "_")
	if len(name) > 220 {
		return out, fmt.Errorf("%w: session ID too long", ErrFormat)
	}
	var idx IndexEntry
	err := l.Index(name, &idx)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		idx = IndexEntry{Agent: s.Agent, Session: s.Session}
	case err != nil:
		return out, err
	}
	if slices.Contains(idx.Sources, s.Source) {
		out.State, out.Normalized = "unchanged", idx.Normalized
		return out, nil
	}
	if _, err := l.PutObject(src); err != nil {
		return out, err
	}
	for _, t := range r.Texts {
		if _, err := l.PutObject(t); err != nil {
			return out, err
		}
	}
	norm, err := json.Marshal(s)
	if err != nil {
		return out, err
	}
	nd, err := l.PutObject(norm)
	if err != nil {
		return out, err
	}
	out.State = "new"
	if len(idx.Sources) > 0 {
		out.State = "updated"
	}
	// a shorter or truncated later copy (rotation, a crash) never replaces a longer form
	if idx.Normalized == "" || len(s.Items) >= idx.Items || idx.Truncated && !s.Truncated {
		idx.Normalized, idx.Items, idx.Truncated = nd, len(s.Items), s.Truncated
	}
	idx.Sources = append(idx.Sources, s.Source)
	out.Normalized = idx.Normalized
	return out, l.PutIndex(name, idx)
}

// Load returns the current normalized session of an index entry.
func Load(l *library.Library, idx IndexEntry) (Session, error) {
	var s Session
	b, err := l.GetObject(idx.Normalized)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
