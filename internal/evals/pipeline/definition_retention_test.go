package pipeline

import (
	"bytes"
	"context"
	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
	"os"
	"testing"
	"time"
)

// The normal AfterCollect call arrives after CLI/MCP releases the state lock.
// This uses the existing pure command fixture; no server, agent or engine starts.
func TestNormalPipelinePrunesWithoutDefinitionReferences(t *testing.T) {
	s, env, _ := setup(t)
	p := evals.Enabled([]string{"opencode"})
	s.Cfg.Evals = &p
	l := library.Open(s, 0)
	old := evaltest.GoBugFix(t, l)
	old["created_at"] = "2000-01-01T00:00:00Z"
	name, e := l.PutManifest("eval", old)
	if e != nil {
		t.Fatal(e)
	}
	env.Now = func() time.Time { return time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) }
	got := Run(context.Background(), s, env)
	if !got.Ran || got.Pruned < 1 {
		t.Fatalf("ordinary pipeline retention skipped: %+v", got)
	}
	var after map[string]any
	if e = l.Manifest("eval", name, &after); e == nil {
		t.Fatal("old manifest survived normal pipeline prune")
	}
}

func TestPipelineNeverDeletesSessionIndexBeforeDefinitionBarrier(t *testing.T) {
	for _, kind := range []string{"directory", "corrupt-file", "link", "busy"} {
		t.Run(kind, func(t *testing.T) {
			s, env, _ := setup(t)
			p := evals.Enabled([]string{"opencode"})
			s.Cfg.Evals = &p
			first := Run(context.Background(), s, env)
			if len(first.Imports) == 0 || first.Imports[0].New != 1 {
				t.Fatalf("actual import fixture did not create index: %+v", first)
			}
			l := library.Open(s, 0)
			name := "opencode/ses_synth_main_0001"
			var before sessions.IndexEntry
			if e := l.Index(name, &before); e != nil {
				t.Fatal(e)
			}
			raw, e := os.ReadFile(s.Path("state", "evals", "index", "opencode", "ses_synth_main_0001.json"))
			if e != nil {
				t.Fatal(e)
			}
			path := s.Path("state", library.WorkDefinitionStateDir)
			switch kind {
			case "directory":
				e = os.MkdirAll(path, 0700)
			case "corrupt-file":
				e = os.WriteFile(path, []byte("broken"), 0600)
			case "link":
				e = os.Symlink(t.TempDir(), path)
			case "busy":
				lock, err := s.TryLockStateContext(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Unlock()
			}
			if e != nil {
				t.Fatal(e)
			}
			cutoff := env.Now().Add(100 * 24 * time.Hour)
			if n := pruneSessions(l, cutoff); n != 0 {
				t.Fatal("pipeline deleted before guard", n)
			}
			// Exercise the normal complete pipeline too when it does not encounter genuine
			// external ownership. The busy case invokes the same actual pruneSessions caller.
			if kind != "busy" {
				later := env
				later.Now = func() time.Time { return cutoff }
				got := Run(context.Background(), s, later)
				if got.Pruned != 0 {
					t.Fatal("normal pipeline reported deletion", got.Pruned)
				}
			}
			after, e := os.ReadFile(s.Path("state", "evals", "index", "opencode", "ses_synth_main_0001.json"))
			if e != nil || !bytes.Equal(raw, after) {
				t.Fatal("session index changed before guard", e)
			}
			if _, e = l.GetObject(before.Normalized); e != nil {
				t.Fatal("normalized session removed before guard", e)
			}
		})
	}
}
