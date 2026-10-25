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
	return openCodeEnvironment(c.token, "")
}

// liveResult is what a live attempt leaves: the frozen tree's digest, the ledger as read
// after every per-attempt path was torn down, whether that teardown completed, and the
// attempt's error.
type liveResult struct {
	prelaunch *PrelaunchTreeCapture
	study     *StudyLaunchReceipt
	digest    string
	ledger    *LedgerReport
	final     bool     // every per-attempt path (agent, gate, networks, listener) is gone
	teardown  []string // what could not be torn down
	err       error
	// opencode attempts: the installed binary's provenance and the capture of its output
	agentBinary map[string]any
	output      map[string]string
	sealedAt    string // when admission was revoked (the ledger's seal time)
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
	if err := o.checkStudyLaunch(); err != nil {
		if o.Counter != nil {
			o.Counter.Seal(0, BypassUnverified, ScopeIsolatedContainer)
			res.ledger = o.Counter.Report()
			res.sealedAt = o.Counter.SealedAt()
		}
		res.err, res.final = err, true
		return res
	}
	expected := o.ExpectedBackend.clone() // freeze the caller value before setup/capture
	var cleanups []func() error
	digest := "" // the attempt's execution digest; no agent_run is recorded without it
	if o.Agent == "test-agent" {
		digest = o.testDigest
	}
	// a capture-enabled attempt chooses the deferred final assembly before anything can seal,
	// then captures the backend before the agent's launch
	var capt *capturer
	var startCap captureSide
	if expected != nil {
		startCap.status = CaptureNotAttempted
	}
	if transport, ref := o.captureOptions(); transport != nil && ref != nil && o.Counter.deferAssembly() {
		capt = newCapturer(transport, ref)
		if expected == nil {
			startCap = capt.capture(ctx, o.Counter)
		}
	}
	finish := func(err error) liveResult {
		if res.study != nil && res.study.State != "prepared" {
			end := o.Counter.mark().Milliseconds()
			res.study.FinishedMs = &end
		}
		// refused before the agent's launch; after the launch runAgentEnclosed has already
		// recorded the outcome and the first record is kept
		if digest != "" {
			o.Counter.recordAgentRun(agentRun{outcome: AgentRefusedBeforeLaunch, digest: digest})
		}
		if !o.Counter.sealStarted() { // revoke admission on every path that admitted nothing yet
			o.Counter.Seal(5*time.Second, BypassUnverified, ScopeIsolatedContainer)
			res.sealedAt = o.Counter.SealedAt()
		}
		var endCap captureSide
		if capt != nil { // after the drain end (Seal returned), before the teardown
			endCap = capt.capture(ctx, o.Counter)
		}
		res.final = true
		for i := len(cleanups) - 1; i >= 0; i-- {
			if cerr := cleanups[i](); cerr != nil {
				res.final = false
				res.teardown = append(res.teardown, cerr.Error())
			}
		}
		if capt != nil { // the one final assembly, after the teardown attempt, on every path
			_ = o.Counter.assemble(startCap, endCap)
		}
		res.ledger, res.err = o.Counter.Report(), err
		return res
	}
	refuse := func(err error) liveResult { return finish(gateRefused(err)) }
	if expected != nil && capt == nil {
		return finish(errors.New("expected backend capture unavailable"))
	}
	cmd := o.agentCmd
	env := gateEnv(o.Counter)
	var recipe openCodeRecipe
	var install []byte
	if o.Agent == "opencode" { // target and binary first: nothing is set up for a refused install
		bin, err := d.openCodeBinaryFor(ctx, *o.OpenCode, img)
		if err != nil {
			return finish(agentInstallError{err})
		}
		// the attempt runs only with the target that was frozen in the run's configuration
		if o.ExpectTarget == "" || bin.prov["target"] != o.ExpectTarget {
			return finish(agentInstallError{fmt.Errorf("the attempt's target %v is not the run's frozen target %q", bin.prov["target"], o.ExpectTarget)})
		}
		res.agentBinary = bin.prov
		instr, err := taskInstruction(taskDir)
		if err != nil {
			return finish(agentInstallError{err})
		}
		recipe, err = makeOpenCodeRecipe(o.Live.model, instr, o.Counter.token, o.StudyLaunchPolicy)
		if err != nil {
			return finish(agentInstallError{err})
		}
		var rg []byte
		if o.Ripgrep != nil {
			rg, err = o.Ripgrep.binary(ctx, fmt.Sprint(bin.prov["target"]))
			if err != nil {
				return finish(agentInstallError{err})
			}
			recipe, err = recipe.withRipgrep(fmt.Sprint(bin.prov["target"]))
			if err != nil {
				return finish(agentInstallError{err})
			}
			bin.prov["ripgrep_sha256"] = ripgrepSHA256
			bin.prov["ripgrep_version"] = ripgrepVersion
		}
		if install, err = recipe.archiveWithRipgrep(bin.data, rg); err != nil {
			return finish(agentInstallError{err})
		}
		cmd, env = recipe.Argv, recipe.Env
		digest, err = openCodeExecutionDigestRuntime(o.Live.model, instr, bin.prov, img, o.StudyLaunchPolicy, o.Ripgrep != nil)
		if err != nil {
			return finish(agentInstallError{err})
		}
		if o.StudyLaunchPolicy != "" {
			a, err := dockerLaunchDigestRuntime(fmt.Sprint(bin.prov["version"]), o.Live.model, fmt.Sprint(bin.prov["target"]), o.StudyLaunchPolicy, o.Ripgrep != nil)
			if err != nil || a != o.ExpectLaunchDigest {
				return finish(agentInstallError{errors.New("study actual launch differs from frozen planned digest")})
			}
			studyStart := o.Counter.mark().Milliseconds()
			res.study = &StudyLaunchReceipt{Policy: o.StudyLaunchPolicy, State: "failed", Clock: "ledger_anchor_monotonic_ms", StartedMs: &studyStart, LaunchDigest: a, ExecutionDigest: digest, ImageID: img, BinaryValidation: "not_run", Eligibility: "requires_real_binary_validation"}
		}
		res.agentBinary["execution_digest"] = digest
	}
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
	id, err := d.create(ctx, &container.Config{Image: img, User: "65534:65534", WorkingDir: "/app", Cmd: cmd, Entrypoint: []string{}, Env: env}, h)
	if err != nil {
		return refuse(err)
	}
	cleanups = append(cleanups, func() error { return d.removeErr(id) })
	var studyCtx context.Context
	if res.study != nil {
		var cancel context.CancelFunc
		studyCtx, cancel = context.WithTimeout(ctx, studyInspectionBound)
		defer cancel()
		launcher, err := d.inspectStudyBefore(studyCtx, id, img, recipe)
		if err != nil {
			return finish(errors.New("study pre-install inspection failed"))
		}
		res.study.LauncherDigest = launcher
	}
	// the starting tree, owned by the agent's user (no root step inside the container)
	repo, err := ownedTree(filepath.Join(taskDir, "environment", "repo"))
	if err != nil {
		return finish(err)
	}
	if _, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(repo)}); err != nil {
		return finish(err)
	}
	if install != nil {
		if _, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(install)}); err != nil {
			return finish(agentInstallError{err})
		}
	}
	bypass := d.Bypass(ctx, g, id)
	if bypass != BypassEnforced {
		return refuse(errors.New("the inspected topology is not the probed policy"))
	}
	if res.study != nil {
		inventory, err := d.inspectStudyAfter(studyCtx, id, img, recipe)
		if err != nil {
			return finish(errors.New("study initial state inspection failed"))
		}
		res.study.InventoryDigest = inventory
		res.study.State = "prepared"
		end := o.Counter.mark().Milliseconds()
		res.study.FinishedMs = &end
	}
	if o.CapturePrelaunchTree {
		var err error
		res.prelaunch, err = d.capturePrelaunchTree(ctx, id, filepath.Join(filepath.Dir(artifact), "prelaunch.tar"), digest, o.Counter)
		if err != nil {
			return finish(err)
		}
	}
	// Guarded runs reuse the normal start capture here, after setup and before caller intent.
	// This does not prevent a later backend change; downstream capture checks remain required.
	if expected != nil {
		startCap = capt.capture(ctx, o.Counter)
		if ctx.Err() != nil || !expectedBackendMatches(expected, startCap) {
			return finish(errors.New("expected backend start capture differs or is unavailable"))
		}
	}
	// The last step before start: caller intent follows every prestart check (study only).
	if o.BeforeAgentStart != nil {
		if res.study == nil || res.study.State != "prepared" {
			return finish(errors.New("a launch hook requires a prepared study launch"))
		}
		facts := LaunchFacts{LaunchDigest: res.study.LaunchDigest, ExecutionDigest: digest, GatePolicyDigest: GatePolicyDigest(),
			Target: fmt.Sprint(res.agentBinary["target"]), Model: o.Live.model}
		if err := o.BeforeAgentStart(facts); err != nil {
			return finish(fmt.Errorf("the launch hook refused the attempt: %w", err))
		}
	}
	runErr := runAgentEnclosed(ctx, d.cli, id, lim.agent, o.Counter, digest)
	o.Counter.Seal(5*time.Second, bypass, ScopeIsolatedContainer) // revoke admission and drain; not the final account
	res.sealedAt = o.Counter.SealedAt()
	if o.Agent == "opencode" {
		res.output = d.saveOutput(ctx, id, filepath.Join(filepath.Dir(artifact), "opencode"))
	}
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

// agentInstallError is an attempt whose agent could not be installed (no launch).
type agentInstallError struct{ err error }

func (a agentInstallError) Error() string {
	return "the agent could not be installed: " + a.err.Error()
}

// saveOutput writes the agent's stdout and stderr separately (each complete and at most
// 8 MiB) next to the artifact and says for each whether it is complete; a partial or
// unavailable stream is never written as if it were complete.
func (d *Docker) saveOutput(ctx context.Context, id, base string) map[string]string {
	st := map[string]string{"stdout": "unavailable", "stderr": "unavailable"}
	if os.MkdirAll(filepath.Dir(base), 0o700) != nil {
		return st
	}
	logs, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return st
	}
	defer logs.Close()
	out, errb := &capWriter{max: 8 << 20}, &capWriter{max: 8 << 20}
	if err := demux(out, errb, logs); err != nil {
		st["stdout"], st["stderr"] = "incomplete", "incomplete"
		return st
	}
	for name, w := range map[string]*capWriter{"stdout": out, "stderr": errb} {
		if w.over {
			st[name] = "truncated"
			continue
		}
		file := base + ".txt"
		if name == "stderr" {
			file = base + ".stderr.txt"
		}
		if os.WriteFile(file, w.buf.Bytes(), 0o600) == nil {
			st[name] = "complete"
		}
	}
	return st
}

type gateRefusal struct{ err error }

func (g gateRefusal) Error() string { return "the model gate refused the attempt: " + g.err.Error() }

func gateRefused(err error) error { return gateRefusal{err} }
