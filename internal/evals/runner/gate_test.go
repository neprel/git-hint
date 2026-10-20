package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Q4 on the local engine (offline: local stubs and a sentinel resolver only; no model): the
// probe passes every positive control and every negative check, its key names the engine,
// the policy and the gate image, and it leaves no network or container behind.
func TestProbeGateOnTheEngine(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	ctx := context.Background()
	p := d.ProbeGate(ctx)
	b, _ := json.MarshalIndent(p, "", "  ")
	t.Logf("%s", b)
	if !p.OK || p.Key == "" || !strings.HasPrefix(p.Image, "sha256:") || len(p.Checks) < 20 {
		t.Fatalf("probe: %s", b)
	}
	nets, err := d.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets.Items {
		if n.Labels["hint"] == "q4" {
			t.Errorf("network %s left behind", n.Name)
		}
	}
}

// The per-trial statement: "enforced" only for a container with exactly the agent settings
// on A next to the policy's gate; a container with capabilities, on B, or without the gate
// DNS is "unverified".
func TestBypassStatement(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	ctx := context.Background()
	image, err := d.gateImage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g, err := d.openNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close(d)
	listen, hostEntry, err := d.proxyHost(ctx, g.BGateway)
	if err != nil {
		t.Fatal(err)
	}
	s, err := startStub(listen)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := d.startGate(ctx, g, image, hostEntry, s.port(), time.Minute); err != nil {
		t.Fatal(err)
	}
	mk := func(edit func(h *container.HostConfig, c *container.Config)) string {
		h, c := g.agentHost(), &container.Config{Image: image, User: "65534:65534", Cmd: []string{"-probe"}}
		edit(h, c)
		id, err := d.create(ctx, c, h)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.remove(id) })
		return id
	}
	if got := d.Bypass(ctx, g, mk(func(*container.HostConfig, *container.Config) {})); got != "enforced" {
		t.Fatalf("agent settings: %s", got)
	}
	for name, edit := range map[string]func(h *container.HostConfig, c *container.Config){
		"capabilities": func(h *container.HostConfig, c *container.Config) { h.CapDrop = nil },
		"root":         func(h *container.HostConfig, c *container.Config) { c.User = "0:0" },
		"network B":    func(h *container.HostConfig, c *container.Config) { h.NetworkMode = container.NetworkMode(g.B) },
		"other DNS":    func(h *container.HostConfig, c *container.Config) { h.DNS = nil },
	} {
		if got := d.Bypass(ctx, g, mk(edit)); got != "unverified" {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := d.Bypass(ctx, nil, ""); got != "unverified" {
		t.Errorf("no gate: %s", got)
	}
}
