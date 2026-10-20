package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// 07B-H Q9: every container configuration the runner uses passes the host boundary; each
// kind of host access is refused before a container exists.
func TestNoHostAccess(t *testing.T) {
	d := &Docker{}
	lim := taskLimits{cpus: 1, memoryMB: 128}
	g := &gateNet{A: "hint-q4-a-x", B: "hint-q4-b-x", GateIP: "10.0.0.2"}
	probe := &container.HostConfig{NetworkMode: "none", CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}}
	for name, h := range map[string]*container.HostConfig{"offline": d.hostConfig(lim), "gate": gateHost("hint-proxy:10.0.0.1"), "agent": g.agentHost(), "probe": probe} {
		if h.NetworkMode == "" {
			h.NetworkMode = "hint-q4-b-x" // the gate joins its network after the start
		}
		if err := noHostAccess(h); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, f := range map[string]func(*container.HostConfig){
		"privileged":         func(h *container.HostConfig) { h.Privileged = true },
		"added capability":   func(h *container.HostConfig) { h.CapAdd = []string{"SYS_ADMIN"} },
		"bind":               func(h *container.HostConfig) { h.Binds = []string{"/:/host"} },
		"docker socket":      func(h *container.HostConfig) { h.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"} },
		"device":             func(h *container.HostConfig) { h.Devices = []container.DeviceMapping{{PathOnHost: "/dev/kvm"}} },
		"tmpfs":              func(h *container.HostConfig) { h.Tmpfs = map[string]string{"/x": ""} },
		"volumes from":       func(h *container.HostConfig) { h.VolumesFrom = []string{"other"} },
		"published port":     func(h *container.HostConfig) { h.PortBindings = network.PortMap{}; h.PublishAllPorts = true },
		"host network":       func(h *container.HostConfig) { h.NetworkMode = "host" },
		"default bridge":     func(h *container.HostConfig) { h.NetworkMode = "bridge" },
		"no network named":   func(h *container.HostConfig) { h.NetworkMode = "" },
		"container network":  func(h *container.HostConfig) { h.NetworkMode = "container:other" },
		"host pid":           func(h *container.HostConfig) { h.PidMode = "host" },
		"host ipc":           func(h *container.HostConfig) { h.IpcMode = "host" },
		"container ipc":      func(h *container.HostConfig) { h.IpcMode = "container:other" },
		"host uts":           func(h *container.HostConfig) { h.UTSMode = "host" },
		"host userns":        func(h *container.HostConfig) { h.UsernsMode = "host" },
		"no-new-privs off":   func(h *container.HostConfig) { h.SecurityOpt = nil },
		"seccomp unconfined": func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, "seccomp=unconfined") },
		"apparmor unconfined": func(h *container.HostConfig) {
			h.SecurityOpt = append(h.SecurityOpt, "apparmor=unconfined")
		},
		"selinux disabled": func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, "label=disable") },
	} {
		h := d.hostConfig(lim)
		f(h)
		if noHostAccess(h) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if noHostAccess(nil) == nil {
		t.Error("a missing host configuration accepted")
	}
	// create refuses before the engine is contacted (no client here)
	bad := d.hostConfig(lim)
	bad.Binds = []string{"/:/host"}
	if _, err := d.create(context.Background(), &container.Config{Image: "sha256:" + strings.Repeat("a", 64)}, bad); err == nil {
		t.Error("create accepted host access")
	}
	cg := d.hostConfig(lim)
	cg.CgroupnsMode = "host"
	if _, err := d.create(context.Background(), &container.Config{Image: "sha256:" + strings.Repeat("a", 64)}, cg); err == nil {
		t.Error("create accepted the host cgroup namespace")
	}
}
