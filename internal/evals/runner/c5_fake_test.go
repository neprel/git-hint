package runner

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// C7BJ-11/12 controls on a synthetic engine API (no engine, no model).

const c5Image = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func frame(kind byte, s string) string {
	h := make([]byte, 8)
	h[0] = kind
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return string(h) + s
}

// c5Engine answers a toolchain probe with logs (status, body) and a build with body.
func c5Engine(t *testing.T, logStatus int, logs, build string) *Docker {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			fmt.Fprint(w, `{"Id":"c"}`)
		case strings.HasSuffix(p, "/containers/c/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":"c","Image":%q}`, c5Image)
		case strings.HasSuffix(p, "/start"):
			w.WriteHeader(204)
		case strings.HasSuffix(p, "/wait"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"StatusCode":0,"Error":null}`)
		case strings.HasSuffix(p, "/logs"):
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			w.WriteHeader(logStatus)
			io.WriteString(w, logs)
		case strings.HasSuffix(p, "/build"):
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, build)
		case strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":%q}`, c5Image)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	c, err := client.New(client.WithHost(s.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Docker{cli: c}
}

func TestToolchainCaptureMustBeComplete(t *testing.T) {
	for _, c := range []struct {
		name, logs, state, reason string
		status                    int
	}{
		{"complete", frame(1, "go version go1.26.7 linux/arm64\n"), "observed", "", 200},
		{"stderr counts in the budget", frame(2, "Python 3.13.15\n"), "observed", "", 200},
		{"capture failure", `{"message":"synthetic"}`, "unknown", "output_unavailable", 500},
		{"cut frame", frame(1, "go version")[:12], "unknown", "output_incomplete", 200},
		{"over one budget in two streams", frame(1, strings.Repeat("a", 3000)) + frame(2, strings.Repeat("b", 3000)), "unknown", "output_truncated", 200},
		{"empty", "", "unknown", "output_empty", 200},
	} {
		p := c5Engine(t, c.status, c.logs, "").probeToolchain(context.Background(), c5Image, []string{"go", "version"})
		if p["state"] != c.state || c.reason != "" && p["reason"] != c.reason || c.state == "unknown" && p["output"] != nil {
			t.Errorf("%s: %v", c.name, p)
		}
	}
}

func TestBuildStreamMustBeComplete(t *testing.T) {
	aux := `{"aux":{"ID":"` + c5Image + `"}}` + "\n"
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"normal", `{"stream":"Step 1/1"}` + "\n" + aux + `{"stream":"done"}` + "\n", true},
		{"overflow after aux", aux + strings.Repeat("x", 4<<20+1) + "\n", false},
		{"malformed JSON after aux", aux + "not json\n", false},
		{"error message", aux + `{"error":"failed"}` + "\n", false},
		{"no aux", `{"stream":"Step 1/1"}` + "\n", false},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+baseImage+"\n"), 0o600)
		id, _, _, err := c5Engine(t, 200, "", c.body).build(context.Background(), dir, "agent")
		if (err == nil) != c.ok || c.ok && id != c5Image {
			t.Errorf("%s: %q %v", c.name, id, err)
		}
	}
}
