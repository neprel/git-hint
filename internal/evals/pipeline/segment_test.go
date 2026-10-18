package pipeline

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/episodes"
	"github.com/neprel/git-hint/internal/evals/evaltest"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
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
	// G10-8: the same session again, also from a new segmenter of a later pass, replays the
	// kept answer without a model call
	again, _, gen2, ok := seg("ses_1", reqs)
	later, _, gen3, ok3 := modelSegmenter(context.Background(), l, policy(srv.URL))("ses_1", reqs)
	if !ok || !ok3 || fmt.Sprint(again, later) != "[A B] [A B]" || *calls != 1 || fmt.Sprint(gen2) != fmt.Sprint(gen) || fmt.Sprint(gen3) != fmt.Sprint(gen) {
		t.Fatalf("replay: %v %v calls %d", again, later, *calls)
	}

	// a failing model: 2 requests including the retry, then the rule episode stays; the
	// failure is not kept as an answer (a later pass does not replay it or call again)
	bad, badCalls := segmentServer(t, http.StatusInternalServerError, "")
	seg = modelSegmenter(context.Background(), l, policy(bad.URL))
	if _, _, _, ok := seg("ses_2", reqs); ok || *badCalls != 2 {
		t.Fatalf("failing model: calls %d", *badCalls)
	}
	if _, _, _, ok := modelSegmenter(context.Background(), l, policy(bad.URL))("ses_2", reqs); ok || *badCalls != 2 {
		t.Fatalf("a failure was replayed or retried: calls %d", *badCalls)
	}

	// G10-11: a stricter policy limit wins (1 request, no retry)
	strict := policy(bad.URL)
	strict.Model.MaxRequestsPerJob, strict.Model.MaxRetries = 1, 0
	if _, _, _, ok := modelSegmenter(context.Background(), l, strict)("ses_strict", reqs); ok || *badCalls != 3 {
		t.Fatalf("stricter policy: calls %d", *badCalls)
	}
	// G10-11: no job starts after the batch context ends
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, ok := modelSegmenter(ctx, l, policy(srv.URL))("ses_cancelled", reqs); ok || *calls != 1 {
		t.Fatalf("a cancelled pass started a job: calls %d", *calls)
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

// importCase imports the sessions of a corpus case; it returns the snapshot resolver.
func importCase(t *testing.T, l *library.Library, dir string) (time.Time, episodes.Snapshots) {
	t.Helper()
	var c struct {
		Now       int64                        `json:"now"`
		Snapshots map[string]map[string]string `json:"snapshots"`
		Sessions  []map[string]any             `json:"sessions"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil || json.Unmarshal(b, &c) != nil {
		t.Fatal(err)
	}
	snap := func(name string) string { h := sha1.Sum([]byte("snap:" + name)); return hex.EncodeToString(h[:]) }
	trees := map[string]string{}
	for name := range c.Snapshots {
		trees[snap(name)] = library.Digest([]byte("tree:" + name))
	}
	for _, sess := range c.Sessions {
		parent, _ := sess["_parent"].(string)
		delete(sess, "_parent")
		raw, _ := json.Marshal(sess)
		src := string(raw)
		for name := range c.Snapshots {
			src = strings.ReplaceAll(src, `"SNAP:`+name+`"`, `"`+snap(name)+`"`)
		}
		r, err := sessions.ParseOpenCode([]byte(src), sessions.OpenCodeOptions{Version: "2.0.26", Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.Import(l, []byte(src), r); err != nil {
			t.Fatal(err)
		}
	}
	return time.UnixMilli(c.Now), func(_, id string) string { return trees[id] }
}

// G10-8: a split session stays split in later passes: the second discovery (a new
// segmenter, as after a restart) replays the kept answer without a model call and gives
// the same episode IDs and provenance; storing it adds no candidate.
func TestSegmentationReplayAcrossPasses(t *testing.T) {
	l := evaltest.Library(t)
	now, snaps := importCase(t, l, filepath.Join("..", "..", "..", "tests", "evals", "corpus", "heldout-seg", "two-tasks-no-check-between"))
	srv, calls := segmentServer(t, http.StatusOK, `{"tasks": ["A", "B"], "families": {"A": "bug_fix", "B": "feature"}}`)
	p := evals.Enabled(nil)
	p.Model = &evals.Model{Use: "lan", BaseURL: srv.URL + "/v1", Model: "local-coder", KeyEnv: "HINT_TEST_MODEL_KEY"}
	repos := func(dir string) string {
		if dir == "/work/shop" {
			return "shop"
		}
		return ""
	}
	pass := func() ([]string, string) {
		props, err := episodes.DiscoverWith(l, now, snaps, repos, modelSegmenter(context.Background(), l, &p))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		gens := ""
		for _, pr := range props {
			ids = append(ids, pr.Record["episode_id"].(string))
			gens += fmt.Sprint(pr.Record["generator"])
		}
		stored, _, err := episodes.Store(l, props)
		if err != nil {
			t.Fatal(err)
		}
		return append(ids, fmt.Sprint("stored=", stored)), gens
	}
	first, g1 := pass()
	second, g2 := pass()
	if len(first) != 3 || first[0] == first[1] || g1 != g2 || *calls != 1 {
		t.Fatalf("first %v second %v calls %d", first, second, *calls)
	}
	if fmt.Sprint(first[:2]) != fmt.Sprint(second[:2]) || second[2] != "stored=0" {
		t.Fatalf("the second pass changed the episodes: %v vs %v", first, second)
	}
}
