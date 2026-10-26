package pipeline

import (
	"archive/tar"
	"context"
	"encoding/json"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func selectedFactsFixture() (trees.SelectedReceipt, runner.PrelaunchInventory) {
	d := "sha256:" + strings.Repeat("a", 64)
	return trees.SelectedReceipt{Selected: []trees.SelectedPathFact{{Path: "src/file.go", State: trees.SelectedRegularPresent, Mode: "100644", Size: 3, ObjectDigest: d}, {Path: "removed.go", State: trees.SelectedAbsent}}}, runner.PrelaunchInventory{Digest: d, Entries: []runner.PrelaunchEntry{{Path: "src/file.go", Type: tar.TypeReg, Mode: 0644, LogicalMode: 0100644, Size: 3, Digest: d}}}
}
func TestSelectedInventoryExactSubsetAndUnmapped(t *testing.T) {
	s, inv := selectedFactsFixture()
	if _, _, _, e := selectedInventoryFacts(s, inv, true); e != nil {
		t.Fatal(e)
	}
	inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: ".git/HEAD", Type: tar.TypeReg, Mode: 0644, Size: 4, Digest: "sha256:" + strings.Repeat("b", 64)}, runner.PrelaunchEntry{Path: "extra.go", Type: tar.TypeReg, Mode: 0644, Size: 5, Digest: "sha256:" + strings.Repeat("c", 64)})
	compared, absent, unmapped, e := selectedInventoryFacts(s, inv, false)
	if e != nil || len(compared) != 1 || len(absent) != 1 || len(unmapped.Entries) != 2 {
		t.Fatal("subset lost unmapped population", e)
	}
	if _, _, _, e = selectedInventoryFacts(s, inv, true); e == nil {
		t.Fatal("extra BASE accepted as exact")
	}
}
func TestSelectedInventoryMismatchAndAliases(t *testing.T) {
	for _, kind := range []string{"byte", "size", "mode", "type", "missing", "absent_dir", "ancestor_link", "alias", "symlink_selected", "hardlink_selected", "ancestor_case_alias"} {
		t.Run(kind, func(t *testing.T) {
			s, inv := selectedFactsFixture()
			switch kind {
			case "byte":
				inv.Entries[0].Digest = "sha256:" + strings.Repeat("b", 64)
			case "size":
				inv.Entries[0].Size++
			case "mode":
				inv.Entries[0].LogicalMode = 0100755
			case "type":
				inv.Entries[0].Type = tar.TypeDir
			case "missing":
				inv.Entries = nil
			case "absent_dir":
				inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "removed.go", Type: tar.TypeDir, Mode: 0755})
			case "ancestor_link":
				inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "src", Type: tar.TypeSymlink, Mode: 0777, LinkTarget: "elsewhere", Unresolved: true})
			case "alias":
				n := inv.Entries[0]
				n.Path = "src/FILE.go"
				inv.Entries = append(inv.Entries, n)
			case "symlink_selected":
				inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "alias", Type: tar.TypeSymlink, Mode: 0777, LinkTarget: "src/file.go", Unresolved: true})
			case "ancestor_case_alias":
				inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "SRC", Type: tar.TypeDir, Mode: 0755})
			case "hardlink_selected":
				inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: "alias", Type: tar.TypeLink, Mode: 0644, LinkTarget: "app/src/file.go", Unresolved: true})
			}
			if _, _, _, e := selectedInventoryFacts(s, inv, false); e == nil {
				t.Fatal("invalid subset accepted", kind)
			}
		})
	}
}
func TestSelectedMappingMissingAndForeignOccurrenceRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := CheckSelectedProjectPrelaunch(ctx, nil, nil, nil); e == nil {
		t.Fatal("cancelled mapping")
	}
	a, b := &observedStartResult{}, &observedStartResult{}
	v := &ObservedPrelaunch{result: a, input: &RetainedTaskInput{store: &store.Store{}, observed: b}}
	if _, e := CheckSelectedProjectPrelaunch(context.Background(), &project.BaselineInputs{}, &project.CaptureSourceInputs{}, v); e == nil || !strings.Contains(e.Error(), "missing_actual_input") {
		t.Fatal("foreign occurrence")
	}
}

func TestRetainedTaskInputActualObjectManifestAndCopies(t *testing.T) {
	s, l := behaviorProject(t)
	base := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "cat\n"})
	result := evaltest.Tree(t, l, map[string]string{"go.mod": "module example\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	built, e := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Repo: s.Cfg.Repos[0].Name, Base: base, Result: result, Request: "Uppercase stdin.", RequestAt: map[string]any{"source": "sha256:" + strings.Repeat("a", 64), "record": "u1"}, Entry: []string{"sh", "tool.sh"}, Cases: []build.IOCase{{ID: "lower", Input: []byte("hello\n")}}, CaseSeconds: 5, Now: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	ref, e := l.PutManifest("eval", built.Record)
	if e != nil {
		t.Fatal(e)
	}
	digest, _ := l.ManifestDigest("eval", ref)
	var rec map[string]any
	l.Manifest("eval", ref, &rec)
	task, e := runner.ExportBounded(context.Background(), l, rec, filepath.Join(t.TempDir(), "task"), runner.DefaultSettings)
	if e != nil {
		t.Fatal(e)
	}
	task.Inputs["instruction"] = "mutable changed"
	cap, e := retainActualTaskInput(context.Background(), s, l, ref, digest, "su-actual", "jb-actual", "baseline", 1, rec, task)
	if e != nil {
		t.Fatal(e)
	}
	copy := cap.Identity()
	copy.Attempt = 99
	copy.Inputs.SourceInputs["instruction"] = "changed"
	if cap.Identity().Attempt != 1 || cap.Identity().Inputs.SourceInputs["instruction"] == "changed" {
		t.Fatal("mutable capability")
	}
	if _, e = json.Marshal(cap); e == nil {
		t.Fatal("wire capability")
	}
	if _, e = retainActualTaskInput(context.Background(), s, l, ref, "sha256:"+strings.Repeat("0", 64), "su-actual", "jb-actual", "baseline", 1, rec, task); e == nil {
		t.Fatal("foreign manifest")
	}
	if _, e = retainActualTaskInput(context.Background(), s, l, ref, digest, "su-actual", "jb-actual", "baseline", 0, rec, task); e == nil {
		t.Fatal("unbound attempt")
	}
	raw, _ := json.Marshal(rec)
	var other map[string]any
	json.Unmarshal(raw, &other)
	id, e := l.PutObject([]byte("other instruction"))
	if e != nil {
		t.Fatal(e)
	}
	other["instruction"] = id
	foreign, e := runner.ExportBounded(context.Background(), l, other, filepath.Join(t.TempDir(), "foreign"), runner.DefaultSettings)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = retainActualTaskInput(context.Background(), s, l, ref, digest, "su-actual", "jb-actual", "baseline", 1, rec, foreign); e == nil {
		t.Fatal("different actual exported instruction bound to manifest")
	}
}

func TestSelectedImplicitOccupancyAndComponentAliases(t *testing.T) {
	for _, kind := range []string{"absent_implicit", "absent_case_implicit", "absent_nfc_implicit", "present_parent_case_implicit", "present_parent_nfc_implicit", "present_self_implicit"} {
		t.Run(kind, func(t *testing.T) {
			s, inv := selectedFactsFixture()
			extra := "removed.go/child"
			switch kind {
			case "absent_case_implicit":
				extra = "REMOVED.GO/child"
			case "absent_nfc_implicit":
				s.Selected[1].Path = "caf\u00e9"
				extra = "cafe\u0301/child"
			case "present_parent_case_implicit":
				extra = "SRC/extra"
			case "present_parent_nfc_implicit":
				s.Selected[0].Path = "caf\u00e9/file.go"
				inv.Entries[0].Path = s.Selected[0].Path
				extra = "cafe\u0301/extra"
			case "present_self_implicit":
				inv.Entries = nil
				extra = "src/file.go/child"
			}
			inv.Entries = append(inv.Entries, runner.PrelaunchEntry{Path: extra, Type: tar.TypeReg, Mode: 0644, LogicalMode: 0100644, Size: 0, Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"})
			if _, _, _, e := selectedInventoryFacts(s, inv, false); e == nil {
				t.Fatal("implicit occupancy/alias accepted", kind)
			}
		})
	}
	// A genuinely absent path remains valid; no invented directory enters output.
	s, inv := selectedFactsFixture()
	_, absent, unmapped, e := selectedInventoryFacts(s, inv, false)
	if e != nil || len(absent) != 1 || absent[0] != "removed.go" || len(unmapped.Entries) != 0 {
		t.Fatal("real absence changed", e)
	}
}
