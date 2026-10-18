package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/episodes"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/model"
)

// Segmentation budget (review 3, E06): per session one job of at most SegmentRequests
// requests including retries and SegmentSeconds; at most SegmentSessions sessions per
// pass. The job journal makes a session's job run once.
const (
	SegmentRequests = 2
	SegmentSeconds  = 120
	SegmentSessions = 10
)

// segmentRecord is a checked segmentation answer, kept so that later passes replay it.
type segmentRecord struct {
	Tasks     []string          `json:"tasks"`
	Families  map[string]string `json:"families"`
	Generator map[string]any    `json:"generator"`
}

// modelSegmenter returns the local model segmentation of the policy, or nil when the
// policy has no model. A checked answer is stored under its job key (session and request
// texts) and replayed in later passes without a model call, so repeated collects keep the
// same boundaries, IDs and provenance. A failed job stores nothing; the rule episode
// stays. The limits are the stricter of the policy's and the segmentation budget; no job
// starts after ctx ends.
func modelSegmenter(ctx context.Context, l *library.Library, p *evals.Policy) episodes.Segmenter {
	if p.Check(evals.OpModel, "") != nil {
		return nil
	}
	lim := p.Limits()
	pp := *p
	m := *p.Model
	m.MaxRequestsPerJob = min(lim.ModelRequests, SegmentRequests)
	m.MaxRetries = min(lim.ModelRetries, m.MaxRequestsPerJob-1)
	m.MaxSecondsPerJob = min(lim.ModelSeconds, SegmentSeconds)
	pp.Model = &m
	used := 0
	return func(session string, reqs []string) ([]string, map[string]string, map[string]any, bool) {
		h := sha256.Sum256([]byte("segment\x00" + session + "\x00" + strings.Join(reqs, "\x00")))
		key := hex.EncodeToString(h[:8])
		var kept segmentRecord
		if l.Index("segments/"+key, &kept) == nil && model.CheckSegments(model.SegmentAnswer{Tasks: kept.Tasks, Families: kept.Families}, len(reqs)) == nil {
			return kept.Tasks, kept.Families, kept.Generator, true // replay: no model call
		}
		if used >= SegmentSessions || ctx.Err() != nil {
			return nil, nil, nil, false // the next pass continues
		}
		c, err := model.New(&pp, l, "jb-"+key)
		if err != nil {
			return nil, nil, nil, false // no model, or this session's job ran (and failed) before
		}
		defer c.Close()
		used++
		ans, _, err := model.Segmentation{Requests: reqs}.GenerateRaw(ctx, c)
		if err != nil {
			return nil, nil, nil, false
		}
		raw, _ := json.Marshal(ans)
		out, err := l.PutObject(raw)
		if err != nil {
			return nil, nil, nil, false
		}
		var inputs []any
		for _, r := range reqs {
			if d, err := l.PutObject([]byte(r)); err == nil {
				inputs = append(inputs, d)
			}
		}
		st, _ := l.Job("jb-" + key)
		gen := map[string]any{"name": "segment", "version": "1", "model": p.Model.Model, "inputs": inputs, "output": out, "requests": st.Requests}
		if err := l.PutIndex("segments/"+key, segmentRecord{Tasks: ans.Tasks, Families: ans.Families, Generator: gen}); err != nil {
			return nil, nil, nil, false // not kept: the next pass would differ, so the rules stay
		}
		return ans.Tasks, ans.Families, gen, true
	}
}
