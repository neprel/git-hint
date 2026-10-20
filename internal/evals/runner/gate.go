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
	Gate     string   // gate container ID
	GateIP   string   // the gate's address in A
	Image    string   // gate image ID
	Listen   string   // as the gate reported it
	Target   string   // the resolved target as the gate reported it
	ProxyIP  string   // the host address the proxy listens on
	Cmd      []string // the gate's frozen command
	Host     string   // the gate's one hosts entry (hint-proxy)
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
func (g *gateNet) close(d *Docker) { _ = g.closeErr(d) }

// closeErr removes the gate container and both networks (bounded) and reports what failed.
func (g *gateNet) closeErr(d *Docker) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	if g.Gate != "" {
		if _, err := d.cli.ContainerRemove(ctx, g.Gate, client.ContainerRemoveOptions{Force: true}); err != nil {
			errs = append(errs, fmt.Errorf("gate container: %v", err))
		}
	}
	for _, n := range []string{g.A, g.B} {
		if _, err := d.cli.NetworkRemove(ctx, n, client.NetworkRemoveOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("network %s: %v", n, err))
		}
	}
	return errors.Join(errs...)
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
	g.Cmd = []string{"-listen-net", g.ASubnet, "-target", fmt.Sprintf("hint-proxy:%d", port), "-max-seconds", fmt.Sprint(int(life.Seconds()))}
	g.Host = hostEntry
	id, err := d.create(ctx, &container.Config{Image: image, User: "65534:65534", Cmd: g.Cmd}, h)
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
		out, _ := d.capture(ctx, id) // retried until the deadline
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

// capture returns a container's complete output (at most 64 KiB) or an error: a failed
// retrieval, a cut frame or truncation is never evidence (C7BJ-15).
func (d *Docker) capture(ctx context.Context, id string) (string, error) {
	r, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", fmt.Errorf("output unavailable: %w", err)
	}
	defer r.Close()
	b := &capWriter{max: 64 << 10}
	if err := demux(b, b, r); err != nil {
		return "", fmt.Errorf("output incomplete: %w", err)
	}
	if b.over {
		return "", errors.New("output truncated")
	}
	return b.buf.String(), nil
}

// agentHost is the host configuration of a container that runs as the agent would.
func (g *gateNet) agentHost() *container.HostConfig {
	pids := int64(512)
	dns, _ := netip.ParseAddr(g.GateIP)
	return &container.HostConfig{NetworkMode: container.NetworkMode(g.A), DNS: []netip.Addr{dns}, ExtraHosts: []string{"gate:" + g.GateIP},
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{PidsLimit: &pids}}
}

// verifyAgent inspects a container started with agentHost and the gate against the policy;
// dns is the resolver and hosts the hosts entries the container must have (the gate address
// and the gate entry, or the probe's variants).
func (d *Docker) verifyAgent(ctx context.Context, g *gateNet, id, dns string, hosts []string) error {
	if err := d.checkA(ctx, g); err != nil {
		return err
	}
	bad := func(why string) error { return fmt.Errorf("%w: %s", errGate, why) }
	in, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || in.Container.HostConfig == nil || in.Container.Config == nil || in.Container.NetworkSettings == nil {
		return bad("agent container not inspectable")
	}
	c := in.Container
	if !slices.Equal(mapKeys(c.NetworkSettings.Networks), []string{g.A}) || string(c.HostConfig.NetworkMode) != g.A {
		return bad("the agent is on other networks")
	}
	if c.Config.User != "65534:65534" || unprivileged(c.HostConfig) != nil {
		return bad("the agent has privileges")
	}
	if len(c.HostConfig.DNS) != 1 || c.HostConfig.DNS[0].String() != dns || !slices.Equal(sortedStrings(c.HostConfig.ExtraHosts...), sortedStrings(hosts...)) {
		return bad("the agent's DNS or hosts entries")
	}
	if len(c.Mounts) > 0 {
		return bad("the agent has mounts")
	}
	return d.verifyGate(ctx, g)
}

// unprivileged checks the restrictions every policy container shares: no capabilities, no
// privileges, no-new-privileges, no binds, no published ports, no host namespaces.
func unprivileged(h *container.HostConfig) error {
	if !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) > 0 || h.Privileged || !slices.Contains(h.SecurityOpt, "no-new-privileges:true") ||
		len(h.Binds) > 0 || len(h.Mounts) > 0 || len(h.PortBindings) > 0 || h.PublishAllPorts || len(h.Devices) > 0 ||
		h.PidMode != "" || h.IpcMode.IsHost() || h.UTSMode.IsHost() || h.UsernsMode.IsHost() {
		return errors.New("privileged")
	}
	return nil
}

// verifyGate inspects the gate against its frozen configuration: the image, the command,
// the user, the one hosts entry, the networks, IP forwarding off, a read-only root and no
// privileges, mounts or published ports (configuration invariants; a Docker administrator
// is trusted).
func (d *Docker) verifyGate(ctx context.Context, g *gateNet) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", errGate, why) }
	gi, err := d.cli.ContainerInspect(ctx, g.Gate, client.ContainerInspectOptions{})
	if err != nil || gi.Container.HostConfig == nil || gi.Container.Config == nil || gi.Container.NetworkSettings == nil {
		return bad("gate not inspectable")
	}
	gc := gi.Container
	h := gc.HostConfig
	switch {
	case gc.Image != g.Image:
		return bad("gate image")
	case !slices.Equal(gc.Config.Entrypoint, []string{"/gate"}) || !slices.Equal(gc.Config.Cmd, g.Cmd) || gc.Config.User != "65534:65534":
		return bad("gate command or user")
	case !slices.Equal(mapKeys(gc.NetworkSettings.Networks), sortedStrings(g.A, g.B)) || string(h.NetworkMode) != g.B:
		return bad("gate networks")
	case !slices.Equal(h.ExtraHosts, []string{g.Host}):
		return bad("gate hosts entries")
	case len(h.Sysctls) != 1 || h.Sysctls["net.ipv4.ip_forward"] != "0" || !h.ReadonlyRootfs:
		return bad("gate forwarding or root")
	case unprivileged(h) != nil || len(gc.Mounts) > 0:
		return bad("gate privileges, mounts or ports")
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
	ready := false
	for i := 0; i < 50 && !ready; i++ {
		out, err := d.capture(ctx, sid)
		ready = err == nil && strings.Contains(out, "HINT_SENTINEL ready")
		if !ready {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ready {
		return no("the sentinel did not start")
	}
	// the sentinel's DNS counter, only from a complete capture (C7BJ-15)
	dnsCount := func() (int, error) {
		out, err := d.capture(ctx, sid)
		if err != nil {
			return 0, err
		}
		return strings.Count(out, "HINT_SENTINEL dns"), nil
	}
	// run executes one probe container and returns its check lines; it fails unless the
	// container ran to a clean exit, its output was captured completely, and it reported
	// every requested check and every identity check exactly once (C7BJ-13)
	identity := []string{"uid", "cap_eff", "cap_bnd", "no_new_privs", "no_docker_socket", "no_global_ipv6"}
	run := func(name, dns string, hosts []string, h *container.HostConfig, args ...string) (map[string]string, error) {
		res := map[string]string{}
		id, err := d.create(ctx, &container.Config{Image: image, User: "65534:65534", Cmd: append([]string{"-probe"}, args...)}, h)
		if err != nil {
			return nil, fmt.Errorf("%s: not created", name)
		}
		defer d.remove(id)
		if dns != "" {
			if err := d.verifyAgent(ctx, g, id, dns, hosts); err != nil {
				return nil, fmt.Errorf("%s: topology: %v", name, err)
			}
			res["topology"] = "ok"
		}
		if err := d.runToEnd(ctx, id, time.Minute, "probe"); err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		out, err := d.capture(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		for _, ln := range strings.Split(out, "\n") {
			if f := strings.Fields(ln); len(f) == 3 && f[0] == "PROBE" {
				if _, dup := res[f[1]]; dup {
					return nil, fmt.Errorf("%s: %s reported twice", name, f[1])
				}
				res[f[1]] = f[2]
			}
		}
		want := append([]string{}, identity...)
		for _, a := range args {
			kind, rest, _ := strings.Cut(a, "=")
			n, _, _ := strings.Cut(rest, "=")
			want = append(want, kind+":"+n)
		}
		for _, k := range want {
			if res[k] != "ok" {
				return nil, fmt.Errorf("%s: check %s is %q", name, k, res[k])
			}
		}
		return res, nil
	}
	merge := func(prefix string, m map[string]string) {
		for k, v := range m {
			p.Checks[prefix+"."+k] = v
		}
	}
	// positive controls from B (the stubs, the sentinel, a DNS query reaching it)
	ctl := gateHost(hostEntry)
	ctl.NetworkMode = container.NetworkMode(g.B)
	sd, _ := netip.ParseAddr(sentIP)
	ctl.DNS = []netip.Addr{sd}
	before, err := dnsCount()
	if err != nil {
		return no("sentinel counter: " + err.Error())
	}
	controls, err := run("control", "", nil, ctl,
		"reach=target=hint-proxy:"+fmt.Sprint(target.port())+"="+target.marker,
		"reach=other=hint-proxy:"+fmt.Sprint(other.port())+"="+other.marker,
		"reach=sentinel="+sentIP+":8081=HINT-SENTINEL-OK",
		"dns=sentinel_query=hint-probe-control.invalid")
	if err != nil {
		return no(err.Error())
	}
	merge("control", controls)
	time.Sleep(time.Second)
	after, err := dnsCount()
	if err != nil || after-before < 1 {
		return no("the control DNS query did not reach the sentinel, or its counter is unknown")
	}
	p.Checks["control.dns_reached_sentinel"] = "ok"
	// the agent's own settings: only the gate
	ah := g.agentHost()
	ah.ExtraHosts = append(ah.ExtraHosts, hostEntry)
	agent, err := run("agent", g.GateIP, []string{"gate:" + g.GateIP, hostEntry}, ah,
		"reach=gate=gate:8080="+target.marker,
		"deny=gate_other_port=gate:8081",
		"deny=gate_b_address="+net.JoinHostPort(gateB, "8080"),
		"deny=proxy_direct=hint-proxy:"+fmt.Sprint(target.port()),
		"deny=host_other_port=hint-proxy:"+fmt.Sprint(other.port()),
		"deny=b_gateway="+net.JoinHostPort(g.BGateway, fmt.Sprint(target.port())),
		"deny=sentinel_tcp="+net.JoinHostPort(sentIP, "8081"),
		"dns=external_name=hint-probe-agent.invalid")
	if err != nil {
		return no(err.Error())
	}
	merge("agent", agent)
	// the same with the sentinel as the configured resolver: no query may reach it
	ad := g.agentHost()
	ad.DNS = []netip.Addr{sd}
	before, err = dnsCount()
	if err != nil {
		return no("sentinel counter: " + err.Error())
	}
	agentDNS, err := run("agent_dns", sentIP, []string{"gate:" + g.GateIP}, ad, "dns=via_sentinel=hint-probe-leak.invalid")
	if err != nil {
		return no(err.Error())
	}
	merge("agent_dns", agentDNS)
	time.Sleep(time.Second)
	after, err = dnsCount()
	if err != nil {
		return no("sentinel counter: " + err.Error())
	}
	if after != before {
		return no("a DNS query from the agent network reached the sentinel")
	}
	p.Checks["agent_dns.zero_upstream_queries"] = "ok"
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
	if d.probeFn != nil {
		p = d.probeFn(ctx)
	} else {
		p = d.ProbeGate(ctx)
	}
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
	if d.verifyAgent(ctx, g, agentID, g.GateIP, []string{"gate:" + g.GateIP}) != nil {
		return "unverified"
	}
	return "enforced"
}
