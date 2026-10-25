//go:build unix

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

func TestRepositoryResolverSameFileAliasAndMissingDeclaration(t *testing.T) {
	shop, _ := setup(t)
	s, e := store.Open(shop)
	if e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(t.TempDir(), "explicit-root-alias")
	if e = os.Symlink(s.Root, alias); e != nil {
		t.Fatal(e)
	}
	resolve := resolver(s)
	for _, rel := range []string{"canary_dir_e1d2/billing/ledger.go", "web-directory-missing/new.go", "canary_dir_e1d2/billing"} {
		physical := filepath.Join(s.Root, rel)
		logical := filepath.Join(alias, rel)
		if rel == "canary_dir_e1d2/billing/ledger.go" {
			p, e := os.Stat(physical)
			if e != nil {
				t.Fatal(e)
			}
			l, e := os.Stat(logical)
			if e != nil || !os.SameFile(p, l) {
				t.Fatal("fixture is not same file", e)
			}
		}
		want, ok := resolve(filepath.ToSlash(physical))
		if !ok {
			t.Fatal("physical target refused", rel)
		}
		got, ok := resolve(filepath.ToSlash(logical))
		if !ok || got != want {
			t.Fatal("alias target identity differs", rel)
		}
	}
	for _, rel := range []string{".hint/state/project.json", ".git/config"} {
		if _, ok := resolve(filepath.ToSlash(filepath.Join(alias, rel))); ok {
			t.Fatal("private target accepted", rel)
		}
	}
	must(t, run(t, alias, openCodeEvents(alias), "adapter", "opencode", "--project", alias))
	file := filepath.Join(s.Root, "canary_dir_e1d2/billing/ledger.go")
	data, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(file, append(data, '\n'), 0644); e != nil {
		t.Fatal(e)
	}
	must(t, runAt(t, alias, time.Minute, "collect"))
	observations, _, e := project.ReadSpool(s)
	if e != nil {
		t.Fatal(e)
	}
	mutations := 0
	for _, observation := range observations {
		if observation.Op == "FILE_MUTATION" {
			mutations++
			if observation.MutationAttribution != "inferred" {
				t.Fatal("same-file alias lost actual mutation attribution", observation.MutationAttribution)
			}
		}
	}
	if mutations != 1 {
		t.Fatal("unexpected mutation denominator", mutations)
	}
}

func TestEvalContextAliasAndUnsafePaths(t *testing.T) {
	shop, _ := setup(t)
	s, e := store.Open(shop)
	if e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(t.TempDir(), "explicit-root-alias")
	if e = os.Symlink(s.Root, alias); e != nil {
		t.Fatal(e)
	}
	var spawned []string
	must(t, runEval(t, alias, &spawned, "eval", "on", "--run", "docker"))
	l := library.Open(s, 0)
	rec := evaltest.GoBugFix(t, l)
	if _, e = l.PutManifest("eval", rec); e != nil {
		t.Fatal(e)
	}
	v2 := evaltest.MapGoBugFix(t, l, rec)
	v2["version"], v2["previous"], v2["state"] = 2, fmt.Sprintf("%s@1", rec["eval_id"]), "validated"
	if _, e = l.PutManifest("eval", v2); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(s.Root, "AGENTS.md")
	if e = os.WriteFile(file, []byte("Synthetic context.\n"), 0644); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.WriteFile(filepath.Join(outside, "NOTES.md"), []byte("private"), 0644); e != nil {
		t.Fatal(e)
	}
	for name, target := range map[string]string{"final-link": file, "parent-link": outside} {
		if e = os.Symlink(target, filepath.Join(s.Root, name)); e != nil {
			t.Fatal(e)
		}
	}
	id := rec["eval_id"].(string)
	for _, path := range []string{filepath.Join(alias, "final-link"), filepath.Join(alias, "parent-link", "NOTES.md"), filepath.Join(alias, ".hint", "state", "project.json"), filepath.Join(alias, "missing.md"), filepath.Join(outside, "NOTES.md")} {
		r := runEval(t, alias, &spawned, "eval", "run", id, "--agent", "oracle", "--arm", "context_only", "--with", path)
		if r.code == 0 || !strings.Contains(r.stderr, "inside the repository") || len(spawned) != 0 {
			t.Fatalf("unsafe context accepted: %+v", r)
		}
	}
	r := runEval(t, alias, &spawned, "eval", "run", id, "--agent", "oracle", "--arm", "context_only", "--with", filepath.Join(alias, "AGENTS.md"))
	if r.code == 0 || !strings.Contains(r.stdout, "packaged (reference: original)") || len(spawned) != 1 {
		t.Fatalf("alias context refused: %+v %v", r, spawned)
	}
	// Spawn is the existing fake recorder. No qualification runner executes.
}
