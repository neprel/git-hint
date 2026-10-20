package runner

// Q4 regressions on a synthetic engine API (from the C7BJ-13..15 review; no engine, no model).
import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeGateID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fakeQ4Container struct {
	cfg, host map[string]any
	nets      map[string]any
	mode      string
}
type fakeQ4Engine struct {
	mu               sync.Mutex
	containers       map[string]*fakeQ4Container
	networks         map[string]map[string]any
	next, dns        int
	failDNS          bool
	loseSentinelLogs bool
	sentinelBreak    string
	sentinelReads    int
	t                *testing.T
}

func fakeQ4Docker(t *testing.T, fail bool) (*Docker, *fakeQ4Engine) {
	t.Helper()
	s := &fakeQ4Engine{containers: map[string]*fakeQ4Container{}, networks: map[string]map[string]any{}, failDNS: fail, t: t}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	c, e := client.New(client.WithHost(srv.URL), client.WithAPIVersion("1.56"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return &Docker{cli: c}, s
}
func (s *fakeQ4Engine) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	emit := func(x any) { json.NewEncoder(w).Encode(x) }
	switch {
	case strings.HasSuffix(p, "/info"):
		emit(map[string]any{"ID": "synthetic-q4", "ServerVersion": "29.4.0", "OSType": "linux", "Architecture": "aarch64", "OperatingSystem": "OrbStack synthetic", "KernelVersion": "synthetic"})
		return
	case strings.HasSuffix(p, "/build"):
		emit(map[string]any{"aux": map[string]any{"ID": fakeGateID}})
		return
	case strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json"):
		emit(map[string]any{"Id": fakeGateID})
		return
	case strings.HasSuffix(p, "/networks/create"):
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		name := m["Name"].(string)
		internal, _ := m["Internal"].(bool)
		ipam := map[string]any{"Subnet": "10.77.0.0/24", "Gateway": "10.77.0.1"}
		if internal {
			ipam = map[string]any{"Subnet": "10.66.0.0/24"}
		}
		s.networks[name] = map[string]any{"Name": name, "Internal": internal, "EnableIPv6": false, "Options": m["Options"], "IPAM": map[string]any{"Config": []any{ipam}}}
		w.WriteHeader(201)
		emit(map[string]any{"Id": name})
		return
	case strings.Contains(p, "/networks/"):
		name := p[strings.Index(p, "/networks/")+len("/networks/"):]
		if strings.HasSuffix(name, "/connect") {
			name = strings.TrimSuffix(name, "/connect")
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			s.containers[m["Container"].(string)].nets[name] = map[string]any{"IPAddress": "10.66.0.2"}
			w.WriteHeader(200)
			return
		}
		if r.Method == "DELETE" {
			delete(s.networks, name)
			w.WriteHeader(204)
			return
		}
		if n, ok := s.networks[name]; ok {
			emit(n)
			return
		}
	case strings.HasSuffix(p, "/containers/create"):
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		h := m["HostConfig"].(map[string]any)
		cmd, _ := m["Cmd"].([]any)
		mode := "gate"
		for _, a := range cmd {
			v, _ := a.(string)
			if v == "-sentinel" {
				mode = "sentinel"
			}
			if strings.Contains(v, "hint-probe-control.invalid") {
				mode = "control"
			}
			if strings.Contains(v, "hint-probe-agent.invalid") {
				mode = "agent"
			}
			if strings.Contains(v, "hint-probe-leak.invalid") {
				mode = "agent_dns"
			}
		}
		if m["User"] == nil || m["User"] == "" {
			m["User"] = "65534:65534"
		}
		if m["Entrypoint"] == nil { // the image's entrypoint, as the engine reports it
			m["Entrypoint"] = []any{"/gate"}
		}
		s.next++
		id := fmt.Sprint("c", s.next)
		n := h["NetworkMode"].(string)
		ip := "10.66.0.3"
		if strings.HasSuffix(n, "-b") {
			ip = "10.77.0.3"
		}
		s.containers[id] = &fakeQ4Container{m, h, map[string]any{n: map[string]any{"IPAddress": ip}}, mode}
		w.WriteHeader(201)
		emit(map[string]any{"Id": id})
		return
	case strings.Contains(p, "/containers/"):
		tail := p[strings.Index(p, "/containers/")+len("/containers/"):]
		parts := strings.Split(tail, "/")
		id := parts[0]
		c := s.containers[id]
		if r.Method == "DELETE" {
			delete(s.containers, id)
			w.WriteHeader(204)
			return
		}
		if c == nil {
			s.t.Errorf("missing container %s", id)
			break
		}
		op := parts[len(parts)-1]
		switch op {
		case "json":
			emit(map[string]any{"Id": id, "Image": fakeGateID, "Config": c.cfg, "HostConfig": c.host, "Mounts": []any{}, "NetworkSettings": map[string]any{"Networks": c.nets}})
			return
		case "start", "stop":
			w.WriteHeader(204)
			return
		case "wait":
			st := 0
			if c.mode == "control" {
				s.dns++
			}
			if c.mode == "agent_dns" && s.failDNS {
				st = 17
			}
			emit(map[string]any{"StatusCode": st, "Error": nil})
			return
		case "logs":
			if c.mode == "sentinel" {
				s.sentinelReads++
				if s.loseSentinelLogs && s.sentinelReads >= 4 {
					w.WriteHeader(500)
					emit(map[string]any{"message": "capture unavailable"})
					return
				}
				if s.sentinelBreak != "" && s.sentinelReads >= 4 {
					hdr := make([]byte, 8)
					hdr[0] = 1
					if s.sentinelBreak == "cut" {
						binary.BigEndian.PutUint32(hdr[4:], 100)
						w.Write(hdr)
						w.Write([]byte("HINT_SENTINEL ready"))
						return
					}
					big := strings.Repeat("x", 70<<10)
					binary.BigEndian.PutUint32(hdr[4:], uint32(len(big)))
					w.Write(hdr)
					w.Write([]byte(big))
					return
				}
			}
			out := ""
			if c.mode == "gate" {
				port := "8080"
				cmd, _ := c.cfg["Cmd"].([]any)
				for i, a := range cmd {
					if a == "-target" && i+1 < len(cmd) {
						v := cmd[i+1].(string)
						port = v[strings.LastIndex(v, ":")+1:]
					}
				}
				out = "HINT_GATE listen=10.66.0.2:8080 target=127.0.0.1:" + port + "\n"
			} else if c.mode == "sentinel" {
				out = "HINT_SENTINEL ready\n" + strings.Repeat("HINT_SENTINEL dns\n", s.dns)
			} else {
				cmd, _ := c.cfg["Cmd"].([]any)
				for _, a := range cmd {
					v, _ := a.(string)
					k, rest, ok := strings.Cut(v, "=")
					if !ok {
						continue
					}
					name, _, _ := strings.Cut(rest, "=")
					out += "PROBE " + k + ":" + name + " ok\n"
				}
				for _, k := range []string{"uid", "cap_eff", "cap_bnd", "no_new_privs", "no_docker_socket", "no_global_ipv6"} {
					out += "PROBE " + k + " ok\n"
				}
			}
			hdr := make([]byte, 8)
			hdr[0] = 1
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(out)))
			w.Write(hdr)
			w.Write([]byte(out))
			return
		}
	}
	s.t.Errorf("unexpected API %s %s", r.Method, p)
	w.WriteHeader(404)
}
func TestGateFakeFailedDNSProbeCannotEnforce(t *testing.T) {
	d, _ := fakeQ4Docker(t, true)
	p := d.ProbeGate(context.Background())
	if p.OK {
		t.Fatalf("DNS probe exits 17 but policy marked OK: %+v", p.Checks)
	}
}
func TestGateFakeSyntheticNormalControl(t *testing.T) {
	d, _ := fakeQ4Docker(t, false)
	p := d.ProbeGate(context.Background())
	if !p.OK {
		t.Fatalf("synthetic normal path must reach completed checks: %+v", p)
	}
}

func TestGateFakeGateConfigurationMustMatch(t *testing.T) {
	d, s := fakeQ4Docker(t, false)
	ctx := context.Background()
	g, err := d.openNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close(d)
	if err = d.startGate(ctx, g, fakeGateID, "hint-proxy:host-gateway", 12345, time.Minute); err != nil {
		t.Fatal(err)
	}
	id, err := d.create(ctx, &container.Config{Image: fakeGateID, User: "65534:65534"}, g.agentHost())
	if err != nil {
		t.Fatal(err)
	}
	key, err := d.probeKey(ctx, fakeGateID)
	if err != nil {
		t.Fatal(err)
	}
	d.probes = map[string]GateProbe{key: {OK: true, Key: key, Image: fakeGateID}}
	if got := d.Bypass(ctx, g, id); got != "enforced" {
		t.Fatalf("normal control: %s", got)
	}
	s.mu.Lock()
	s.containers[g.Gate].cfg["Cmd"] = []any{"-sentinel"}
	s.containers[g.Gate].cfg["User"] = "0:0"
	s.containers[g.Gate].host["Privileged"] = true
	s.containers[g.Gate].host["CapAdd"] = []any{"NET_ADMIN"}
	s.mu.Unlock()
	if got := d.Bypass(ctx, g, id); got != "unverified" {
		t.Fatalf("wrong gate command + root + privileged + cap-add still %s", got)
	}
}

func TestGateFakeUnavailableDNSCaptureCannotEnforce(t *testing.T) {
	d, s := fakeQ4Docker(t, false)
	s.loseSentinelLogs = true
	p := d.ProbeGate(context.Background())
	if p.OK {
		t.Fatalf("sentinel capture unavailable on both DNS-negative snapshots but policy marked OK; reads=%d", s.sentinelReads)
	}
}

func TestGateFakeBrokenDNSCaptureCannotEnforce(t *testing.T) {
	for _, mode := range []string{"cut", "huge"} {
		d, s := fakeQ4Docker(t, false)
		s.sentinelBreak = mode
		if p := d.ProbeGate(context.Background()); p.OK {
			t.Errorf("%s sentinel capture: OK", mode)
		}
	}
}

// Each single deviation of the gate or the agent from the frozen policy is unverified.
func TestGateFakeMutations(t *testing.T) {
	type mut struct {
		gate bool
		edit func(c *fakeQ4Container)
	}
	for name, m := range map[string]mut{
		"gate command":       {true, func(c *fakeQ4Container) { c.cfg["Cmd"] = []any{"-sentinel"} }},
		"gate user":          {true, func(c *fakeQ4Container) { c.cfg["User"] = "0:0" }},
		"gate entrypoint":    {true, func(c *fakeQ4Container) { c.cfg["Entrypoint"] = []any{"/gate", "-sentinel"} }},
		"gate no entrypoint": {true, func(c *fakeQ4Container) { c.cfg["Entrypoint"] = nil }},
		"gate other program": {true, func(c *fakeQ4Container) { c.cfg["Entrypoint"] = []any{"/bin/sh"} }},
		"gate extra argument": {true, func(c *fakeQ4Container) {
			c.cfg["Cmd"] = append(append([]any{}, c.cfg["Cmd"].([]any)...), "-port", "9090")
		}},
		"gate other target": {true, func(c *fakeQ4Container) {
			cmd := append([]any{}, c.cfg["Cmd"].([]any)...)
			for i := range cmd {
				if cmd[i] == "-target" {
					cmd[i+1] = "hint-proxy:1"
				}
			}
			c.cfg["Cmd"] = cmd
		}},
		"gate privileged": {true, func(c *fakeQ4Container) { c.host["Privileged"] = true }},
		"gate cap add":    {true, func(c *fakeQ4Container) { c.host["CapAdd"] = []any{"NET_ADMIN"} }},
		"gate hosts":      {true, func(c *fakeQ4Container) { c.host["ExtraHosts"] = []any{"hint-proxy:10.0.0.9"} }},
		"gate sysctls":    {true, func(c *fakeQ4Container) { c.host["Sysctls"] = map[string]any{"net.ipv4.ip_forward": "1"} }},
		"gate writable":   {true, func(c *fakeQ4Container) { c.host["ReadonlyRootfs"] = false }},
		"gate ports": {true, func(c *fakeQ4Container) {
			c.host["PortBindings"] = map[string]any{"8080/tcp": []any{map[string]any{"HostPort": "8080"}}}
		}},
		"gate binds":        {true, func(c *fakeQ4Container) { c.host["Binds"] = []any{"/var/run/docker.sock:/var/run/docker.sock"} }},
		"agent hosts":       {false, func(c *fakeQ4Container) { c.host["ExtraHosts"] = []any{"gate:10.66.0.2", "hint-proxy:host-gateway"} }},
		"agent ports":       {false, func(c *fakeQ4Container) { c.host["PublishAllPorts"] = true }},
		"agent host pid":    {false, func(c *fakeQ4Container) { c.host["PidMode"] = "host" }},
		"agent cap add":     {false, func(c *fakeQ4Container) { c.host["CapAdd"] = []any{"NET_RAW"} }},
		"agent no-new-priv": {false, func(c *fakeQ4Container) { c.host["SecurityOpt"] = []any{} }},
	} {
		d, s := fakeQ4Docker(t, false)
		ctx := context.Background()
		g, err := d.openNetworks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.startGate(ctx, g, fakeGateID, "hint-proxy:host-gateway", 12345, time.Minute); err != nil {
			t.Fatal(err)
		}
		id, err := d.create(ctx, &container.Config{Image: fakeGateID, User: "65534:65534"}, g.agentHost())
		if err != nil {
			t.Fatal(err)
		}
		key, _ := d.probeKey(ctx, fakeGateID)
		d.probes = map[string]GateProbe{key: {OK: true, Key: key, Image: fakeGateID}}
		if got := d.Bypass(ctx, g, id); got != "enforced" {
			t.Fatalf("%s: control %s", name, got)
		}
		s.mu.Lock()
		target := s.containers[id]
		if m.gate {
			target = s.containers[g.Gate]
		}
		m.edit(target)
		s.mu.Unlock()
		if got := d.Bypass(ctx, g, id); got != "unverified" {
			t.Errorf("%s: %s", name, got)
		}
		g.close(d)
	}
}
