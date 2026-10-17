package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/evaltest"
)

// TestHarborLANNetwork checks the live network setup without a model: in LAN-model mode
// the agent phase must reach HINT's proxy (which forwards to a fake upstream here) and
// nothing else. Set HINT_HARBOR to run it; it needs Docker with Harbor's egress sidecar.
func TestHarborLANNetwork(t *testing.T) {
	if os.Getenv("HINT_HARBOR") == "" {
		t.Skip("set HINT_HARBOR to the harbor executable")
	}
	ctx := context.Background()
	h, err := FindHarbor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"fake":"upstream"}`) }))
	defer up.Close()
	t.Setenv("HINT_TEST_LAN_KEY", "test-key")
	lv, err := StartLive(LiveConfig{BaseURL: up.URL + "/v1", Model: "m", KeyEnv: "HINT_TEST_LAN_KEY", AgentVersion: "2.0.26", Listen: os.Getenv("HINT_TEST_LISTEN")})
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	c := lv.NewTrial(5)
	l := evaltest.Library(t)
	rec := evaltest.GoBugFix(t, l)
	task := filepath.Join(t.TempDir(), "task")
	if _, err := Export(l, rec, task, Settings{Mode: LANModel, LANHost: lv.ContainerHost, AgentSeconds: 300, VerifierSeconds: 300, BuildSeconds: 900, CPUs: 2, MemoryMB: 2048, Pids: 512}); err != nil {
		t.Fatal(err)
	}
	probe := `#!/bin/sh
proxy=no; other=no
if curl -s -m 10 -X POST -H 'Content-Type: application/json' -d '{}' "` + lv.BaseURL(c) + `/chat/completions" | grep -q fake; then proxy=yes; fi
if curl -s -m 10 https://example.com/ >/dev/null 2>&1; then other=yes; fi
echo "HINT_NET proxy=$proxy other=$other"
`
	os.WriteFile(filepath.Join(task, "solution", "solve.sh"), []byte(probe), 0o755)
	jobs := t.TempDir()
	trials, err := h.Run(ctx, task, RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "lan", Timeout: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := filepath.Glob(filepath.Join(jobs, "lan", "*", "agent", "oracle.txt"))
	var out string
	for _, m := range ms {
		b, _ := os.ReadFile(m)
		out += string(b)
	}
	t.Logf("trial %+v; probe %q; proxy requests %d", trials, strings.TrimSpace(out), c.Requests())
	if !strings.Contains(out, "HINT_NET proxy=yes other=no") || c.Requests() != 1 {
		t.Fatalf("network: %q (requests %d)", out, c.Requests())
	}
}
