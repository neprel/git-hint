package mcpserver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/mcpserver"
	"github.com/neprel/git-hint/internal/store"
)

type noAgents struct{}

func (noAgents) Output(context.Context, string, string, ...string) ([]byte, error) {
	return nil, errors.New("not installed")
}

// hint_collect runs the eval pass only under the user's policy and reports counts; it
// never starts a worker when container runs are off.
func TestCollectEvalsFollowThePolicy(t *testing.T) {
	shop, _ := project_(t)
	opts := mcpserver.Options{Dir: shop,
		EvalEnv: func() pipeline.Env {
			return pipeline.Env{Home: t.TempDir(), DataHome: t.TempDir(), Cmd: noAgents{}, Now: time.Now}
		},
		Spawn: func() error { t.Error("worker started"); return nil }}
	cs := session(t, opts)
	c := mustCall(t, cs, "hint_collect", map[string]any{"full": true})
	if _, ok := c["evals"]; ok {
		t.Fatalf("evals reported without a policy: %v", c["evals"])
	}
	s, err := store.Open(shop)
	if err != nil {
		t.Fatal(err)
	}
	p := evals.Enabled(nil)
	s.Cfg.Evals = &p
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	c = mustCall(t, cs, "hint_collect", map[string]any{})
	ev, ok := c["evals"].(map[string]any)
	if !ok || ev["ran"] != true || ev["worker"] != "none" {
		t.Fatalf("evals: %v", c["evals"])
	}
}
