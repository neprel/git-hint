package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ordinary server compare output, generated from synthetic resolved for_trial
// usage. This tests the formatter, not producer origin or real canonical intake.
const receivedComparisonServerFixture = `{"groups":[{"arm":"baseline","attempts":1,"controls":{"agent":"opencode","agent_version":"2.0.26","budget":{"max_requests":40,"max_seconds":900},"context_policy":{"ref":null,"state":"disabled"},"declared_changes":[],"harness_settings":[],"model_ref":"44444444444444444444444444444444","runner_version":"0.1.0"},"counter_scope":"full_task","counter_semantics":"hint-task-tokens-v1","environment":{"agent":"opencode","agent_version":"2.0.26","model_ref":"44444444444444444444444444444444","runner_version":"0.1.0"},"label":"observed","outcomes":{"passed":1},"partition":"held_out","requests":{"coverage":[1,1],"known":5,"total":5},"success_rate":{"interval":null,"observed":1.0,"tasks":1},"suite_ref":"11111111111111111111111111111111","task_families":1,"tasks":1,"tokens":{"coverage":[1,1],"known":12345,"total":12345},"tokens_per_success":{"coverage":[1,1],"includes_failed_attempts":true,"observed":12345.0},"wall_seconds":{"coverage":[1,1],"known":60.0,"median_known":60.0,"total":60.0}}],"incomparable":[],"notes":["Every attempt and its resources count; a total with an unknown value stays unknown (see known and coverage).","Groups never pool different controls; a pair names the controls of both sides.","Differences are observed on these tasks; they are not predictions or confirmed effects."],"pairs":[],"variant_pairs":[]}`

func receivedFixtureGroup(t *testing.T) map[string]any {
	t.Helper()
	var root map[string]any
	if e := json.Unmarshal([]byte(receivedComparisonServerFixture), &root); e != nil {
		t.Fatal(e)
	}
	return root["groups"].([]any)[0].(map[string]any)
}
func receivedGroupBytes(t *testing.T, g map[string]any) []byte {
	t.Helper()
	b, e := json.Marshal(map[string]any{"groups": []any{g}, "pairs": []any{map[string]any{"tokens_saved": 999999999}}, "notes": []string{"decoy forecast saving"}})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestEvalCompareReceivedServerFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "received.json")
	data := []byte(receivedComparisonServerFixture)
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"eval", "compare", "--received", "received.json"}, &Env{Dir: dir, Stdout: &stdout, Stderr: &stderr, Spawn: func(...string) error { t.Fatal("spawn from received formatter"); return nil }})
	want := "Observed received usage\nGroup 1: attempts=1; canonical full-task tokens=12345 (uncached input + output)\n"
	if code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(data, after) {
		t.Fatal("formatter changed saved response", e)
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 1 {
		t.Fatal("formatter created local state", e, len(files))
	}
	for _, word := range []string{"saving", "effect", "forecast", "999999999", "44444444"} {
		if strings.Contains(stdout.String(), word) {
			t.Fatal("unrelated or raw value printed", word)
		}
	}
}
func TestEvalCompareReceivedKnownNumbers(t *testing.T) {
	for _, number := range []json.Number{"0", "12345", "12345.0", "1e3", "9007199254740993"} {
		g := receivedFixtureGroup(t)
		tok := g["tokens"].(map[string]any)
		tok["known"], tok["total"] = number, number
		out, e := formatReceivedComparison(receivedGroupBytes(t, g))
		if e != nil || strings.Contains(out, "tokens=unknown") {
			t.Fatal(number, out, e)
		}
	}
}
func TestEvalCompareReceivedUnknownNeverUsesDecoys(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"unlabelled", func(g map[string]any) { delete(g, "counter_semantics") }},
		{"missing_scope", func(g map[string]any) { delete(g, "counter_scope") }},
		{"root", func(g map[string]any) {
			g["counter_semantics"] = "harbor-0.23.0+opencode:n-tokens-root-v1"
			g["counter_scope"] = "root_session"
		}},
		{"mixed", func(g map[string]any) { g["counter_semantics"] = "unknown"; g["counter_scope"] = "unknown" }},
		{"not_observed", func(g map[string]any) { g["label"] = "predicted" }},
		{"missing_total", func(g map[string]any) { delete(g["tokens"].(map[string]any), "total") }},
		{"unknown_total", func(g map[string]any) { g["tokens"].(map[string]any)["total"] = nil }},
		{"partial", func(g map[string]any) { g["tokens"].(map[string]any)["coverage"] = []int{1, 2} }},
		{"zero_attempts", func(g map[string]any) { g["attempts"] = 0; g["tokens"].(map[string]any)["coverage"] = []int{0, 0} }},
		{"unknown_known_part", func(g map[string]any) { g["tokens"].(map[string]any)["known"] = nil }},
		{"unequal_known_part", func(g map[string]any) { g["tokens"].(map[string]any)["known"] = 1 }},
		{"wrong_coverage_type", func(g map[string]any) { g["tokens"].(map[string]any)["coverage"] = []any{true, 1} }},
	}
	for _, value := range []any{true, "12345", -1, 1.5, json.Number("1e99999999"), json.Number("9223372036854775808")} {
		v := value
		cases = append(cases, struct {
			name   string
			change func(map[string]any)
		}{"invalid_total", func(g map[string]any) { g["tokens"].(map[string]any)["total"] = v }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := receivedFixtureGroup(t)
			tc.change(g)
			g["input_tokens"], g["output_tokens"] = 987654321, 987654321
			g["provider_total"] = 987654321
			out, e := formatReceivedComparison(receivedGroupBytes(t, g))
			if e != nil || !strings.Contains(out, "canonical full-task tokens=unknown") || strings.Contains(out, "987654321") || strings.Contains(out, "999999999") {
				t.Fatal(out, e)
			}
		})
	}
}
func TestEvalCompareReceivedMalformedAndBounds(t *testing.T) {
	for _, raw := range []string{"", `null`, `[]`, `{}`, `{"groups":null}`, `{"groups":[null]}`, `{"groups":[]} {}`, `{"groups":[],"groups":[]}`, `{"groups":[{"attempts":1,"attempts":2}]}`, `{"groups":[{"tokens":{"total":null,"total":1}}]}`, strings.Repeat(" ", receivedCompareMaxBytes+1)} {
		if out, e := formatReceivedComparison([]byte(raw)); e == nil || out != "" {
			t.Fatal("malformed input returned partial output", out, e)
		}
	}
	many := `{"groups":[` + strings.TrimSuffix(strings.Repeat(`{},`, 1001), ",") + `]}`
	if out, e := formatReceivedComparison([]byte(many)); e == nil || out != "" {
		t.Fatal("too many groups accepted")
	}
	for _, args := range [][]string{{"eval", "compare", "--received", ""}, {"eval", "compare", "--received", "missing.json"}, {"eval", "compare", "--received", "x", "suite"}, {"eval", "compare", "--json", "--received", "x"}} {
		dir := t.TempDir()
		r := run(t, dir, "", args...)
		if r.code == 0 || r.stdout != "" {
			t.Fatal("invalid received command fell back", r)
		}
		files, e := os.ReadDir(dir)
		if e != nil || len(files) != 0 {
			t.Fatal("invalid received command wrote state")
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "data.json")
	if e := os.WriteFile(target, []byte(receivedComparisonServerFixture), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link.json")
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	if r := run(t, dir, "", "eval", "compare", "--received", link); r.code == 0 {
		t.Fatal("received symlink accepted")
	}
}
