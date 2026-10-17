package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type fakeOC struct{ root, exportID, export string }

func (f fakeOC) Output(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	switch {
	case name == "opencode" && len(args) == 1 && args[0] == "--version":
		return []byte("2.0.26\n"), nil
	case name == "opencode" && args[0] == "session" && args[1] == "list":
		id := f.exportID
		if id == "" {
			id = "ses_synth_main_0001"
		}
		return json.Marshal([]map[string]any{{"id": id, "directory": f.root}})
	case name == "opencode" && args[0] == "session" && args[1] == "export" && (args[2] == f.exportID || f.exportID == "" && args[2] == "ses_synth_main_0001"):
		return []byte(f.export), nil
	}
	return nil, errors.New("unexpected")
}

func setup(t *testing.T) (*store.Store, Env, string) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "cart.go", "package shop\n\nfunc Total() int { return 1 }\n")
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "base")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	// an OpenCode-style snapshot repository with the base tree of the project
	snap := filepath.Join(data, "opencode", "snapshot", "prj_synth", "wt1")
	if out, err := exec.Command("git", "init", "-q", "--bare", snap).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	cmd := exec.Command("sh", "-c", `git --git-dir "$1" --work-tree "$2" add -A . ':!.git' ':!.hint' && git --git-dir "$1" --work-tree "$2" write-tree`, "sh", snap, dir)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "idx"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	tree := lines[len(lines)-1]
	b, _ := os.ReadFile(filepath.Join("..", "..", "..", "tests", "evals", "sessions", "opencode-2.0.26-main.json"))
	exp := strings.ReplaceAll(string(b), strings.Repeat("1", 40), tree) // msg_a1 starts at the real tree
	exp = strings.ReplaceAll(exp, `"/work/shop"`, `"`+dir+`"`)
	exp = strings.ReplaceAll(exp, "Fix it so the total applies the discount.", "Fix it. HINTCANARY_request_text")
	env := Env{Home: t.TempDir(), DataHome: data, Cmd: fakeOC{root: dir, export: exp}, Now: time.Now}
	return s, env, tree
}

func TestPassNeedsThePolicy(t *testing.T) {
	s, env, _ := setup(t)
	if r := Run(context.Background(), s, env); r.Ran || r.Blocked == "" {
		t.Fatalf("pass without a policy: %+v", r)
	}
	off := evals.Policy{PolicyVersion: 1, Capture: "off"}
	s.Cfg.Evals = &off
	if r := Run(context.Background(), s, env); r.Ran {
		t.Fatal("pass with capture off")
	}
}

func TestPassKeepsSnapshotsAndLeavesExportsClean(t *testing.T) {
	s, env, tree := setup(t)
	p := evals.Enabled([]string{"opencode"})
	s.Cfg.Evals = &p
	r := Run(context.Background(), s, env)
	if !r.Ran || len(r.Imports) != 1 || r.Imports[0].New != 1 {
		t.Fatalf("pass %+v", r)
	}
	// the fixture names 3 snapshot trees: one exists, two are unknown to the repository
	if r.Snapshots.Kept != 1 || r.Snapshots.Unavailable["baseline_missing"] != 2 {
		t.Fatalf("snapshots %+v", r.Snapshots)
	}
	l := library.Open(s, 0)
	var m snapMap
	if err := l.Index("snapmap/prj_synth", &m); err != nil || m.Trees[tree] == "" {
		t.Fatalf("snapshot map %v %+v", err, m)
	}
	// the fixture session ended long ago: one closed bug-fix episode with an exact base
	if r.Episodes.Proposed != 1 || r.Episodes.Stored != 1 {
		t.Fatalf("episodes %+v", r.Episodes)
	}
	r2 := Run(context.Background(), s, env)
	if r2.Episodes.Stored != 0 || r2.Episodes.Unchanged != 1 {
		t.Fatalf("second pass episodes %+v", r2.Episodes)
	}
	if r2.Imports[0].Unchanged != 1 || r2.Snapshots.Kept != 1 {
		t.Fatalf("second pass %+v", r2)
	}
	// ordinary canonical exports never carry eval evidence
	if _, err := project.Collect(s, project.CollectOptions{Full: true}); err != nil {
		t.Fatal(err)
	}
	pkg, err := project.BuildPackage(s, project.ExportOptions{All: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pkg.Docs {
		if strings.Contains(string(d.Raw), "HINTCANARY_request_text") {
			t.Fatal("session text in a canonical export")
		}
	}
}
