package project

import (
	"fmt"
	"time"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/scan"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

// Limits for a recommendation document (exchange §3 too_large).
const (
	MaxDocumentBytes = 16 << 20
	MaxPlans         = 1000
	MaxStepsPerPlan  = 1000
)

// Current is the state of the work trees now, used to check a recommendation.
type Current struct {
	Repos map[string]*CurrentRepo // repo ID -> state
	Names map[string]string       // entity ID -> local display name ("repo: path symbol")
	Local hint.LocalState
}

// CurrentRepo is the current state of one repository.
type CurrentRepo struct {
	Name     string
	Snapshot *snapshot.Local
	Stored   bool // the current state equals the latest stored snapshot
}

// CurrentState scans every repository (with the cache) and compares it with the latest
// stored snapshot. Nothing is written except the rebuildable scan cache.
func CurrentState(s *store.Store, now time.Time) (*Current, error) {
	st, err := loadExportState(s)
	if err != nil {
		return nil, err
	}
	c := &Current{Repos: map[string]*CurrentRepo{}, Names: map[string]string{}, Local: hint.LocalState{
		SupportedVersions: version.Supported, ProjectNamespace: s.Cfg.ProjectNamespace,
		Now:             now.UTC().Format(time.RFC3339Nano),
		SnapshotDigests: map[string]string{}, SnapshotDigestSets: map[string][]string{},
		Resolvable: []string{}, Ambiguous: []string{}}}
	for _, r := range s.Cfg.Repos {
		repoID := s.RepoID(r)
		cachePath := s.Path("cache", "scan-"+repoID+".json")
		cache := scan.LoadCache(cachePath)
		res, err := scan.Scan(scan.Options{Root: s.RepoRoot(r), RepoID: repoID, Key: s.Key(), Cache: cache})
		if err != nil {
			return nil, err
		}
		if s.CheckExcluded() == nil {
			_ = cache.Save(cachePath)
		}
		prev, err := LoadLatest(s, repoID)
		if err != nil {
			return nil, err
		}
		cur, changed, err := snapshot.Build(prev, res, repoID, s.Key(), snapshot.ReadGitState(s.RepoRoot(r)), now)
		if err != nil {
			return nil, err
		}
		c.Repos[repoID] = &CurrentRepo{Name: r.Name, Snapshot: cur, Stored: !changed}
		c.Local.SnapshotDigests[repoID] = cur.Record.Digest
		if !changed {
			c.Local.SnapshotDigestSets[repoID] = st.SnapshotDigests[cur.Record.SnapshotID]
		}
		for id, ix := range cur.Index {
			c.Local.Resolvable = append(c.Local.Resolvable, id)
			c.Names[id] = r.Name + ": " + ix.Display()
		}
		for id := range cur.Ambiguous {
			c.Local.Ambiguous = append(c.Local.Ambiguous, id)
		}
	}
	return c, nil
}

// Imported is the result of a recommendation import.
type Imported struct {
	Result hint.ImportResult
	Doc    map[string]any // the parsed document (nil if rejected before parsing)
}

// Import validates a recommendation_set the same way in file mode and API mode
// (exchange §4). It never executes a step.
func Import(raw []byte, cur *Current) (*Imported, error) {
	if len(raw) > MaxDocumentBytes {
		return &Imported{Result: hint.ImportResult{Document: "too_large"}}, nil
	}
	sch, err := hint.LoadSchemas()
	if err != nil {
		return nil, err
	}
	res := hint.ImportRecommendations(raw, sch, cur.Local, nil)
	im := &Imported{Result: res}
	if res.Document != "accepted" {
		return im, nil
	}
	dec, err := hint.Decode(raw)
	if err != nil {
		return nil, err
	}
	doc, err := hint.Plain(dec)
	if err != nil {
		return nil, err
	}
	plans, _ := doc["payload"].(map[string]any)["plans"].([]any)
	if len(plans) > MaxPlans {
		return &Imported{Result: hint.ImportResult{Document: "too_large"}}, nil
	}
	for _, p := range plans {
		if steps, _ := p.(map[string]any)["steps"].([]any); len(steps) > MaxStepsPerPlan {
			return &Imported{Result: hint.ImportResult{Document: "too_large"}}, nil
		}
	}
	im.Doc = doc
	return im, nil
}

// SaveRecommendation stores an imported recommendation document (raw bytes) and returns
// its path below .hint/state/recommendations/.
func SaveRecommendation(s *store.Store, raw []byte) (string, error) {
	d, err := hint.Digest(mustPlain(raw))
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("recommendation_set-%s.hint.json", d[len("sha256:"):len("sha256:")+12])
	if err := s.WriteState(raw, "state", "recommendations", name); err != nil {
		return "", err
	}
	if err := s.WriteJSON(map[string]string{"file": name}, "state", "recommendations", "latest.json"); err != nil {
		return "", err
	}
	return s.Path("state", "recommendations", name), nil
}

// LatestRecommendation returns the path of the last stored recommendation document.
func LatestRecommendation(s *store.Store) (string, error) {
	var ref struct {
		File string `json:"file"`
	}
	if err := s.ReadJSON(&ref, "state", "recommendations", "latest.json"); err != nil {
		return "", fmt.Errorf("no stored recommendation; use `git hint recommend` or `git hint plan --from <file>`")
	}
	return s.Path("state", "recommendations", ref.File), nil
}

func mustPlain(raw []byte) any {
	dec, err := hint.Decode(raw)
	if err != nil {
		return string(raw)
	}
	return dec
}
