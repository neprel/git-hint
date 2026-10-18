package episodes

import (
	"github.com/neprel/git-hint/internal/evals/sessions"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A later import can change a stored proposal (here: a parallel session makes it
// ambiguous). The change gets a revision ID; the earlier proposal stays unchanged.
func TestChangedProposalGetsARevision(t *testing.T) {
	l := lib(t)
	rec := func(attr string) *Proposal {
		return &Proposal{Record: map[string]any{
			"record": "episode", "manifest_version": 1, "episode_id": "ep-00000000000000aa", "created_at": time.Now().UTC().Format(time.RFC3339),
			"rules_version": RulesVersion,
			"sources":       []any{map[string]any{"agent": "opencode", "parser_version": "0.1.0", "source": "sha256:" + string(make64('a')), "session": "ses_x", "role": "primary"}},
			"boundary":      map[string]any{"basis": "inferred", "start": map[string]any{"source": "sha256:" + string(make64('a')), "record": "u1"}, "rule": "request-to-check", "support": "high"},
			"requests":      []any{map[string]any{"at": map[string]any{"source": "sha256:" + string(make64('a')), "record": "u1"}}},
			"repositories":  []any{map[string]any{"repo": "shop", "base_evidence": "unavailable"}},
			"attribution":   attr, "reasons": []string{},
		}}
	}
	if s, _, err := Store(l, []*Proposal{rec("exclusive")}); err != nil || s != 1 {
		t.Fatalf("first store: %d %v", s, err)
	}
	if _, u, err := Store(l, []*Proposal{rec("exclusive")}); err != nil || u != 1 {
		t.Fatalf("same proposal again: %d %v", u, err)
	}
	p := rec("ambiguous")
	if s, _, err := Store(l, []*Proposal{p}); err != nil || s != 1 {
		t.Fatalf("changed proposal: %d %v", s, err)
	}
	if p.Record["episode_id"] == "ep-00000000000000aa" {
		t.Fatal("the changed proposal overwrote the stored one")
	}
	var old map[string]any
	if err := l.Manifest("episode", "ep-00000000000000aa", &old); err != nil || old["attribution"] != "exclusive" {
		t.Fatalf("earlier proposal: %v %v", err, old["attribution"])
	}
	var cur map[string]any
	if err := l.Index("episodes/ep-00000000000000aa", &cur); err != nil || cur["current"] != p.Record["episode_id"] {
		t.Fatalf("current index: %v %v", err, cur)
	}
}

func make64(c byte) []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return b
}

// The recorded OpenCode 2.0.22 export: the shell tool's passing go test is the episode's
// outcome (the live prospective check lost it before "shell" was read as a command).
func TestOpenCode2022CheckIsTheOutcome(t *testing.T) {
	l := lib(t)
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "evals", "sessions", "opencode-2.0.22-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := sessions.ParseOpenCode(src, sessions.OpenCodeOptions{Version: "2.0.22"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Import(l, src, r); err != nil {
		t.Fatal(err)
	}
	props, err := Discover(l, time.UnixMilli(r.Session.Items[len(r.Session.Items)-1].Time).Add(time.Hour),
		func(_, _ string) string { return "" }, func(string) string { return "shop" })
	if err != nil || len(props) != 1 {
		t.Fatalf("%d proposals %v", len(props), err)
	}
	if props[0].Outcome != "test_passed" || strings.Join(props[0].Paths, ",") != "discount_test.go" {
		t.Fatalf("outcome %s paths %v reasons %v", props[0].Outcome, props[0].Paths, props[0].Record["reasons"])
	}
}
