package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	must(t, member(t, shop, "split", "t1", true, 0))
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
