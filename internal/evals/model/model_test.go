package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
)

func policy(url string) *evals.Policy {
	p := evals.Enabled(nil)
	p.Model = &evals.Model{Use: "lan", BaseURL: url + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY", MaxRequestsPerJob: 2, MaxRetries: 1, MaxSecondsPerJob: 30}
	return &p
}

func TestBudgetAndRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer model-key" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("HINT_TEST_MODEL_KEY", "model-key")
	l := evaltest.Library(t)
	c, err := New(policy(srv.URL), l, "jb-0000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, 10); err == nil {
		t.Fatal("a failing model gave an answer")
	}
	if calls.Load() != 2 { // one request and one retry, as the policy allows
		t.Fatalf("calls %d", calls.Load())
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, 10); !errors.Is(err, ErrBudget) {
		t.Fatalf("beyond the budget: %v (calls %d)", err, calls.Load())
	}
	if _, err := New(policy(srv.URL), l, "jb-0000000000000001"); err == nil {
		t.Fatal("a restart got a new budget for the same job")
	}
}

func TestDraftValidation(t *testing.T) {
	d := GoTestDraft{Package: "shop", Module: "shop"}
	ok := "package shop\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) { if Total(10, 3) != 7 { t.Fatal() } }\n"
	if err := d.Validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"other package": strings.Replace(ok, "package shop", "package other", 1),
		"os/exec":       strings.Replace(ok, `import "testing"`, "import (\n\t\"os/exec\"\n\t\"testing\"\n)", 1) + "var _ = exec.Command\n",
		"foreign":       strings.Replace(ok, `import "testing"`, "import (\n\t\"example.com/x\"\n\t\"testing\"\n)", 1) + "var _ = x.Y\n",
		"no test":       "package shop\n\nfunc helper() {}\n",
		"not go":        "Here is the test you asked for.",
	} {
		if d.Validate(bad) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDraftFromFencedAnswer(t *testing.T) {
	draft := "package shop\n\nimport \"testing\"\n\nfunc TestDiscount(t *testing.T) {\n\tif Total(10, 3) != 7 {\n\t\tt.Fatal(\"total\")\n\t}\n}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "local-coder" || req["temperature"].(float64) != 0 {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Sure:\n```go\n" + draft + "```\n"}}}})
	}))
	defer srv.Close()
	t.Setenv("HINT_TEST_MODEL_KEY", "model-key")
	l := evaltest.Library(t)
	c, err := New(policy(srv.URL), l, "jb-0000000000000002")
	if err != nil {
		t.Fatal(err)
	}
	got, err := GoTestDraft{Package: "shop", Module: "shop", Request: "fix", Before: evaltest.GoBug, After: evaltest.GoFix}.Generate(context.Background(), c)
	if err != nil || got != draft {
		t.Fatalf("%v %q", err, got)
	}
}
