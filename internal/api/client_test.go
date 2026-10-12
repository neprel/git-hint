package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerAddressRules(t *testing.T) {
	for _, bad := range []string{"", "http://example.invalid", "ftp://h", "https://u:p@h", "https://h/?q=1", "https://h/#f", "https:///x"} {
		if _, err := New(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"https://hint.example.invalid", "https://h:8443/base/", "http://127.0.0.1:9", "http://localhost:1", "http://[::1]:2"} {
		if _, err := New(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

func TestNoRedirectWrongTypeAndRetryableStatus(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/hint/v0.1/recommendations":
			switch r.URL.Query().Get("project_namespace") {
			case "redirect":
				http.Redirect(w, r, "https://other.invalid/steal", http.StatusFound)
			case "html":
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte("<html>"))
			case "down":
				http.Error(w, "x", http.StatusServiceUnavailable)
			}
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Recommendations(ctx, "redirect"); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("redirect followed or accepted: %v", err)
	}
	if hits != 1 {
		t.Errorf("redirect target requested: %d hits", hits)
	}
	if _, err := c.Recommendations(ctx, "html"); err == nil || !strings.Contains(err.Error(), "not a HINT document") {
		t.Errorf("wrong content type accepted: %v", err)
	}
	if _, err := c.Recommendations(ctx, "down"); !errors.Is(err, ErrNetwork) {
		t.Errorf("503 must be retryable: %v", err)
	}
	if _, err := c.PostDocument(ctx, []byte("{}"), "x", "sha256:00"); err == nil {
		t.Error("missing receipt accepted")
	}
}
