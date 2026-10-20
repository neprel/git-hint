package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Q4 model gate (07B-J §20 with the reviewer's corrections): an agent container reaches one
// destination only, the model proxy, through a gate container. The agent network A is an
// internal bridge with an isolated IPv4 gateway (no bridge address, so no gateway or host
// service is reachable) and without IPv6; the agent's DNS is the gate address (no upstream
// resolver), it runs as 65534 without capabilities and with no-new-privileges. The gate is
// on A and on its own bridge B; it listens on its A address only, forwards to one target
// resolved once, has IP forwarding off, no capabilities and a read-only root. The probe runs
// negative tests with positive controls against local stubs and a sentinel resolver only;
// "enforced" needs a passed probe for the same engine, policy and gate image and a passed
// inspection of each trial's topology. Nothing changes host firewall or daemon settings.

//go:embed gate/main.go
var gateSource []byte

// GatePolicy is the version of the network policy (part of the probe cache identity).
const GatePolicy = "q4-net-v1"

// gatePolicyText is what the policy enforces (recorded with every probe).
const gatePolicyText = "agent: internal bridge A, gateway_mode_ipv4=isolated, IPv6 off, DNS = gate A address, " +
	"CapDrop ALL, uid 65534, no-new-privileges, no mounts; gate: A+B, one listener on its A address, one target resolved once, " +
	"ip_forward 0, CapDrop ALL, read-only root"

// gateBuilder is the pinned Go image that compiles the gate (the Go image of build.Images).
const gateBuilder = "golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514"

// gateImage builds the gate from its embedded source into a scratch image.
func (d *Docker) gateImage(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "hint-gate-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	files := map[string]string{
		"main.go": string(gateSource),
		"go.mod":  "module hintgate\n\ngo 1.26\n",
		"Dockerfile": "FROM " + gateBuilder + " AS build\nWORKDIR /src\nCOPY go.mod main.go ./\n" +
			"RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /gate .\nFROM scratch\nCOPY --from=build /gate /gate\n" +
			"USER 65534:65534\nENTRYPOINT [\"/gate\"]\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			return "", err
		}
	}
	id, _, _, err := d.build(ctx, dir, "gate")
	return id, err
}

// gateNet is the network topology of one gated run.
type gateNet struct {
	A, B     string // network names
	ASubnet  string
	BGateway string
	Gate     string // gate container ID
	GateIP   string // the gate's address in A
	Image    string // gate image ID
	Listen   string // as the gate reported it
	Target   string // the resolved target as the gate reported it
	ProxyIP  string // the host address the proxy listens on
}

var errGate = errors.New("the model gate is not available on this engine")

func randName() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "hint-q4-" + hex.EncodeToString(b)
}

// proxyHost is the minimal host address that the gate can reach the proxy on: loopback on
// Docker Desktop and OrbStack (host-gateway reaches the host's loopback there), the bridge
// B gateway on a local native Linux engine; any other engine is refused (remote engines are
// not assumed).
func (d *Docker) proxyHost(ctx context.Context, bGateway string) (listen, hostEntry string, err error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", "", errGate
	}
	osName := info.Info.OperatingSystem
	switch {
	case strings.Contains(osName, "OrbStack") || strings.Contains(osName, "Docker Desktop"):
		return "127.0.0.1", "hint-proxy:host-gateway", nil
	case runtime.GOOS == "linux" && info.Info.OSType == "linux" && strings.HasPrefix(d.cli.DaemonHost(), "unix://") && bGateway != "":
		return bGateway, "hint-proxy:" + bGateway, nil
	}
	return "", "", fmt.Errorf("%w: no verified host interface for %q", errGate, osName)
}

// openNetworks creates A and B for one run.
func (d *Docker) openNetworks(ctx context.Context) (*gateNet, error) {
	name := randName()
	off := false
	labels := map[string]string{"hint": "q4", "hint.policy": GatePolicy}
	g := &gateNet{A: name + "-a", B: name + "-b"}
	if _, err := d.cli.NetworkCreate(ctx, g.A, client.NetworkCreateOptions{Driver: "bridge", Internal: true, EnableIPv6: &off,
		Options: map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "isolated"}, Labels: labels}); err != nil {
		return nil, fmt.Errorf("%w: %v", errGate, err)
	}
	if _, err := d.cli.NetworkCreate(ctx, g.B, client.NetworkCreateOptions{Driver: "bridge", EnableIPv6: &off, Labels: labels}); err != nil {
		d.cli.NetworkRemove(context.Background(), g.A, client.NetworkRemoveOptions{})
		return nil, fmt.Errorf("%w: %v", errGate, err)
	}
	a, err := d.cli.NetworkInspect(ctx, g.A, client.NetworkInspectOptions{})
	if err != nil || len(a.Network.IPAM.Config) != 1 {
		g.close(d)
		return nil, errGate
	}
	g.ASubnet = a.Network.IPAM.Config[0].Subnet.String()
	b, err := d.cli.NetworkInspect(ctx, g.B, client.NetworkInspectOptions{})
	if err == nil && len(b.Network.IPAM.Config) == 1 && b.Network.IPAM.Config[0].Gateway.IsValid() {
		g.BGateway = b.Network.IPAM.Config[0].Gateway.String()
	}
	if err := d.checkA(ctx, g); err != nil {
		g.close(d)
		return nil, err
	}
	return g, nil
}

// checkA inspects network A against the policy.
func (d *Docker) checkA(ctx context.Context, g *gateNet) error {
	a, err := d.cli.NetworkInspect(ctx, g.A, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("%w: network A not inspectable", errGate)
	}
	n := a.Network
	if !n.Internal || n.EnableIPv6 || n.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" ||
		len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Gateway.IsValid() {
		return fmt.Errorf("%w: network A is not internal, isolated and IPv4-only", errGate)
	}
	return nil
}

// close removes the gate container and both networks.
func (g *gateNet) close(d *Docker) {
	ctx := context.Background()
	if g.Gate != "" {
		d.remove(g.Gate)
	}
	d.cli.NetworkRemove(ctx, g.A, client.NetworkRemoveOptions{})
	d.cli.NetworkRemove(ctx, g.B, client.NetworkRemoveOptions{})
}

func gateHost(extra ...string) *container.HostConfig {
	pids := int64(64)
	return &container.HostConfig{CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true,
		ExtraHosts: extra, Resources: container.Resources{Memory: 64 << 20, PidsLimit: &pids}}
}

// startGate starts the gate on B and A, forwarding to hint-proxy:port, and waits for its
// listen report.
func (d *Docker) startGate(ctx context.Context, g *gateNet, image, hostEntry string, port int, life time.Duration) error {
	h := gateHost(hostEntry)
	h.NetworkMode = container.NetworkMode(g.B)
	h.Sysctls = map[string]string{"net.ipv4.ip_forward": "0"}
	id, err := d.create(ctx, &container.Config{Image: image, Cmd: []string{"-listen-net", g.ASubnet, "-target", fmt.Sprintf("hint-proxy:%d", port),
		"-max-seconds", fmt.Sprint(int(life.Seconds()))}}, h)
	if err != nil {
		return err
	}
	g.Gate, g.Image = id, image
	if _, err := d.cli.NetworkConnect(ctx, g.A, client.NetworkConnectOptions{Container: id}); err != nil {
		return fmt.Errorf("%w: %v", errGate, err)
	}
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := d.logs(ctx, id)
		if i := strings.Index(out, "HINT_GATE listen="); i >= 0 {
			f := strings.Fields(out[i:])
			if len(f) >= 3 {
				g.Listen, g.Target = strings.TrimPrefix(f[1], "listen="), strings.TrimPrefix(f[2], "target=")
				host, _, _ := net.SplitHostPort(g.Listen)
				g.GateIP = host
				return nil
			}
		}
		if strings.Contains(out, "HINT_GATE error") {
			return fmt.Errorf("%w: %s", errGate, strings.TrimSpace(out))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%w: the gate did not start", errGate)
}

// logs returns at most 64 KiB of a container's output (diagnostic only).
func (d *Docker) logs(ctx context.Context, id string) string {
	r, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer r.Close()
	b := &capWriter{max: 64 << 10}
	_ = demux(b, b, r)
	return b.buf.String()
}

// agentHost is the host configuration of a container that runs as the agent would.
func (g *gateNet) agentHost() *container.HostConfig {
	pids := int64(512)
	dns, _ := netip.ParseAddr(g.GateIP)
	return &container.HostConfig{NetworkMode: container.NetworkMode(g.A), DNS: []netip.Addr{dns}, ExtraHosts: []string{"gate:" + g.GateIP},
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{PidsLimit: &pids}}
}

// verifyAgent inspects a container started with agentHost and the gate against the policy;
// dns is the resolver it must have (the gate address, or the probe's sentinel).
func (d *Docker) verifyAgent(ctx context.Context, g *gateNet, id, dns string) error {
	if err := d.checkA(ctx, g); err != nil {
		return err
	}
	bad := func(why string) error { return fmt.Errorf("%w: %s", errGate, why) }
	in, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || in.Container.HostConfig == nil || in.Container.Config == nil || in.Container.NetworkSettings == nil {
		return bad("agent container not inspectable")
	}
	c := in.Container
	nets := mapKeys(c.NetworkSettings.Networks)
	if !slices.Equal(nets, []string{g.A}) || string(c.HostConfig.NetworkMode) != g.A {
		return bad("the agent is on other networks")
	}
	if !slices.Contains(c.HostConfig.CapDrop, "ALL") || len(c.HostConfig.CapAdd) > 0 || c.HostConfig.Privileged ||
		!slices.Contains(c.HostConfig.SecurityOpt, "no-new-privileges:true") || c.Config.User != "65534:65534" {
		return bad("the agent has privileges")
	}
	if len(c.HostConfig.DNS) != 1 || c.HostConfig.DNS[0].String() != dns || len(c.Mounts) > 0 || len(c.HostConfig.Binds) > 0 {
		return bad("the agent's DNS or mounts")
	}
	gi, err := d.cli.ContainerInspect(ctx, g.Gate, client.ContainerInspectOptions{})
	if err != nil || gi.Container.HostConfig == nil || gi.Container.NetworkSettings == nil {
		return bad("gate not inspectable")
	}
	gc := gi.Container
	if gc.Image != g.Image || !slices.Equal(mapKeys(gc.NetworkSettings.Networks), sortedStrings(g.A, g.B)) ||
		gc.HostConfig.Sysctls["net.ipv4.ip_forward"] != "0" || !slices.Contains(gc.HostConfig.CapDrop, "ALL") || !gc.HostConfig.ReadonlyRootfs {
		return bad("the gate differs from the policy")
	}
	return nil
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sortedStrings(s ...string) []string { slices.Sort(s); return s }

// GateProbe is the result of one network probe of an engine.
type GateProbe struct {
	Key     string            `json:"key"`    // engine, policy and gate image identity
	Policy  string            `json:"policy"` // GatePolicy and its text
	OK      bool              `json:"ok"`
	Reason  string            `json:"reason,omitempty"`
	Checks  map[string]string `json:"checks"`
	Image   string            `json:"gate_image"`
	Network map[string]string `json:"network"`
}

// probeKey is the cache identity of a probe: engine ID, version, platform, policy and gate
// image.
func (d *Docker) probeKey(ctx context.Context, image string) (string, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	in := info.Info
	h := sha256.Sum256([]byte(strings.Join([]string{in.ID, in.ServerVersion, in.OSType, in.Architecture, in.OperatingSystem, in.KernelVersion,
		GatePolicy, gatePolicyText, image}, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// stub is a local HTTP listener that answers every request with a marker.
type stub struct {
	ln     net.Listener
	srv    *http.Server
	marker string
}

func startStub(host string) (*stub, error) {
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	s := &stub{ln: ln, marker: "HINT-STUB-" + hex.EncodeToString(b)}
	s.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, s.marker+"\n")
	})}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *stub) port() int      { return s.ln.Addr().(*net.TCPAddr).Port }
func (s *stub) close()         { s.srv.Close() }
func (s *stub) String() string { return s.ln.Addr().String() }

// ProbeGate runs the network probe of this engine: the gate and the agent settings are
// set up as for a trial, with two local stubs (the proxy target and another port) and a
// sentinel resolver; positive controls from B must reach every target; from A only the
// gate may be reached, no stub, gateway, sentinel or other port, and no DNS query may reach
// the sentinel. Any failure is no enforcement.
func (d *Docker) ProbeGate(ctx context.Context) GateProbe {
	p := GateProbe{Policy: GatePolicy + ": " + gatePolicyText, Checks: map[string]string{}, Network: map[string]string{}}
	no := func(why string) GateProbe { p.Reason = why; return p }
	image, err := d.gateImage(ctx)
	if err != nil {
		return no("gate image: " + err.Error())
	}
	p.Image = image
	if p.Key, err = d.probeKey(ctx, image); err != nil {
		return no("engine identity unknown")
	}
	g, err := d.openNetworks(ctx)
	if err != nil {
		return no(err.Error())
	}
	defer g.close(d)
	listen, hostEntry, err := d.proxyHost(ctx, g.BGateway)
	if err != nil {
		return no(err.Error())
	}
	target, err := startStub(listen)
	if err != nil {
		return no("stub: " + err.Error())
	}
	defer target.close()
	other, err := startStub(listen)
	if err != nil {
		return no("stub: " + err.Error())
	}
	defer other.close()
	g.ProxyIP = listen
	if err := d.startGate(ctx, g, image, hostEntry, target.port(), 5*time.Minute); err != nil {
		return no(err.Error())
	}
	p.Network = map[string]string{"a_subnet": g.ASubnet, "gate_listen": g.Listen, "gate_target": g.Target, "proxy_host": listen}
	// the sentinel: a controlled resolver and TCP listener on B
	sh := gateHost()
	sh.NetworkMode = container.NetworkMode(g.B)
	sh.Sysctls = map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"}
	sid, err := d.create(ctx, &container.Config{Image: image, Cmd: []string{"-sentinel"}}, sh)
	if err != nil {
		return no("sentinel: " + err.Error())
	}
	defer d.remove(sid)
	if _, err := d.cli.ContainerStart(ctx, sid, client.ContainerStartOptions{}); err != nil {
		return no("sentinel start")
	}
	si, err := d.cli.ContainerInspect(ctx, sid, client.ContainerInspectOptions{})
	if err != nil || si.Container.NetworkSettings == nil || si.Container.NetworkSettings.Networks[g.B] == nil {
		return no("sentinel address")
	}
	sentIP := si.Container.NetworkSettings.Networks[g.B].IPAddress.String()
	gateB := ""
	if gi, err := d.cli.ContainerInspect(ctx, g.Gate, client.ContainerInspectOptions{}); err == nil && gi.Container.NetworkSettings != nil && gi.Container.NetworkSettings.Networks[g.B] != nil {
		gateB = gi.Container.NetworkSettings.Networks[g.B].IPAddress.String()
	}
	for i := 0; i < 50 && !strings.Contains(d.logs(ctx, sid), "HINT_SENTINEL ready"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	dnsCount := func() int { return strings.Count(d.logs(ctx, sid), "HINT_SENTINEL dns") }
	run := func(name, dns string, h *container.HostConfig, args ...string) map[string]string {
		res := map[string]string{}
		id, err := d.create(ctx, &container.Config{Image: image, User: "65534:65534", Cmd: append([]string{"-probe"}, args...)}, h)
		if err != nil {
			res[name] = "fail"
			return res
		}
		defer d.remove(id)
		if name == "agent" || name == "agent_dns" {
			if err := d.verifyAgent(ctx, g, id, dns); err != nil {
				res["topology"] = "fail"
				return res
			}
			res["topology"] = "ok"
		}
		if err := d.runToEnd(ctx, id, time.Minute, "probe"); err != nil {
			res[name] = "fail"
			return res
		}
		for _, ln := range strings.Split(d.logs(ctx, id), "\n") {
			if f := strings.Fields(ln); len(f) == 3 && f[0] == "PROBE" {
				res[f[1]] = f[2]
			}
		}
		return res
	}
	// positive controls from B (the stubs, the sentinel, a DNS query reaching it)
	ctl := gateHost(hostEntry)
	ctl.NetworkMode = container.NetworkMode(g.B)
	sd, _ := netip.ParseAddr(sentIP)
	ctl.DNS = []netip.Addr{sd}
	before := dnsCount()
	controls := run("control", "", ctl,
		"reach=target=hint-proxy:"+fmt.Sprint(target.port())+"="+target.marker,
		"reach=other=hint-proxy:"+fmt.Sprint(other.port())+"="+other.marker,
		"reach=sentinel="+sentIP+":8081=HINT-SENTINEL-OK",
		"dns=sentinel_query=hint-probe-control.invalid")
	time.Sleep(time.Second)
	controlDNS := dnsCount() - before
	// the agent's own settings: only the gate
	ah := g.agentHost()
	ah.ExtraHosts = append(ah.ExtraHosts, hostEntry)
	deny := []string{
		"reach=gate=gate:8080=" + target.marker,
		"deny=gate_other_port=gate:8081",
		"deny=gate_b_address=" + net.JoinHostPort(gateB, "8080"),
		"deny=proxy_direct=hint-proxy:" + fmt.Sprint(target.port()),
		"deny=host_other_port=hint-proxy:" + fmt.Sprint(other.port()),
		"deny=b_gateway=" + net.JoinHostPort(g.BGateway, fmt.Sprint(target.port())),
		"deny=sentinel_tcp=" + net.JoinHostPort(sentIP, "8081"),
		"dns=external_name=hint-probe-agent.invalid",
	}
	agent := run("agent", g.GateIP, ah, deny...)
	// the same with the sentinel as the configured resolver: no query may reach it
	ad := g.agentHost()
	ad.DNS = []netip.Addr{sd}
	before = dnsCount()
	agentDNS := run("agent_dns", sentIP, ad, "dns=via_sentinel=hint-probe-leak.invalid")
	time.Sleep(time.Second)
	leaked := dnsCount() - before
	for k, v := range controls {
		p.Checks["control."+k] = v
	}
	for k, v := range agent {
		p.Checks["agent."+k] = v
	}
	for k, v := range agentDNS {
		p.Checks["agent_dns."+k] = v
	}
	p.Checks["control.dns_reached_sentinel"] = okIf(controlDNS > 0)
	p.Checks["agent_dns.zero_upstream_queries"] = okIf(leaked == 0)
	expected := []string{"control.reach:target", "control.reach:other", "control.reach:sentinel", "control.dns_reached_sentinel",
		"agent.topology", "agent.reach:gate", "agent.deny:gate_other_port", "agent.deny:gate_b_address", "agent.deny:proxy_direct",
		"agent.deny:host_other_port", "agent.deny:b_gateway", "agent.deny:sentinel_tcp", "agent.dns:external_name", "agent.uid",
		"agent.cap_eff", "agent.cap_bnd", "agent.no_new_privs", "agent.no_docker_socket", "agent.no_global_ipv6",
		"agent_dns.topology", "agent_dns.zero_upstream_queries"}
	for _, k := range expected {
		if p.Checks[k] != "ok" {
			return no("check " + k + " did not pass")
		}
	}
	p.OK = true
	return p
}

// probe returns the network probe of this engine, policy and gate image, running it once
// per runner session; a failed probe is not kept (the next call probes again).
func (d *Docker) probe(ctx context.Context, image string) (GateProbe, error) {
	key, err := d.probeKey(ctx, image)
	if err != nil {
		return GateProbe{}, err
	}
	d.mu.Lock()
	p, ok := d.probes[key]
	d.mu.Unlock()
	if ok {
		return p, nil
	}
	p = d.ProbeGate(ctx)
	if !p.OK || p.Key != key || p.Image != image {
		return p, fmt.Errorf("%w: %s", errGate, p.Reason)
	}
	d.mu.Lock()
	if d.probes == nil {
		d.probes = map[string]GateProbe{}
	}
	d.probes[key] = p
	d.mu.Unlock()
	return p, nil
}

// Bypass is the ledger's egress statement for one gated agent container: "enforced" only
// when a passed probe exists for this engine, policy and gate image and the container and
// the gate inspect as the policy says; otherwise "unverified".
func (d *Docker) Bypass(ctx context.Context, g *gateNet, agentID string) string {
	if g == nil || g.Image == "" {
		return "unverified"
	}
	if _, err := d.probe(ctx, g.Image); err != nil {
		return "unverified"
	}
	if d.verifyAgent(ctx, g, agentID, g.GateIP) != nil {
		return "unverified"
	}
	return "enforced"
}

func okIf(b bool) string {
	if b {
		return "ok"
	}
	return "fail"
}
