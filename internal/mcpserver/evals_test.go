package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
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

func TestEvalToolsHoldNoTextAndObeyTheRules(t *testing.T) {
	shop, _ := project_(t)
	s, err := store.Open(shop)
	if err != nil {
		t.Fatal(err)
	}
	l := library.Open(s, 0)
	if _, err := l.PutManifest("eval", evaltest.GoBugFix(t, l)); err != nil {
		t.Fatal(err)
	}
	cs := session(t, mcpserver.Options{Dir: shop})
	out := mustCall(t, cs, "hint_evals", map[string]any{})
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"state":"packaged"`) || strings.Contains(string(raw), "discount") || strings.Contains(string(raw), "sha256:") {
		t.Fatalf("hint_evals: %s", raw)
	}
	if _, msg := call(t, cs, "hint_eval_run", map[string]any{"agent": "oracle"}); !strings.Contains(msg, "eval_run_denied") {
		t.Fatalf("run without the MCP rule: %q", msg)
	}
	gh(t, shop, "init", "--mcp-eval-run", "allow")
	cs = session(t, mcpserver.Options{Dir: shop})
	if _, msg := call(t, cs, "hint_eval_run", map[string]any{"agent": "oracle"}); !strings.Contains(msg, "eval_not_allowed") {
		t.Fatalf("run without the eval policy: %q", msg)
	}
}
