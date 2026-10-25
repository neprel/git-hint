// Package project implements the client workflows on top of the local store: collect,
// export, recommendation import and plan rendering.
package project

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"time"

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
	restoreLocalFields(&l)
	return &l, nil
}

// restoreLocalFields restores the local entity fields that the exported form does not carry.
func restoreLocalFields(l *snapshot.Local) {
	for i := range l.Entities {
		ix := l.Index[l.Entities[i].ID]
		l.Entities[i].Path, l.Entities[i].Name, l.Entities[i].Line, l.Entities[i].Hash = ix.Path, ix.Name, ix.Line, ix.Hash
	}
}

// ErrSnapshotConflict reports a stored snapshot with the same ID but other build inputs.
var ErrSnapshotConflict = errors.New("a different snapshot is already stored under this snapshot ID")

// snapshotMetadataAllowance bounds the stored copy of a same-revision snapshot: only the
// capture time (fixed length), the digest (fixed length) and the scan-effort counters may
// differ from the fresh build.
const snapshotMetadataAllowance = 4096

// saveSnapshot stores a new snapshot once and then moves the latest pointer to it. It uses
// the confined no-follow create-once primitive, so a stored snapshot is never overwritten.
// Equal stored bytes are idempotent. A stored record that is a valid earlier build of the
// same scan result (for example before a crash ahead of the latest pointer) is returned
// unchanged. Any other stored record or path refuses before the latest pointer changes.
func saveSnapshot(s *store.Store, l *snapshot.Local) (*snapshot.Local, error) {
	repo, id := l.Record.RepoID, l.Record.SnapshotID
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	ctx, elem := context.Background(), []string{"state", "snapshots", repo, id + ".json"}
	limit := int64(len(b)) + snapshotMetadataAllowance
	err = s.WriteStateImmutableContext(ctx, b, limit, elem...)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		stored, err := s.ReadStateBounded(ctx, limit, elem...)
		if err != nil {
			return nil, errors.Join(ErrSnapshotConflict, err)
		}
		if !bytes.Equal(stored, b) {
			old, ok := sameRevision(stored, l)
			if !ok {
				return nil, ErrSnapshotConflict
			}
			l = old
		}
	default:
		return nil, err
	}
	if err := s.WriteJSON(latestRef{id}, "state", "snapshots", repo, "latest.json"); err != nil {
		return nil, err
	}
	return l, nil
}

// sameRevision returns the stored record when it is a valid earlier build of exactly the
// same scan result: its capture time is a valid UTC second, its digest matches its records,
// and after replacing only the capture time, the digest and the scan-effort counters
// (parsed, reused) with the fresh values, the whole stored record equals the fresh build,
// including the private index and the ambiguity map.
func sameRevision(stored []byte, cur *snapshot.Local) (*snapshot.Local, bool) {
	var old snapshot.Local
	if json.Unmarshal(stored, &old) != nil {
		return nil, false
	}
	at, err := time.Parse(time.RFC3339, old.Record.CapturedAt)
	if err != nil || at.IsZero() || at.UTC().Format(time.RFC3339) != old.Record.CapturedAt {
		return nil, false
	}
	if d, err := snapshot.Digest(old.Record, old.Entities, old.Relations); err != nil || d != old.Record.Digest {
		return nil, false
	}
	norm := old
	norm.Record.CapturedAt, norm.Record.Digest = cur.Record.CapturedAt, cur.Record.Digest
	norm.Stats.Parsed, norm.Stats.Reused = cur.Stats.Parsed, cur.Stats.Reused
	got, err1 := json.Marshal(norm)
	want, err2 := json.Marshal(cur)
	if err1 != nil || err2 != nil || !bytes.Equal(got, want) {
		return nil, false
	}
	restoreLocalFields(&old)
	return &old, true
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

// SpoolDamage is what the spool reader could not take as evidence: unreadable lines and
// observation IDs recorded with two different bodies. A conflicting ID keeps no version (no
// first or last winner); both are unknown loss for every consumer (C7BE-21).
type SpoolDamage struct {
	Unreadable int
	Conflicts  []string
	// Withheld holds the first version of each conflicting ID (its scope only; never exported)
	Withheld []activity.Observation
}

// Any reports any damage.
func (d SpoolDamage) Any() bool { return d.Unreadable > 0 || len(d.Conflicts) > 0 }

// ReadSpoolChecked returns the spooled observations in order, identical replays collapsed,
// with the damage of the spool.
func ReadSpoolChecked(s *store.Store) ([]activity.Observation, SpoolDamage, error) {
	var d SpoolDamage
	batchObs, _, err := readActivityBatches(s)
	if err != nil {
		return nil, d, err
	}
	var input io.Reader = bytes.NewReader(nil)
	f, err := os.Open(s.Path("state", "activity", spoolFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, d, err
	}
	if err == nil {
		defer f.Close()
		input = f
	}
	var batches bytes.Buffer
	if f != nil && len(batchObs) > 0 {
		if info, e := f.Stat(); e == nil && info.Size() > 0 {
			var last [1]byte
			if _, e := f.ReadAt(last[:], info.Size()-1); e != nil {
				return nil, d, e
			}
			if last[0] != '\n' {
				batches.WriteByte('\n')
			}
		}
	}
	for _, o := range batchObs {
		raw, _ := json.Marshal(o)
		batches.Write(raw)
		batches.WriteByte('\n')
	}
	input = io.MultiReader(input, &batches)
	return parseSpoolInput(input)
}

// parseSpoolInput shares the ordinary replay/conflict parser with bounded input readers.
func parseSpoolInput(input io.Reader) ([]activity.Observation, SpoolDamage, error) {
	var d SpoolDamage
	var order []string
	first := map[string]activity.Observation{}
	body := map[string]string{}
	conflict := map[string]bool{}
	sc := bufio.NewScanner(input)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var o activity.Observation
		if json.Unmarshal(sc.Bytes(), &o) != nil || o.ObservationID == "" {
			d.Unreadable++
			continue
		}
		canon, _ := json.Marshal(o)
		if b, ok := body[o.ObservationID]; ok {
			if b != string(canon) && !conflict[o.ObservationID] {
				conflict[o.ObservationID] = true
				d.Conflicts = append(d.Conflicts, o.ObservationID)
				d.Withheld = append(d.Withheld, first[o.ObservationID])
			}
			continue
		}
		body[o.ObservationID] = string(canon)
		first[o.ObservationID] = o
		order = append(order, o.ObservationID)
	}
	var out []activity.Observation
	for _, id := range order {
		if !conflict[id] {
			out = append(out, first[id])
		}
	}
	return out, d, sc.Err()
}

// ReadSpool returns the spooled observations (identical replays collapsed; conflicting IDs
// withheld) and the number of damaged entries (unreadable lines plus conflicting IDs).
func ReadSpool(s *store.Store) ([]activity.Observation, int, error) {
	obs, d, err := ReadSpoolChecked(s)
	return obs, d.Unreadable + len(d.Conflicts), err
}

// Sources returns the registered sources by ID.
func Sources(s *store.Store) (map[string]activity.Source, error) {
	m := map[string]activity.Source{}
	if err := s.ReadJSON(&m, "state", "activity", sourcesFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_, batches, err := readActivityBatches(s)
	if err != nil {
		return nil, err
	}
	for id, src := range batches {
		if old, ok := m[id]; ok && !equalJSON(old, src) {
			return nil, ErrActivityBatch
		}
		m[id] = src
	}
	return m, nil
}

// RegisterSource records a source entry once.
func RegisterSource(s *store.Store, src activity.Source) error {
	m, err := Sources(s)
	if err != nil {
		return err
	}
	if old, ok := m[src.SourceID]; ok {
		if equalJSON(old, src) {
			return nil
		}
		_, batches, err := readActivityBatches(s)
		if err != nil {
			return err
		}
		if _, immutable := batches[src.SourceID]; immutable {
			return ErrActivityBatch
		}
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

// SessionTasks returns the declared-task evidence of the OpenCode adapter's agent sessions:
// session pseudonym -> task ID -> observations ("" counts observations without a task), and
// the adapter's source IDs. Only records of the OpenCode adapter count.
func SessionTasks(s *store.Store) (map[string]map[string]int, []string, error) {
	srcs, err := Sources(s)
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	for id, src := range srcs {
		if src.Adapter.Name == "opencode-plugin" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	obs, _, err := ReadSpool(s)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]map[string]int{}
	for _, o := range obs {
		if src, ok := srcs[o.SourceID]; !ok || src.Adapter.Name != "opencode-plugin" {
			continue
		}
		sess := o.Correlation["session_id"]
		if sess == "" {
			continue
		}
		if out[sess] == nil {
			out[sess] = map[string]int{}
		}
		out[sess][o.Correlation["task_id"]]++
	}
	return out, ids, nil
}
