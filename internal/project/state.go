// Package project implements the client workflows on top of the local store: collect,
// export, recommendation import and plan rendering.
package project

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
)

// Spool and source files below .hint/state/activity/.
const (
	spoolFile   = "spool.jsonl"
	sourcesFile = "sources.json"
)

type latestRef struct {
	SnapshotID string `json:"snapshot_id"`
}

// LoadLatest returns the latest stored snapshot of a repository, or nil.
func LoadLatest(s *store.Store, repoID string) (*snapshot.Local, error) {
	var ref latestRef
	if err := s.ReadJSON(&ref, "state", "snapshots", repoID, "latest.json"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return LoadSnapshot(s, repoID, ref.SnapshotID)
}

// LoadSnapshot reads one stored snapshot.
func LoadSnapshot(s *store.Store, repoID, snapID string) (*snapshot.Local, error) {
	var l snapshot.Local
	if err := s.ReadJSON(&l, "state", "snapshots", repoID, snapID+".json"); err != nil {
		return nil, err
	}
	// restore the local fields that the exported form does not carry
	for i := range l.Entities {
		ix := l.Index[l.Entities[i].ID]
		l.Entities[i].Path, l.Entities[i].Name, l.Entities[i].Line, l.Entities[i].Hash = ix.Path, ix.Name, ix.Line, ix.Hash
	}
	return &l, nil
}

func saveSnapshot(s *store.Store, l *snapshot.Local) error {
	repo := l.Record.RepoID
	if err := s.WriteJSON(l, "state", "snapshots", repo, l.Record.SnapshotID+".json"); err != nil {
		return err
	}
	return s.WriteJSON(latestRef{l.Record.SnapshotID}, "state", "snapshots", repo, "latest.json")
}

// AppendObservation adds one observation to the spool.
func AppendObservation(s *store.Store, o activity.Observation) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return s.AppendLine(b, "state", "activity", spoolFile)
}

// OpenSpool opens the activity spool for a long-running writer (the agent adapter).
func OpenSpool(s *store.Store) (*store.Appender, error) {
	return s.OpenAppend("state", "activity", spoolFile)
}

// ReadSpool returns all spooled observations and the number of unreadable lines.
func ReadSpool(s *store.Store) ([]activity.Observation, int, error) {
	f, err := os.Open(s.Path("state", "activity", spoolFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var out []activity.Observation
	bad := 0
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var o activity.Observation
		if json.Unmarshal(sc.Bytes(), &o) != nil || o.ObservationID == "" {
			bad++
			continue
		}
		if seen[o.ObservationID] {
			continue
		}
		seen[o.ObservationID] = true
		out = append(out, o)
	}
	return out, bad, sc.Err()
}

// Sources returns the registered sources by ID.
func Sources(s *store.Store) (map[string]activity.Source, error) {
	m := map[string]activity.Source{}
	if err := s.ReadJSON(&m, "state", "activity", sourcesFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

// RegisterSource records a source entry once.
func RegisterSource(s *store.Store, src activity.Source) error {
	m, err := Sources(s)
	if err != nil {
		return err
	}
	if old, ok := m[src.SourceID]; ok && equalJSON(old, src) {
		return nil
	}
	m[src.SourceID] = src
	return s.WriteJSON(m, "state", "activity", sourcesFile)
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// exportState is .hint/state/exports/state.json: what was already exported.
type exportState struct {
	// Observations already exported (an export sends only new ones unless --all).
	Observations []string `json:"observations"`
	// SnapshotDigests maps a snapshot ID to the digests it had in exported documents
	// (a policy can change the exported form, and with it the digest).
	SnapshotDigests map[string][]string `json:"snapshot_digests"`
}

func loadExportState(s *store.Store) (*exportState, error) {
	st := &exportState{SnapshotDigests: map[string][]string{}}
	if err := s.ReadJSON(st, "state", "exports", "state.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if st.SnapshotDigests == nil {
		st.SnapshotDigests = map[string][]string{}
	}
	return st, nil
}

func (st *exportState) record(s *store.Store, obs []string, digests map[string]string) error {
	have := map[string]bool{}
	for _, o := range st.Observations {
		have[o] = true
	}
	for _, o := range obs {
		if !have[o] {
			have[o] = true
			st.Observations = append(st.Observations, o)
		}
	}
	sort.Strings(st.Observations)
	for snap, d := range digests {
		found := false
		for _, x := range st.SnapshotDigests[snap] {
			found = found || x == d
		}
		if !found {
			st.SnapshotDigests[snap] = append(st.SnapshotDigests[snap], d)
		}
	}
	return s.WriteJSON(st, "state", "exports", "state.json")
}
