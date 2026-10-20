package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Live agent runs in the container runner (07B-J §20, Q4 accepted offline): each attempt gets
// its own proxy listener on the platform's proxy host, its own gate and agent network, and an
// agent container from the task's own agent image with the agent policy (only A, DNS = gate,
// uid 65534, no capabilities, no-new-privileges, no mounts). The run starts only when the
// network probe for this engine, policy and gate image passed and the inspected topology
// matches (Bypass = enforced); otherwise nothing is launched. After the agent stops, the
// model ledger is sealed; the final tree is frozen and checked by the separate verifier as
// for every attempt. The real key stays in the proxy; the container sees the gate URL and a
// dummy key only.

// ErrGateRefused is the exception of an attempt that was not launched because the model gate
// could not be shown to hold.
const ErrGateRefused = "ModelGateRefusedError"

// attach serves the proxy on one more listener (the per-attempt proxy host) until close.
func (lv *Live) attach(host string) (int, func(), error) {
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: lv.srv.Handler}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { srv.Close() }, nil
}

// gateEnv is the agent's model environment through the gate.
func gateEnv(c *Counter) []string {
	return []string{"OPENAI_BASE_URL=http://gate:8080/t/" + c.token, "OPENAI_API_KEY=hint-proxy", "HOME=/tmp"}
}

// livePhase runs one live attempt and returns the frozen tree's digest and the sealed ledger.
// A refused gate returns ErrGateRefused before any agent container exists.
func (d *Docker) livePhase(ctx context.Context, taskDir string, o RunOptions, lim taskLimits, img, artifact string) (string, *LedgerReport, error) {
	gateImg, err := d.gateImage(ctx)
	if err != nil {
		return "", nil, gateRefused(err)
	}
	if _, err := d.probe(ctx, gateImg); err != nil {
		return "", nil, gateRefused(err)
	}
	g, err := d.openNetworks(ctx)
	if err != nil {
		return "", nil, gateRefused(err)
	}
	defer g.close(d)
	host, entry, err := d.proxyHost(ctx, g.BGateway)
	if err != nil {
		return "", nil, gateRefused(err)
	}
	port, stop, err := o.Live.attach(host)
	if err != nil {
		return "", nil, gateRefused(err)
	}
	defer stop()
	if err := d.startGate(ctx, g, gateImg, entry, port, lim.agent+time.Minute); err != nil {
		return "", nil, gateRefused(err)
	}
	h := g.agentHost()
	pids := int64(1024)
	h.Resources = container.Resources{NanoCPUs: int64(lim.cpus * 1e9), Memory: lim.memoryMB << 20, PidsLimit: &pids}
	id, err := d.create(ctx, &container.Config{Image: img, User: "65534:65534", WorkingDir: "/app", Cmd: o.agentCmd, Env: gateEnv(o.Counter)}, h)
	if err != nil {
		return "", nil, gateRefused(err)
	}
	defer d.remove(id)
	// the starting tree, owned by the agent's user (no root step inside the container)
	repo, err := ownedTree(filepath.Join(taskDir, "environment", "repo"))
	if err != nil {
		return "", nil, err
	}
	if _, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(repo)}); err != nil {
		return "", nil, err
	}
	bypass := d.Bypass(ctx, g, id)
	if bypass != BypassEnforced {
		return "", nil, gateRefused(errors.New("the inspected topology is not the probed policy"))
	}
	runErr := d.runToEnd(ctx, id, lim.agent, "agent")
	report := o.Counter.Seal(5*time.Second, bypass, ScopeIsolatedContainer)
	if runErr != nil {
		return "", report, runErr
	}
	src, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: "/app"})
	if err != nil {
		return "", report, err
	}
	defer src.Content.Close()
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		return "", report, err
	}
	digest, err := freezeTree(src.Content, artifact)
	return digest, report, err
}

// ownedTree is the archive of the starting tree under app/ (the directory itself first) with
// every entry owned by 65534:65534, the agent's user.
func ownedTree(repo string) ([]byte, error) {
	b, err := tarDir(repo, "app")
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if err := tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 65534, Gid: 65534, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		h.Uid, h.Gid = 65534, 65534
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type gateRefusal struct{ err error }

func (g gateRefusal) Error() string { return "the model gate refused the attempt: " + g.err.Error() }

func gateRefused(err error) error { return gateRefusal{err} }
