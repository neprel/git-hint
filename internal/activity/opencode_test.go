package activity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

var testKey = make([]byte, 32)

func norm() *Normalizer {
	return &Normalizer{Key: testKey, SourceID: hint.DeriveID(testKey, "source", "t"), Cwd: "/w/proj",
		Resolve: func(abs string) (string, bool) {
			rel, ok := strings.CutPrefix(abs, "/w/proj/")
			if !ok {
				return "", false
			}
			return hint.FileID(testKey, "repo", rel), true
		},
		Now: func() time.Time { return time.Unix(1791700000, 0) }}
}

func events(lines ...map[string]any) string {
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func ev(hook, tool, call string, out map[string]any) map[string]any {
	return map[string]any{"hook": hook, "t_wall_ms": 1791700000000,
		"input": map[string]any{"tool": tool, "sessionID": "s1", "callID": call}, "output": out}
}

func stream(t *testing.T, in string) []Observation {
	t.Helper()
	var out []Observation
	if _, err := norm().Stream(strings.NewReader(in), func(o Observation) error { out = append(out, o); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestShellWorkdirAndTargets(t *testing.T) {
	obs := stream(t, events(
		ev("tool.execute.before", "bash", "c1", map[string]any{"args": map[string]any{"command": "cat a.go", "workdir": "sub"}}),
		ev("tool.execute.after", "bash", "c1", map[string]any{"metadata": map[string]any{"exit": 0}, "output": "[redacted:len=5]"}),
	))
	if len(obs) != 1 {
		t.Fatal(obs)
	}
	o := obs[0]
	want := hint.FileID(testKey, "repo", "sub/a.go")
	if o.Op != "READ" || o.Source != "shell_parser" || o.Precision != "inferred" || o.CommandClass != "READ" ||
		len(o.Targets) != 1 || o.Targets[0].EntityID != want || o.ContentDelivery != "unknown" {
		t.Fatalf("%+v", o)
	}
	if o.Size == nil || *o.Size.Value != 5 || *o.Outcome.ExitCode != 0 || o.Outcome.Status != "success" {
		t.Fatalf("%+v", o)
	}
}

func TestPathOutsideRepositoryIsNotExported(t *testing.T) {
	obs := stream(t, events(
		ev("tool.execute.before", "read", "c1", map[string]any{"args": map[string]any{"filePath": "/etc/HINTCANARY_passwd"}}),
		ev("tool.execute.after", "read", "c1", map[string]any{"metadata": map[string]any{}, "output": "x"}),
	))
	o := obs[0]
	if len(o.Targets) != 0 || !contains(o.Missing, "targets") {
		t.Fatalf("%+v", o)
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "HINTCANARY") || strings.Contains(string(b), "/etc") {
		t.Fatalf("content leaked: %s", b)
	}
}

func TestFailureAndMissingAfter(t *testing.T) {
	obs := stream(t, events(
		ev("tool.execute.before", "edit", "c1", map[string]any{"args": map[string]any{"filePath": "/w/proj/a.go"}}),
		ev("tool.execute.after", "edit", "c1", map[string]any{"metadata": map[string]any{}, "status": "error"}),
		ev("tool.execute.before", "bash", "c2", map[string]any{"args": map[string]any{"command": "go test ./..."}}),
	))
	if len(obs) != 2 {
		t.Fatal(obs)
	}
	if obs[0].Outcome.Status != "failure" || !contains(obs[0].Missing, "size") {
		t.Fatalf("%+v", obs[0])
	}
	// a call without an after event: outcome unknown, end missing
	if obs[1].Op != "TEST" || obs[1].Outcome.Status != "unknown" || obs[1].Time.End != nil ||
		!contains(obs[1].Missing, "time.end") || !contains(obs[1].Missing, "outcome.exit_code") {
		t.Fatalf("%+v", obs[1])
	}
}

func TestUnknownShellStaysUnknown(t *testing.T) {
	for _, cmd := range []string{"hintctl sync --all", "echo 'unterminated", "coproc cat x"} {
		obs := stream(t, events(ev("tool.execute.before", "bash", "c", map[string]any{"args": map[string]any{"command": cmd}})))
		if o := obs[0]; o.Op != "EXECUTE" || o.CommandClass != "UNKNOWN" || o.Precision != "unknown" {
			t.Errorf("%q: %+v", cmd, o)
		}
	}
}

func TestObservationIDsAreUniquePerSessionAndCall(t *testing.T) {
	n := norm()
	a := hint.DeriveID(n.Key, "event", n.SourceID, "s1", "c1")
	b := hint.DeriveID(n.Key, "event", n.SourceID, "s2", "c1")
	if a == b {
		t.Fatal("same call ID in two sessions gives one observation ID")
	}
}

func TestMutationAttribution(t *testing.T) {
	k := testKey
	file := hint.FileID(k, "repo", "a.go")
	since, until := time.Unix(1000, 0), time.Unix(2000, 0)
	at := Timestamp(time.Unix(1500, 0))
	edit := Observation{ObservationID: "e1", Op: "EDIT_INTENT", Time: Time{End: &at}, Targets: []Target{{file}},
		Correlation: map[string]string{"tool_call_id": hint.DeriveID(k, "call", "x")}}
	read := Observation{ObservationID: "r1", Op: "READ", Time: Time{End: &at}, Targets: []Target{{file}}}
	old := Timestamp(time.Unix(500, 0))
	early := Observation{ObservationID: "e0", Op: "EDIT_INTENT", Time: Time{End: &old}, Targets: []Target{{file}}}
	ch := []FileChange{{EntityID: file}}
	cases := []struct {
		recent []Observation
		want   string
	}{
		{nil, "none"},
		{[]Observation{read}, "none"},
		{[]Observation{early}, "none"},
		{[]Observation{edit, read}, "inferred"},
		{[]Observation{edit, func() Observation { e := edit; e.ObservationID = "e2"; return e }()}, "none"},
	}
	for i, c := range cases {
		o := Mutations(k, "a", "b", ch, since, until, c.recent)[0]
		if o.MutationAttribution != c.want {
			t.Errorf("case %d: %s, want %s", i, o.MutationAttribution, c.want)
		}
		if c.want == "none" && len(o.Correlation) != 0 {
			t.Errorf("case %d: unassigned mutation has correlation %v", i, o.Correlation)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
