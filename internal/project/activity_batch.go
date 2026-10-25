package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
)

const activityBatchDir = "batches"
const maxActivityBatchBytes = 64 << 20

var batchID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var ErrActivityBatch = errors.New("activity batch refused")

type activityBatch struct {
	Version      int                    `json:"version"`
	Digest       string                 `json:"digest"`
	Source       activity.Source        `json:"source"`
	Observations []activity.Observation `json:"observations"`
}

func activityBatchDigest(b activityBatch) (string, error) {
	return hint.Digest(map[string]any{"version": b.Version, "source": b.Source, "observations": b.Observations})
}

func validateActivityBatch(s *store.Store, b activityBatch) error {
	if b.Version != 1 || !batchID.MatchString(b.Source.SourceID) {
		return ErrActivityBatch
	}
	digest, err := activityBatchDigest(b)
	if err != nil || digest != b.Digest {
		return ErrActivityBatch
	}
	ids := map[string]bool{}
	for _, o := range b.Observations {
		if o.SourceID != b.Source.SourceID || ids[o.ObservationID] {
			return ErrActivityBatch
		}
		ids[o.ObservationID] = true
	}
	payload := map[string]any{"observations": b.Observations, "metrics": []any{}}
	pd, err := hint.Digest(payload)
	if err != nil {
		return ErrActivityBatch
	}
	repos := []string{}
	for _, r := range s.Cfg.Repos {
		repos = append(repos, s.RepoID(r))
	}
	sort.Strings(repos)
	doc := map[string]any{"hint_version": "0.2.0", "document_type": "activity_stream", "document_id": b.Source.SourceID,
		"project_namespace": s.Cfg.ProjectNamespace, "repositories": repos, "producer": map[string]any{"tool": "hint-cli", "version": "0.1.0"},
		"created_at": time.Now().UTC().Format(time.RFC3339), "sources": []activity.Source{b.Source},
		"redaction":      map[string]any{"policy_id": hint.DeriveID(s.Key(), "activity-batch-validation"), "profile": "detailed", "record_types": []string{"Observation"}, "free_text": "denied", "time_resolution_seconds": 0},
		"content_digest": pd, "payload": payload}
	raw, err := json.Marshal(doc)
	if err != nil {
		return ErrActivityBatch
	}
	decoded, err := hint.Decode(raw)
	if err != nil {
		return ErrActivityBatch
	}
	schemas, err := hint.LoadSchemas()
	if err != nil {
		return err
	}
	if schemas.Validate(decoded) != nil {
		return ErrActivityBatch
	}
	// the semantic checks read plain numbers, as in the export path
	plain, err := hint.Plain(decoded)
	if err != nil || len(hint.Check(plain, nil)) != 0 {
		return ErrActivityBatch
	}
	return nil
}

// WriteActivityBatch commits a source and all its minimized observations in one atomic file.
// Exact replay is idempotent. A different body for this source or any existing event refuses.
// Readers share the old activity lifecycle; no raw capture or partial source is stored here.
func WriteActivityBatch(s *store.Store, src activity.Source, obs []activity.Observation) error {
	lock, err := s.LockState(true)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return writeActivityBatchLocked(s, src, obs)
}

// writeActivityBatchLocked is WriteActivityBatch for a caller that holds the state lock (one
// lock owner; the state lock is not reentrant).
func writeActivityBatchLocked(s *store.Store, src activity.Source, obs []activity.Observation) error {
	raw, committed, err := planActivityBatch(s, src, obs)
	if err != nil || committed {
		return err
	}
	return s.WriteState(raw, "state", "activity", activityBatchDir, src.SourceID+".json")
}

// planActivityBatch runs every check of writeActivityBatchLocked without writing: it returns the
// batch bytes and whether the exact batch is committed already (a replay).
func planActivityBatch(s *store.Store, src activity.Source, obs []activity.Observation) ([]byte, bool, error) {
	b := activityBatch{Version: 1, Source: src, Observations: obs}
	if b.Observations == nil {
		b.Observations = []activity.Observation{}
	}
	var err error
	b.Digest, err = activityBatchDigest(b)
	if err != nil {
		return nil, false, ErrActivityBatch
	}
	if err = validateActivityBatch(s, b); err != nil {
		return nil, false, err
	}
	raw, err := json.Marshal(b)
	if err != nil || len(raw) > maxActivityBatchBytes {
		return nil, false, ErrActivityBatch
	}
	// Validate every existing batch before accepting a replay or creating another one.
	if _, _, err = readActivityBatches(s); err != nil {
		return nil, false, err
	}
	name := src.SourceID + ".json"
	var old activityBatch
	err = s.ReadJSON(&old, "state", "activity", activityBatchDir, name)
	if err == nil {
		if equalJSON(old, b) {
			return raw, true, nil
		}
		return nil, false, ErrActivityBatch
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	sources, err := Sources(s)
	if err != nil {
		return nil, false, err
	}
	if old, ok := sources[src.SourceID]; ok && !equalJSON(old, src) {
		return nil, false, ErrActivityBatch
	}
	existing, damage, err := ReadSpoolChecked(s)
	if err != nil {
		return nil, false, err
	}
	if damage.Any() {
		return nil, false, ErrActivityBatch
	}
	seen := map[string]bool{}
	for _, o := range existing {
		seen[o.ObservationID] = true
	}
	for _, o := range obs {
		if seen[o.ObservationID] {
			return nil, false, ErrActivityBatch
		}
	}
	return raw, false, nil
}

func readActivityBatches(s *store.Store) ([]activity.Observation, map[string]activity.Source, error) {
	dir := s.Path("state", "activity", activityBatchDir)
	entries, err := os.ReadDir(dir)
	sources := map[string]activity.Source{}
	observations := []activity.Observation{}
	if errors.Is(err, fs.ErrNotExist) {
		return observations, sources, nil
	}
	if err != nil {
		return nil, nil, err
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		} // Uncommitted atomic-write temporary files are invisible.
		if !batchID.MatchString(entry.Name()[:len(entry.Name())-5]) || !entry.Type().IsRegular() {
			return nil, nil, ErrActivityBatch
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(f, maxActivityBatchBytes+1))
		f.Close()
		if err != nil || len(raw) > maxActivityBatchBytes {
			return nil, nil, ErrActivityBatch
		}
		b, err := decodeActivityBatch(s, entry.Name(), raw)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range b.Observations {
			if ids[o.ObservationID] {
				return nil, nil, ErrActivityBatch
			}
			ids[o.ObservationID] = true
		}
		sources[b.Source.SourceID] = b.Source
		observations = append(observations, b.Observations...)
	}
	return observations, sources, nil
}

// Committed metadata must not hide a second body behind duplicate JSON object keys.
func uniqueBatchJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 128 {
			return ErrActivityBatch
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrActivityBatch
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrActivityBatch
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrActivityBatch
			}
		default:
			return ErrActivityBatch
		}
		return nil
	}
	if value(0) != nil {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func decodeActivityBatch(s *store.Store, name string, raw []byte) (activityBatch, error) {
	if !uniqueBatchJSON(raw) {
		return activityBatch{}, ErrActivityBatch
	}
	var b activityBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || dec.Decode(new(any)) != io.EOF || name != b.Source.SourceID+".json" {
		return activityBatch{}, ErrActivityBatch
	}
	if err := validateActivityBatch(s, b); err != nil {
		return activityBatch{}, fmt.Errorf("%w: invalid committed batch", ErrActivityBatch)
	}
	return b, nil
}
