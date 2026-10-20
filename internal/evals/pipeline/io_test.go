package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/build"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
)

// 07B-J §19: the qualification worker with the current runner never runs a cli-file-v1 eval;
// it stays packaged (runner_unsupported) and no trial is spent on it.
func TestQualifyLeavesB1PackagedWithoutTheContainerRunner(t *testing.T) {
	s, env := e2eProject(t)
	p := evals.Enabled([]string{"opencode"})
	p.Run = &evals.Run{Use: "docker", Runner: "docker"}
	s.Cfg.Evals = &p
	l := library.Open(s, 0)
	base := evaltest.Tree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "cat\n"})
	ref := evaltest.Tree(t, l, map[string]string{"go.mod": "module shop\n\ngo 1.26\n", "tool.sh": "tr a-z A-Z\n"})
	b, err := build.BuildIO(l, build.InputIO{EpisodeID: "ep-0123456789abcdef", Repo: "shop", Base: base, Result: ref, Request: "upper case",
		RequestAt: map[string]any{"source": library.Digest([]byte("r")), "record": "u1"}, Entry: []string{"sh", "tool.sh"},
		Cases: []build.IOCase{{ID: "a", Input: []byte("x\n")}}, CaseSeconds: 5, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	name, err := l.PutManifest("eval", b.Record)
	if err != nil {
		t.Fatal(err)
	}
	r := &e2eRunner{}
	q := Qualify(context.Background(), s, r, env.Now())
	if q.States["runner_unsupported"] != 1 || q.Trials != 0 {
		t.Fatalf("qualify %+v", q)
	}
	if names, _ := l.Manifests("eval"); len(names) != 1 || names[0] != name {
		t.Fatalf("a new version was written: %v", names)
	}
}
