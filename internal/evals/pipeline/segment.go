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

// modelSegmenter returns the local model segmentation of the policy, or nil when the
// policy has no model. Each answer is stored and named in the generator record.
func modelSegmenter(ctx context.Context, l *library.Library, p *evals.Policy) episodes.Segmenter {
	if p.Check(evals.OpModel, "") != nil {
		return nil
	}
	pp := *p
	m := *p.Model
	m.MaxRequestsPerJob, m.MaxRetries, m.MaxSecondsPerJob = SegmentRequests, SegmentRequests-1, SegmentSeconds
	if p.Model.MaxSecondsPerJob > 0 && p.Model.MaxSecondsPerJob < SegmentSeconds {
		m.MaxSecondsPerJob = p.Model.MaxSecondsPerJob
	}
	pp.Model = &m
	used := 0
	return func(session string, reqs []string) ([]string, map[string]string, map[string]any, bool) {
		if used >= SegmentSessions {
			return nil, nil, nil, false // the next pass continues
		}
		h := sha256.Sum256([]byte("segment\x00" + session + "\x00" + strings.Join(reqs, "\x00")))
		job := "jb-" + hex.EncodeToString(h[:8])
		c, err := model.New(&pp, l, job)
		if err != nil {
			return nil, nil, nil, false // no model, or this session's job ran before
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
		st, _ := l.Job(job)
		gen := map[string]any{"name": "segment", "version": "1", "model": p.Model.Model, "inputs": inputs, "output": out, "requests": st.Requests}
		return ans.Tasks, ans.Families, gen, true
	}
}
