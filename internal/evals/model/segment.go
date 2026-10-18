package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Segmentation asks the local model which of a session's user requests belong to the same
// task. It sees only the request texts, in order; no code and no tool output. The answer
// is untrusted: the caller splits a rule episode only at request boundaries and keeps
// interleaved tasks together as ambiguous.
type Segmentation struct {
	Requests []string
}

// SegmentAnswer is one task label per request and an optional family per task.
type SegmentAnswer struct {
	Tasks    []string          `json:"tasks"`
	Families map[string]string `json:"families"`
}

const segmentSystem = `You read the user's requests of one coding session, in order, and say which requests belong to the same task.
Answer with one JSON object only:
{"tasks": ["A", "A", "B"], "families": {"A": "bug_fix", "B": "feature"}}
Rules:
- "tasks" has exactly one label per request, in the same order.
- A request that corrects, clarifies or extends the previous work on the same task gets the same label.
- A request that starts unrelated work gets a new label.
- Families: bug_fix, feature, compatibility, refactor. Leave a task out of "families" when you are not sure.`

var families = map[string]bool{"bug_fix": true, "feature": true, "compatibility": true, "refactor": true}

// GenerateRaw returns the checked answer and the raw text (local diagnosis only).
func (s Segmentation) GenerateRaw(ctx context.Context, c *Client) (SegmentAnswer, string, error) {
	var b strings.Builder
	for i, r := range s.Requests {
		fmt.Fprintf(&b, "Request %d:\n<<<\n%s\n>>>\n", i+1, strings.TrimSpace(r))
	}
	var ans SegmentAnswer
	if len(s.Requests) == 0 || b.Len() > 32<<10 {
		return ans, "", fmt.Errorf("the requests do not fit a segmentation")
	}
	out, err := c.Chat(ctx, []Message{{Role: "system", Content: segmentSystem}, {Role: "user", Content: b.String()}}, 400)
	if err != nil {
		return ans, "", err
	}
	raw := out
	if m := fence.FindStringSubmatch(out); m != nil {
		out = m[1]
	}
	j := jsonObject.FindString(out)
	if j == "" || json.Unmarshal([]byte(j), &ans) != nil {
		return ans, raw, fmt.Errorf("the segmentation answer is not JSON")
	}
	return ans, raw, CheckSegments(ans, len(s.Requests))
}

// CheckSegments validates an answer for n requests: one non-empty label per request, known
// families of known labels.
func CheckSegments(a SegmentAnswer, n int) error {
	if len(a.Tasks) != n {
		return fmt.Errorf("the segmentation answer has %d labels for %d requests", len(a.Tasks), n)
	}
	labels := map[string]bool{}
	for _, t := range a.Tasks {
		if strings.TrimSpace(t) == "" || len(t) > 20 {
			return fmt.Errorf("the segmentation answer has an unusable label")
		}
		labels[t] = true
	}
	for t, f := range a.Families {
		if !labels[t] || !families[f] {
			return fmt.Errorf("the segmentation answer has an unknown task or family")
		}
	}
	return nil
}
