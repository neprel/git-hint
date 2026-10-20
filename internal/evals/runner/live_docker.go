package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
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

// attach serves the proxy on one more listener (the per-attempt proxy host); stop closes
// it and waits (bounded) for its active handlers, so no request is still on the way in.
func (lv *Live) attach(host string) (int, func() error, error) {
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: lv.srv.Handler}
	go srv.Serve(ln)
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
			return fmt.Errorf("proxy listener: %v", err)
		}
		return nil
	}
	return ln.Addr().(*net.TCPAddr).Port, stop, nil
}

// gateEnv is the agent's model environment through the gate.
func gateEnv(c *Counter) []string {
	return []string{"OPENAI_BASE_URL=http://gate:8080/t/" + c.token, "OPENAI_API_KEY=hint-proxy", "HOME=/tmp"}
}

// liveResult is what a live attempt leaves: the frozen tree's digest, the ledger as read
// after every per-attempt path was torn down, whether that teardown completed, and the
// attempt's error.
type liveResult struct {
	digest   string
	ledger   *LedgerReport
	final    bool     // every per-attempt path (agent, gate, networks, listener) is gone
	teardown []string // what could not be torn down
	err      error
}

// livePhase runs one live attempt. Order (C7BJ-16): the agent runs; the ledger is sealed
// (admission revoked, admitted requests drained); the tree is captured; then the agent
// container, the gate, both networks and the attempt's proxy listener are torn down with
// bounded deadlines (the listener waits for its active handlers); only then is the ledger
// read for the result, on success and on every error path, so a request that arrived during
// capture or teardown is in it as late. A refused gate returns before any agent container
// exists (the ledger is then sealed empty with unverified egress by the caller).
func (d *Docker) livePhase(ctx context.Context, taskDir string, o RunOptions, lim taskLimits, img, artifact string) liveResult {
	var res liveResult
	var cleanups []func() error
	finish := func(err error) liveResult {
		if o.Counter.Report() == nil { // revoke admission on every path that admitted nothing yet
			o.Counter.Seal(5*time.Second, BypassUnverified, ScopeIsolatedContainer)
		}
		res.final = true
		for i := len(cleanups) - 1; i >= 0; i-- {
			if cerr := cleanups[i](); cerr != nil {
				res.final = false
				res.teardown = append(res.teardown, cerr.Error())
			}
		}
		res.ledger, res.err = o.Counter.Report(), err
		return res
	}
	refuse := func(err error) liveResult { return finish(gateRefused(err)) }
	gateImg, err := d.gateImage(ctx)
	if err != nil {
		return refuse(err)
	}
	if _, err := d.probe(ctx, gateImg); err != nil {
		return refuse(err)
	}
	g, err := d.openNetworks(ctx)
	if err != nil {
		return refuse(err)
	}
	cleanups = append(cleanups, func() error { return g.closeErr(d) })
	host, entry, err := d.proxyHost(ctx, g.BGateway)
	if err != nil {
		return refuse(err)
	}
	port, stop, err := o.Live.attach(host)
	if err != nil {
		return refuse(err)
	}
	cleanups = append(cleanups, stop) // runs after the gate is gone (cleanups run in reverse)
	cleanups[0], cleanups[1] = cleanups[1], cleanups[0]
	if err := d.startGate(ctx, g, gateImg, entry, port, lim.agent+time.Minute); err != nil {
		return refuse(err)
	}
	h := g.agentHost()
	pids := int64(1024)
	h.Resources = container.Resources{NanoCPUs: int64(lim.cpus * 1e9), Memory: lim.memoryMB << 20, PidsLimit: &pids}
	id, err := d.create(ctx, &container.Config{Image: img, User: "65534:65534", WorkingDir: "/app", Cmd: o.agentCmd, Env: gateEnv(o.Counter)}, h)
	if err != nil {
		return refuse(err)
	}
	cleanups = append(cleanups, func() error { return d.removeErr(id) })
	// the starting tree, owned by the agent's user (no root step inside the container)
	repo, err := ownedTree(filepath.Join(taskDir, "environment", "repo"))
	if err != nil {
		return finish(err)
	}
	if _, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(repo)}); err != nil {
		return finish(err)
	}
	bypass := d.Bypass(ctx, g, id)
	if bypass != BypassEnforced {
		return refuse(errors.New("the inspected topology is not the probed policy"))
	}
	runErr := d.runToEnd(ctx, id, lim.agent, "agent")
	o.Counter.Seal(5*time.Second, bypass, ScopeIsolatedContainer) // revoke admission and drain; not the final account
	if runErr != nil {
		return finish(runErr)
	}
	src, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: "/app"})
	if err != nil {
		return finish(err)
	}
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		src.Content.Close()
		return finish(err)
	}
	res.digest, err = freezeTree(src.Content, artifact)
	src.Content.Close()
	if err != nil {
		res.digest = ""
	}
	return finish(err)
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
