package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
)

func segmentServer(t *testing.T, status int, answer string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HINT_TEST_MODEL_KEY", "model-key")
	return srv, &calls
}

// E06 budget: one job per session with at most 2 requests including retries; a session's
// job runs once (resumable only as the rule episode); at most 10 sessions per pass; the
// answer and inputs are kept as the generator record.
func TestModelSegmenterBudget(t *testing.T) {
	l := evaltest.Library(t)
	policy := func(url string) *evals.Policy {
		p := evals.Enabled(nil)
		p.Model = &evals.Model{Use: "lan", BaseURL: url + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY", MaxRequestsPerJob: 9, MaxRetries: 5}
		return &p
	}
	reqs := []string{"Fix the discount bug in Total.", "Next task: give Name a default guest value."}

	srv, calls := segmentServer(t, http.StatusOK, `{"tasks": ["A", "B"], "families": {"A": "bug_fix", "B": "feature"}}`)
	seg := modelSegmenter(context.Background(), l, policy(srv.URL))
	tasks, fams, gen, ok := seg("ses_1", reqs)
	if !ok || fmt.Sprint(tasks) != "[A B]" || fams["B"] != "feature" || *calls != 1 || gen["requests"] != 1 || gen["output"] == nil || len(gen["inputs"].([]any)) != 2 {
		t.Fatalf("%v %v %v %v calls %d", tasks, fams, gen, ok, *calls)
	}
	if _, _, _, ok := seg("ses_1", reqs); ok || *calls != 1 {
		t.Fatalf("a session's job ran twice: calls %d", *calls)
	}

	// a failing model: 2 requests including the retry, then the rule episode stays
	bad, badCalls := segmentServer(t, http.StatusInternalServerError, "")
	seg = modelSegmenter(context.Background(), l, policy(bad.URL))
	if _, _, _, ok := seg("ses_2", reqs); ok || *badCalls != 2 {
		t.Fatalf("failing model: calls %d", *badCalls)
	}

	// a wrong-size answer is refused
	wrong, _ := segmentServer(t, http.StatusOK, `{"tasks": ["A"]}`)
	if _, _, _, ok := modelSegmenter(context.Background(), l, policy(wrong.URL))("ses_3", reqs); ok {
		t.Fatal("a wrong-size answer was accepted")
	}

	// at most 10 sessions per pass
	cap, capCalls := segmentServer(t, http.StatusOK, `{"tasks": ["A", "A"]}`)
	seg = modelSegmenter(context.Background(), l, policy(cap.URL))
	for i := 0; i < 11; i++ {
		seg(fmt.Sprintf("ses_cap_%d", i), reqs)
	}
	if *capCalls != SegmentSessions {
		t.Fatalf("pass cap: calls %d", *capCalls)
	}

	// no model in the policy: no segmenter
	p := evals.Enabled(nil)
	if modelSegmenter(context.Background(), l, &p) != nil {
		t.Fatal("a segmenter without the model policy")
	}
}
