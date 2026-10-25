package runner

import (
	"context"
	"encoding/json"
	"github.com/moby/moby/client"
	"os"
	"runtime"
	"testing"
	"time"
)

// Real local routing with synthetic stubs only. The Linux test process must run
// in the daemon host network namespace; an ordinary bridge container must refuse.
func TestProbeGateFromLinuxCaller(t *testing.T) {
	if os.Getenv("HINT_DOCKER_TESTS") != "1" || runtime.GOOS != "linux" {
		t.Skip("opt-in Linux caller network check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	d, err := FindDocker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// No image download: the gate's fixed builder must already exist locally.
	if _, err = d.cli.ImageInspect(ctx, gateBuilder); err != nil {
		t.Fatalf("cached gate builder unavailable: %v", err)
	}
	before, err := d.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	priorContainers, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	knownContainers := map[string]bool{}
	for _, c := range priorContainers.Items {
		knownContainers[c.ID] = true
	}
	known := map[string]bool{}
	for _, n := range before.Items {
		known[n.ID] = true
	}
	p := d.ProbeGate(ctx)
	raw, _ := json.Marshal(p)
	t.Log(string(raw))
	clean, cc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cc()
	after, err := d.cli.NetworkList(clean, client.NetworkListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range after.Items {
		if !known[n.ID] && n.Labels["hint"] == "q4" {
			t.Errorf("new probe network remains: %s", n.ID)
		}
	}
	remaining, err := d.cli.ContainerList(clean, client.ContainerListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range remaining.Items {
		if !knownContainers[c.ID] && c.ImageID == p.Image {
			t.Errorf("new probe container remains: %s", c.ID)
		}
	}
	if !p.OK || len(p.Checks) < 20 {
		t.Fatalf("Linux caller probe refused: %s", raw)
	}
}
