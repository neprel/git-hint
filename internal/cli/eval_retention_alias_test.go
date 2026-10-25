//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
)

func TestEvalRetentionAliasPreservesBusyAndDefinitionGuards(t *testing.T) {
	shop, _ := setup(t)
	alias := filepath.Join(t.TempDir(), "project-alias")
	if e := os.Symlink(shop, alias); e != nil {
		t.Fatal(e)
	}
	var spawned []string
	must(t, runEval(t, alias, &spawned, "eval", "on"))
	s, e := store.Open(alias)
	if e != nil {
		t.Fatal(e)
	}
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	rec["created_at"] = "2025-01-01T00:00:00Z"
	name, e := l.PutManifest("eval", rec)
	if e != nil {
		t.Fatal(e)
	}
	assertRetained := func() {
		t.Helper()
		var got map[string]any
		if e := l.Manifest("eval", name, &got); e != nil {
			t.Fatal("guard lost manifest", e)
		}
	}
	lock, e := s.LockState(false)
	if e != nil {
		t.Fatal(e)
	}
	r := runEval(t, alias, &spawned, "eval", "forget", "--all")
	lock.Unlock()
	if r.code == 0 || !strings.Contains(r.stderr, store.ErrBusy.Error()) {
		t.Fatalf("busy guard: %+v", r)
	}
	assertRetained()
	barrier := s.Path("state", library.WorkDefinitionStateDir)
	if e = os.Mkdir(barrier, 0700); e != nil {
		t.Fatal(e)
	}
	r = runEval(t, alias, &spawned, "eval", "forget", "--all")
	if r.code == 0 || !strings.Contains(r.stderr, library.ErrDefinitionReferences.Error()) {
		t.Fatalf("definition guard: %+v", r)
	}
	must(t, runEval(t, alias, &spawned, "collect"))
	assertRetained()
	if e = os.Remove(barrier); e != nil {
		t.Fatal(e)
	}
	must(t, runEval(t, alias, &spawned, "collect"))
	if names, e := l.Manifests("eval"); e != nil || len(names) != 0 {
		t.Fatal("ordinary pruning failed", names, e)
	}
	must(t, runEval(t, alias, &spawned, "eval", "forget", "--all"))
	if len(spawned) != 0 {
		t.Fatal("test launched worker", spawned)
	}
}
