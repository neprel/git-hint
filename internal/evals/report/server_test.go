package report

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/api"
)

// TestUploadToServer sends a report built by the client to a running HINT server and
// checks the receipt, the repeat answer and a conflicting report. Set
// HINT_EVALREPORT_SERVER (base URL) and HINT_SERVER_TOKEN (a token for fixedNS).
func TestUploadToServer(t *testing.T) {
	url := os.Getenv("HINT_EVALREPORT_SERVER")
	if url == "" {
		t.Skip("set HINT_EVALREPORT_SERVER")
	}
	l, names := seeded(t)
	b, _, err := Builder{Key: fixedKey, Namespace: fixedNS}.Build(l, names, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	c, err := api.New(url)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.PostEvalReport(context.Background(), b, m["report_id"].(string), m["content_digest"].(string))
	if err != nil || (rc.Status != "accepted" && rc.Status != "same") {
		t.Fatalf("upload: %v %+v", err, rc)
	}
	rc, err = c.PostEvalReport(context.Background(), b, m["report_id"].(string), m["content_digest"].(string))
	if err != nil || rc.Status != "same" {
		t.Fatalf("repeat: %v %+v", err, rc)
	}
	b2, _, _ := Builder{Key: fixedKey, Namespace: fixedNS}.Build(l, names[:1], time.Now())
	var m2 map[string]any
	json.Unmarshal(b2, &m2)
	m2["report_id"] = m["report_id"] // same ID, other content
	delete(m2, "content_digest")
	if _, err := c.PostEvalReport(context.Background(), reseal(m2), m["report_id"].(string), ""); err == nil {
		t.Fatal("a conflicting report was accepted")
	}
}
