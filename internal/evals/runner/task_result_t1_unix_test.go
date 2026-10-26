//go:build unix

package runner_test

// T1 regressions over the ACTUAL final TAR, SDK callbacks and project resolver.
// Only owned synthetic sentinels and stub SDKs participate; no engine or model.
import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/project"
)

func terminalT1Resolve(t *testing.T, f mappingFixture, final []runner.SelectedMappingTestNode) (*project.SelectedResultInputs, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	live, file := runner.SelectedMappingTestLive(t)
	r := &runner.SelectedMappingTestRunner{T: t, Nodes: terminalGitNodes(t, f.s.Root), FinalNodes: final}
	var mapping *pipeline.SelectedProjectMapping
	var result *pipeline.SelectedProjectResult
	var callbackErr error
	_, e := pipeline.RunSuite(ctx, f.s, r, runner.DockerRunnerSemver, pipeline.SuiteOptions{Agent: "opencode", Attempts: 1, Live: live, Evals: []string{f.eval}, Arm: "baseline", StudyLaunchPolicy: runner.StudyLaunchSingleRootV1, CapturePrelaunchTree: true, OpenCodeTgz: file,
		PrelaunchCheck: func(ctx context.Context, v *pipeline.ObservedPrelaunch) error {
			mapping, callbackErr = pipeline.CheckSelectedProjectPrelaunch(ctx, f.baseline, f.source, v)
			return callbackErr
		},
		ResultCheck: func(ctx context.Context, v *pipeline.ObservedProjectResult) error {
			result, callbackErr = pipeline.CheckSelectedProjectResult(ctx, mapping, v)
			return callbackErr
		}}, time.Now())
	if e != nil || callbackErr != nil || result == nil || !result.Ready() {
		t.Fatalf("actual custody refused error=%v callback=%v", e, callbackErr)
	}
	return result.ResolveSelectedInputs(ctx, trees.Limits{})
}

func terminalConfigWith(t *testing.T, f mappingFixture, extra string) string {
	t.Helper()
	config, e := os.ReadFile(filepath.Join(f.s.Root, ".git/config"))
	if e != nil {
		t.Fatal(e)
	}
	return string(config) + extra
}

// Inverts the unsafe a8 reproduction: an agent-authored local excludesFile that
// names an owned outside sentinel refuses; nothing is read into the library.
func TestTerminalT1OutsideExcludeRefused(t *testing.T) {
	for _, kind := range []string{"absolute", "home"} {
		t.Run(kind, func(t *testing.T) {
			f := newMappingFixture(t)
			sentinel := []byte("untracked-hidden.go\nSYNTHETIC-OUTSIDE-SENTINEL-NOT-A-REAL-SECRET\n")
			outside := filepath.Join(os.Getenv("HOME"), "terminal-outside-excludes")
			if e := os.WriteFile(outside, sentinel, 0600); e != nil {
				t.Fatal(e)
			}
			configured := outside
			if kind == "home" {
				configured = "~/terminal-outside-excludes"
			}
			final := []runner.SelectedMappingTestNode{{Path: ".git/config", Body: terminalConfigWith(t, f, "\n[core]\n\texcludesFile = "+configured+"\n"), Mode: 0644}, {Path: "untracked-hidden.go", Body: "package hidden\n", Mode: 0644}}
			selected, e := terminalT1Resolve(t, f, final)
			if e == nil || selected != nil {
				t.Fatal("agent-authored outside excludesFile admitted")
			}
			if _, e := f.l.GetObject(library.Digest(sentinel)); e == nil {
				t.Fatal("outside sentinel retained")
			}
		})
	}
}

// A NONEMPTY retained Native global exclude is replayed at Git's own global slot.
func TestTerminalT1RetainedGlobalExcludePositive(t *testing.T) {
	mappingGlobalExclude = []byte("*.log\n")
	defer func() { mappingGlobalExclude = nil }()
	f := newMappingFixture(t)
	selected, e := terminalT1Resolve(t, f, []runner.SelectedMappingTestNode{{Path: "main.go", Body: "package main\nfunc Value()int{return 2}\n", Mode: 0644}, {Path: "debug.log", Body: "synthetic log\n", Mode: 0644}, {Path: "added.go", Body: "package main\n", Mode: 0644}})
	if e != nil {
		t.Fatal("supported nonempty global replay refused", e)
	}
	id := selected.Identity()
	rel := id.Relation
	if rel.ReplayState != trees.ArtifactReplayNativePresent || rel.ReplayDigest != library.Digest([]byte("*.log\n")) || rel.State != "native_policy_correspondence_observed" || rel.Authority != "unknown" || rel.Limitation == "" || rel.NativeInvocation == rel.ArtifactInvocation || rel.NativeInvocation != f.source.Identity().OuterInvocation {
		t.Fatalf("relation %+v", rel)
	}
	if id.SourceView.Version != trees.SourceViewArtifactVersion || len(id.SourceView.CandidatePaths) != 0 {
		t.Fatal("not the artifact profile")
	}
	for _, fact := range id.SourceView.Facts {
		if fact.Role == "global_exclude" || fact.Role == "global_config" || fact.Role == "system_config" {
			t.Fatal("host source in artifact receipt", fact)
		}
	}
	paths := map[string]bool{}
	for _, n := range id.Selected.Selected {
		paths[n.Path] = true
	}
	if paths["debug.log"] || !paths["added.go"] || !paths["main.go"] {
		t.Fatal("global replay selection differs", paths)
	}
	extra := false
	for _, n := range selected.UnmappedInventory().Entries {
		extra = extra || n.Path == "debug.log"
	}
	if !extra {
		t.Fatal("excluded file hidden from unmapped full inventory")
	}
}

// Changed or unproved policy sources refuse composition; the native closure is
// never rewritten to pass.
func TestTerminalT1PolicyChangeRefusesComposition(t *testing.T) {
	cases := map[string]func(mappingFixture) []runner.SelectedMappingTestNode{
		"new_root_gitignore": func(mappingFixture) []runner.SelectedMappingTestNode {
			return []runner.SelectedMappingTestNode{{Path: ".gitignore", Body: "added.go\n", Mode: 0644}, {Path: "added.go", Body: "package main\n", Mode: 0644}}
		},
		"unproved_directory": func(mappingFixture) []runner.SelectedMappingTestNode {
			return []runner.SelectedMappingTestNode{{Path: "newdir/x.go", Body: "package newdir\n", Mode: 0644}}
		},
		"info_exclude_changed": func(mappingFixture) []runner.SelectedMappingTestNode {
			return []runner.SelectedMappingTestNode{{Path: ".git/info/exclude", Body: "added.go\n", Mode: 0644}, {Path: "added.go", Body: "package main\n", Mode: 0644}}
		},
		"ignorecase_changed": func(f mappingFixture) []runner.SelectedMappingTestNode {
			body := strings.Replace(terminalConfigWith(t, f, ""), "ignorecase = true", "ignorecase = false", 1)
			if !strings.Contains(body, "ignorecase = false") {
				body += "\n[core]\n\tignorecase = false\n"
			}
			return []runner.SelectedMappingTestNode{{Path: ".git/config", Body: body, Mode: 0644}}
		},
	}
	for name, final := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMappingFixture(t)
			selected, e := terminalT1Resolve(t, f, final(f))
			if !errors.Is(e, project.ErrSelectedResultRelation) || selected != nil {
				t.Fatal("unproved policy relation admitted", e)
			}
		})
	}
}
