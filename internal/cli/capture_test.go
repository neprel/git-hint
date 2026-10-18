package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// closedUnit returns a project whose work unit "split" has a closed successful attempt after
// a collected change, and the plan ID.
func closedUnit(t *testing.T) (string, string) {
	t.Helper()
	shop, _ := setup(t)
	srv := singleRepoServer(t, shop)
	t.Cleanup(srv.Close)
	must(t, run(t, shop, "", "recommend", "--yes", "--server", srv.URL))
	planID := firstPlan(t, shop)
	must(t, run(t, shop, "", "eval", "work", "start", "split", "--plan", planID, "--family", "billing"))
	must(t, member(t, shop, "split", "t1", true, -time.Minute)) // its events lie before the close
	change(t, shop, "func Captured() int { return 1 }\n")
	must(t, run(t, shop, "", "eval", "work", "close", "split", "--outcome", "completed"))
	return shop, planID
}

func declaration(t *testing.T, shop string) project.Declaration {
	t.Helper()
	s, _ := store.Open(shop)
	ds, err := project.Declarations(s, shopRepo(t, s))
	if err != nil || len(ds) != 1 {
		t.Fatalf("declarations %v %v", ds, err)
	}
	return ds[0]
}

func TestLatestDeclarationCapturesTheAfterTree(t *testing.T) {
	shop, planID := closedUnit(t)
	ledger := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
	os.Chmod(ledger, 0o755)                                                               // a mode change is invisible to the snapshot
	os.Symlink("ledger.go", filepath.Join(shop, "canary_dir_e1d2", "billing", "link.go")) // so is a symlink
	// an ignored file (a rule of this temporary test repository) is not part of the state
	ex, _ := os.OpenFile(filepath.Join(shop, ".git", "info", "exclude"), os.O_APPEND|os.O_WRONLY, 0o644) // keeps the .hint/ rule
	ex.WriteString("*.log\n")
	ex.Close()
	os.WriteFile(filepath.Join(shop, "ignored.log"), []byte("x"), 0o644)
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	d := declaration(t, shop)
	if d.AfterTree == "" || d.AfterTreeProv.Method == "" {
		t.Fatalf("no captured tree: %+v", d)
	}
	s, _ := store.Open(shop)
	tree, err := trees.Load(library.Open(s, 0), d.AfterTree)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, f := range tree.Files {
		modes[f.Path] = f.Mode
	}
	if _, ok := modes["ignored.log"]; ok {
		t.Fatal("an ignored file entered the captured state")
	}
	if modes["canary_dir_e1d2/billing/ledger.go"] != "100755" || modes["canary_dir_e1d2/billing/link.go"] != "120000" {
		t.Fatalf("modes and symlinks are not in the captured state: %v", modes)
	}
	// a repeat is idempotent: the stored declaration (and its first provenance) stays
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	if d2 := declaration(t, shop); d2 != d {
		t.Fatalf("the repeat changed the declaration: %+v", d2)
	}
}

func TestCaptureRefusesAWorkTreeThatIsNotTheAfterSnapshot(t *testing.T) {
	for name, edit := range map[string]func(shop string){
		"untracked file": func(shop string) { os.WriteFile(filepath.Join(shop, "new.go"), []byte("package main\n"), 0o644) },
		"deleted file":   func(shop string) { os.Remove(filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger_test.go")) },
		"edited file": func(shop string) {
			f := filepath.Join(shop, "canary_dir_e1d2", "billing", "ledger.go")
			b, _ := os.ReadFile(f)
			os.WriteFile(f, append(b, []byte("\nfunc Later() int { return 2 }\n")...), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			shop, planID := closedUnit(t)
			edit(shop)
			r := run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest")
			if r.code == 0 || !strings.Contains(r.stderr, "not the after snapshot") {
				t.Fatalf("%s: %+v", name, r)
			}
			s, _ := store.Open(shop)
			if ds, _ := project.Declarations(s, shopRepo(t, s)); len(ds) != 0 {
				t.Fatal("a refused capture wrote a declaration")
			}
		})
	}
}

func TestAHistoricalAfterHasNoArtifact(t *testing.T) {
	shop, planID := closedUnit(t)
	s, _ := store.Open(shop)
	latest, _ := project.LoadLatest(s, shopRepo(t, s))
	change(t, shop, "func After() int { return 3 }\n") // the declared state is now historical
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", latest.Record.SnapshotID))
	if d := declaration(t, shop); d.AfterTree != "" || d.AfterSnapshotID != latest.Record.SnapshotID {
		t.Fatalf("a historical after state got an artifact: %+v", d)
	}
}

func TestVariantPlanNeedsTheCapturedAfterTree(t *testing.T) {
	shop, planID := closedUnit(t)
	s, _ := store.Open(shop)
	if _, _, err := project.VariantPlan(s, "split", planID); err == nil || !strings.Contains(err.Error(), "no declaration") {
		t.Fatalf("without a declaration: %v", err)
	}
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	plan, after, err := project.VariantPlan(s, "split", planID)
	if err != nil || plan["after_tree"] != after || plan["plan_id"] != planID {
		t.Fatalf("plan %v %v", plan, err)
	}
	l := library.Open(s, 0)
	w, _ := trees.FromWorktree(context.Background(), l, shop, trees.Limits{})
	if w != after {
		t.Fatal("the unchanged work tree is not the captured after state")
	}
	os.WriteFile(filepath.Join(shop, "canary_dir_e1d2", "billing", "late.go"), []byte("package billing\n"), 0o644) // a change after the capture
	if w2, _ := trees.FromWorktree(context.Background(), l, shop, trees.Limits{}); w2 == after {
		t.Fatal("a changed work tree still matches the captured state")
	}
	// the stored captured tree itself never changes
	if _, err := trees.Load(l, after); err != nil {
		t.Fatal(err)
	}
}

func TestVariantPlanIsUnavailableForAHistoricalAfter(t *testing.T) {
	shop, planID := closedUnit(t)
	s, _ := store.Open(shop)
	latest, _ := project.LoadLatest(s, shopRepo(t, s))
	change(t, shop, "func Later2() int { return 4 }\n")
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", latest.Record.SnapshotID))
	if _, _, err := project.VariantPlan(s, "split", planID); err == nil || !strings.Contains(err.Error(), "no captured tree") {
		t.Fatalf("historical: %v", err)
	}
}

func TestOneWorkAttemptHasOneDeclaredAfterState(t *testing.T) {
	shop, planID := closedUnit(t)
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	change(t, shop, "func Second() int { return 5 }\n") // a later state for the same closed attempt
	r := run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest")
	if r.code == 0 || !strings.Contains(r.stderr, "different declaration") {
		t.Fatalf("a second after state for one work attempt: %+v", r)
	}
	if d := declaration(t, shop); d.AfterTree == "" {
		t.Fatal("the first declaration was changed")
	}
}

// C7BE-9: binding flags are checked before any policy, runner or model step.
func TestEvalRunRefusesIncompleteBindingFlagsFirst(t *testing.T) {
	shop, _ := setup(t)
	for _, args := range [][]string{
		{"--agent", "nop", "--plan", strings.Repeat("a", 32), "--work-unit", "u"},                                      // no --with
		{"--agent", "nop", "--arm", "refactor", "--plan", strings.Repeat("a", 32), "--with", shop},                     // no --work-unit
		{"--agent", "nop", "--arm", "refactor", "--work-unit", "u", "--with", shop},                                    // no --plan
		{"--agent", "nop", "--arm", "baseline", "--plan", strings.Repeat("a", 32), "--work-unit", "u", "--with", shop}, // wrong arm
		{"--agent", "nop", "--arm", "refactor", "--plan", strings.Repeat("a", 32), "--work-unit", "u", "--from", "x.json", "--with", shop},
	} {
		r := run(t, shop, "", append([]string{"eval", "run"}, args...)...)
		if r.code != 2 || strings.Contains(strings.ToLower(r.stderr), "harbor") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if _, err := os.Stat(filepath.Join(shop, ".hint", "state", "evals", "work")); err == nil {
		t.Fatal("a refused run wrote a run journal")
	}
}

// C7BE-8 (client): late activity after the closure makes a new plan binding unavailable.
func TestVariantPlanRefusesAStaleUnit(t *testing.T) {
	shop, planID := closedUnit(t)
	must(t, run(t, shop, "", "changes", "--declare-plan", planID, "--work-unit", "split", "--after", "latest"))
	must(t, member(t, shop, "split", "late-task", false, 60*time.Second)) // activity after the closure
	s, _ := store.Open(shop)
	if _, _, err := project.VariantPlan(s, "split", planID); err == nil || !strings.Contains(err.Error(), "later activity") {
		t.Fatalf("a stale unit gave a binding: %v", err)
	}
	if d := declaration(t, shop); d.AfterTree == "" {
		t.Fatal("the historical declaration was changed")
	}
}
