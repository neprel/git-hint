package model

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RequirementMap asks the local model to split the request and the corrections into
// atomic requirements and to link each one to the hidden tests that verify it. The answer
// is untrusted: the caller applies it with build.ApplyMapping, which checks every quote
// against the user's text, every test name against the verifier and the completeness of
// the split. The tests are evaluator-side; nothing of them reaches the agent.
type RequirementMap struct {
	Sources map[string]string // requirement ID (R1, R2, ...) -> request or correction text
	Order   []string          // the source IDs in order
	TestIDs []string          // test IDs (file::Name) or whole checks (C1, C2)
	Notes   map[string]string // a short description of a whole check
	Files   map[string]string // hidden test file (or check input) -> its source
}

// MapAnswer is the expected answer shape.
type MapAnswer struct {
	Requirements []struct {
		Source string   `json:"source"`
		Quote  string   `json:"quote"`
		Tests  []string `json:"tests"`
	} `json:"requirements"`
	Context []string `json:"context"`
}

const mapSystem = `You split a user's coding request into atomic requirements and link each requirement to the tests that verify it.
Answer with one JSON object only:
{"requirements": [{"source": "R1", "quote": "<exact words of the user>", "tests": ["<test ID>"]}], "context": ["<exact words of the user>"]}
Rules:
- Copy every quote exactly from the user's text.
- Every part of the user's text must be in one requirement quote or in context.
- Context is only for words that state no requirement: background, a description of the problem, politeness.
- Link a test only when its code checks the quoted behavior. When no test checks a requirement, give it an empty tests list.
- Never invent a test ID. Use only the test IDs listed.`

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// Generate returns the parsed answer.
func (m RequirementMap) Generate(ctx context.Context, c *Client) (MapAnswer, error) {
	ans, _, err := m.GenerateRaw(ctx, c)
	return ans, err
}

// GenerateRaw also returns the raw answer text (local diagnosis only).
func (m RequirementMap) GenerateRaw(ctx context.Context, c *Client) (MapAnswer, string, error) {
	var b strings.Builder
	b.WriteString("User's text (quote only the text between the markers):\n")
	for _, id := range m.Order {
		fmt.Fprintf(&b, "<<<%s\n%s\n>>>\n", id, strings.TrimSpace(m.Sources[id]))
	}
	b.WriteString("\nTest IDs:\n")
	for _, id := range m.TestIDs {
		if n := m.Notes[id]; n != "" {
			b.WriteString(id + " (" + n + ")\n")
		} else {
			b.WriteString(id + "\n")
		}
	}
	files := make([]string, 0, len(m.Files))
	for f := range m.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	b.WriteString("\nTest files:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "--- %s\n%s\n", f, strings.TrimSpace(m.Files[f]))
	}
	var ans MapAnswer
	if b.Len() > 64<<10 {
		return ans, "", fmt.Errorf("the request and tests are too large for a mapping")
	}
	out, err := c.Chat(ctx, []Message{{Role: "system", Content: mapSystem}, {Role: "user", Content: b.String()}}, 1500)
	if err != nil {
		return ans, "", err
	}
	raw := out
	if m := fence.FindStringSubmatch(out); m != nil {
		out = m[1]
	}
	j := jsonObject.FindString(out)
	if j == "" || json.Unmarshal([]byte(j), &ans) != nil {
		return ans, raw, fmt.Errorf("the mapping answer is not JSON")
	}
	if len(ans.Requirements) == 0 || len(ans.Requirements) > 50 {
		return ans, raw, fmt.Errorf("the mapping answer has no usable requirement list")
	}
	return ans, raw, nil
}
