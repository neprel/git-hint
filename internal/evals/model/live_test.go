package model

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/evaltest"
)

// TestLiveDraft asks the owner's LAN model for one test draft of the synthetic bug fix
// (at most 2 requests). Set HINT_LIVE_MODEL_URL (private address), HINT_LIVE_MODEL and
// HINT_LOCAL_MODEL_KEY. HINT_LIVE_DRAFT_OUT keeps the draft.
func TestLiveDraft(t *testing.T) {
	url := os.Getenv("HINT_LIVE_MODEL_URL")
	if url == "" {
		t.Skip("set HINT_LIVE_MODEL_URL")
	}
	p := evals.Enabled(nil)
	p.Model = &evals.Model{Use: "lan", BaseURL: url, Model: os.Getenv("HINT_LIVE_MODEL"), KeyEnv: "HINT_LOCAL_MODEL_KEY",
		MaxRequestsPerJob: 2, MaxRetries: 1, MaxSecondsPerJob: 300}
	if err := p.Check(evals.OpModel, ""); err != nil {
		t.Fatal(err)
	}
	l := evaltest.Library(t)
	c, err := New(&p, l, "jb-11ce0000000000aa")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	d := GoTestDraft{Package: "shop", Module: "shop", Request: "Total ignores the discount. Fix Total so it subtracts the discount from the price.",
		Before: evaltest.GoBug, After: evaltest.GoFix}
	src, err := d.Generate(context.Background(), c)
	st, _ := l.Job("jb-11ce0000000000aa")
	t.Logf("requests %d retries %d seconds %.1f wall %s", st.Requests, st.Retries, st.Seconds, time.Since(start).Round(time.Second))
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if out := os.Getenv("HINT_LIVE_DRAFT_OUT"); out != "" {
		os.WriteFile(out, []byte(src), 0o600)
	}
	if st.Requests > 2 {
		t.Fatalf("budget exceeded: %d requests", st.Requests)
	}
}
