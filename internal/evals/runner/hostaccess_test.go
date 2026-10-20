package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
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

// C7BJ-18: the engine's own report after creation is the evidence. Each case changes one field
// of a normal report; the container is removed before any start and no ID is returned.
func TestCreateChecksTheReportedContainer(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	normal := func() map[string]any {
		return map[string]any{"Id": "c", "Image": image, "Mounts": []any{},
			"HostConfig": map[string]any{"NetworkMode": "none", "SecurityOpt": []string{"no-new-privileges:true"}}}
	}
	host := func(m map[string]any) map[string]any { return m["HostConfig"].(map[string]any) }
	cases := map[string]func(map[string]any){
		"control":             func(map[string]any) {},
		"missing host config": func(m map[string]any) { delete(m, "HostConfig") },
		"null host config":    func(m map[string]any) { m["HostConfig"] = nil },
		"image volume": func(m map[string]any) {
			m["Mounts"] = []any{map[string]any{"Type": "volume", "Name": "auto", "Source": "/var/lib/docker/volumes/auto/_data", "Destination": "/data", "RW": true}}
		},
		"reported bind mount": func(m map[string]any) {
			m["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/", "Destination": "/host", "RW": false}}
		},
		"reported binds":       func(m map[string]any) { host(m)["Binds"] = []string{"/:/host"} },
		"reported privileged":  func(m map[string]any) { host(m)["Privileged"] = true },
		"reported host net":    func(m map[string]any) { host(m)["NetworkMode"] = "host" },
		"reported devices":     func(m map[string]any) { host(m)["Devices"] = []any{map[string]any{"PathOnHost": "/dev/kvm"}} },
		"reported no nnp":      func(m map[string]any) { host(m)["SecurityOpt"] = []string{} },
		"other image reported": func(m map[string]any) { m["Image"] = "sha256:" + strings.Repeat("b", 64) },
	}
	for name, mutate := range cases {
		for _, deleteFails := range []bool{false, true} {
			var started, deleted bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(map[string]any{"Id": "c"})
				case strings.HasSuffix(r.URL.Path, "/json"):
					m := normal()
					mutate(m)
					json.NewEncoder(w).Encode(m)
				case strings.HasSuffix(r.URL.Path, "/start"):
					started = true
					w.WriteHeader(204)
				case r.Method == "DELETE":
					deleted = true
					if deleteFails {
						w.WriteHeader(500)
						json.NewEncoder(w).Encode(map[string]any{"message": "synthetic"})
						return
					}
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			cli, err := client.New(client.WithHost(api.URL), client.WithAPIVersion("1.56"))
			if err != nil {
				t.Fatal(err)
			}
			d := &Docker{cli: cli}
			id, err := d.create(context.Background(), &container.Config{Image: image}, d.hostConfig(taskLimits{cpus: 1, memoryMB: 128}))
			cli.Close()
			api.Close()
			switch {
			case name == "control":
				if err != nil || id != "c" || deleted {
					t.Errorf("control: %q %v deleted %v", id, err, deleted)
				}
			case err == nil || id != "" || !deleted || started:
				t.Errorf("%s: accepted id %q err %v deleted %v started %v", name, id, err, deleted, started)
			case deleteFails && !strings.Contains(err.Error(), "synthetic"):
				t.Errorf("%s: the failed removal is not reported: %v", name, err)
			}
		}
	}
}

// An image that declares a VOLUME gets an engine volume by default: the runner refuses the
// container and leaves none behind (engine; local image built from the pinned base).
func TestDockerRefusesAnImageVolume(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+baseImage+"\nVOLUME /data\n"), 0o644)
	ctx := context.Background()
	id, _, _, err := d.build(ctx, dir, "agent")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := d.create(ctx, &container.Config{Image: id, Cmd: []string{"true"}}, d.hostConfig(taskLimits{cpus: 1, memoryMB: 128}))
	if err == nil || cid != "" || !strings.Contains(err.Error(), "mount") {
		t.Fatalf("an image volume was accepted: %q %v", cid, err)
	}
	left, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range left.Items {
		if c.ImageID == id {
			t.Fatalf("container %s left behind", c.ID)
		}
	}
}
