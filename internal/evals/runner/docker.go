package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// DockerRunnerVersion is the version of the container runner's trial procedure; it is
// recorded with every trial.
const DockerRunnerVersion = "hint-docker-runner-" + DockerRunnerSemver

// DockerRunnerSemver is the runner's version in trial records, configurations and reports.
const DockerRunnerSemver = "0.1.0"

// Docker runs eval trials through the Docker Engine API (embedded SDK; the engine is a
// prerequisite of the controlled eval mode only). One trial: a fresh agent container
// from the image of the task's environment/ directory (never the task root: the hidden
// tests and the reference stay out of the agent's build context); the agent is stopped
// before its /app tree is copied out as a checked, immutable artifact; a separate
// verifier container without network gets that artifact and the hidden tests and writes
// the results. The job directory has the layout ReadJob reads.
type Docker struct {
	cli     *client.Client
	Version string // engine version
	mu      sync.Mutex
	probes  map[string]GateProbe                // passed network probes by key (engine, policy, gate image)
	probeFn func(ctx context.Context) GateProbe // replaces the network probe in tests (nil: ProbeGate)
}

// FindDocker connects to the Docker engine named by the environment (DOCKER_HOST, the
// default socket) and checks that it answers.
func FindDocker(ctx context.Context) (*Docker, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("%w: Docker client: %v", ErrUnavailable, err)
	}
	v, err := cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: the Docker engine is not running or not reachable", ErrUnavailable)
	}
	return &Docker{cli: cli, Version: v.Version}, nil
}

// Architecture is the engine's architecture as it reports it.
func (d *Docker) Architecture(ctx context.Context) (string, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	return info.Info.Architecture, nil
}

// Close releases the client.
func (d *Docker) Close() error { return d.cli.Close() }

// task limits from task.toml (written by Export)
type taskLimits struct {
	agent, verifier time.Duration
	cpus            float64
	memoryMB        int64
	lan             bool
}

var (
	tomlTimeout = regexp.MustCompile(`(?m)^\[(agent|verifier)\][^\[]*?timeout_sec = ([0-9.]+)`)
	tomlCPUs    = regexp.MustCompile(`(?m)^cpus = ([0-9.]+)`)
	tomlMemory  = regexp.MustCompile(`(?m)^memory_mb = ([0-9]+)`)
)

func readLimits(taskDir string) (taskLimits, error) {
	b, err := os.ReadFile(filepath.Join(taskDir, "task.toml"))
	if err != nil {
		return taskLimits{}, err
	}
	s := string(b)
	l := taskLimits{agent: time.Hour, verifier: time.Hour, cpus: 2, memoryMB: 4096, lan: strings.Contains(s, `network_mode = "allowlist"`)}
	for _, m := range tomlTimeout.FindAllStringSubmatch(s, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		if m[1] == "agent" {
			l.agent = time.Duration(v * float64(time.Second))
		} else {
			l.verifier = time.Duration(v * float64(time.Second))
		}
	}
	if m := tomlCPUs.FindStringSubmatch(s); m != nil {
		l.cpus, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := tomlMemory.FindStringSubmatch(s); m != nil {
		l.memoryMB, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return l, nil
}

// Run executes the attempts of one job on a task directory and reads its trials.
func (d *Docker) Run(ctx context.Context, taskDir string, o RunOptions) ([]Trial, error) {
	if err := o.checkStudyLaunch(); err != nil {
		return nil, err
	}
	if o.Attempts <= 0 {
		o.Attempts = 1
	}
	switch o.Agent {
	case "nop", "oracle":
	case "test-agent":
		if o.agentCmd == nil || o.Live == nil || o.Counter == nil || o.Attempts != 1 {
			return nil, errors.New("a live attempt needs the proxy, its counter and one attempt")
		}
	case "opencode":
		if o.Live == nil || o.Counter == nil || o.OpenCode == nil || o.Attempts != 1 {
			return nil, fmt.Errorf("%w: a live opencode attempt needs the model proxy, its counter, the pinned OpenCode and one attempt", ErrUnavailable)
		}
	default:
		return nil, fmt.Errorf("agent %q is not allowed", o.Agent)
	}
	lim, err := readLimits(taskDir)
	if err != nil {
		return nil, err
	}
	if lim.lan {
		return nil, fmt.Errorf("%w: a task with an agent network allowlist is refused (a live agent reaches the model only through HINT's gate)", ErrUnavailable)
	}
	agentID, agentTag, agentCtx, err := d.build(ctx, filepath.Join(taskDir, "environment"), "agent")
	if err != nil {
		return nil, fmt.Errorf("%w: agent image: %v", ErrUnavailable, err)
	}
	iot, err := readIOTask(taskDir)
	if err != nil {
		return nil, err
	}
	// every container is created by the engine's image ID, resolved once here (C5)
	im := images{agentTag: agentTag, agentCtx: agentCtx}
	if im.agentImage, err = d.resolveImage(ctx, agentID); err != nil {
		return nil, fmt.Errorf("%w: agent image: %v", ErrUnavailable, err)
	}
	im.agent = im.agentImage.ID
	if iot == nil { // a B1 task has no verifier image: the runner compares on the host
		var verID string
		if verID, im.verifierTag, im.verifierCtx, err = d.build(ctx, filepath.Join(taskDir, "tests"), "verifier"); err != nil {
			return nil, fmt.Errorf("%w: verifier image: %v", ErrUnavailable, err)
		}
		if im.verifierImage, err = d.resolveImage(ctx, verID); err != nil {
			return nil, fmt.Errorf("%w: verifier image: %v", ErrUnavailable, err)
		}
		im.verifier = im.verifierImage.ID
	}
	prov, err := d.jobProvenance(ctx, taskDir, im)
	if err != nil {
		return nil, err
	}
	job := filepath.Join(o.JobsDir, o.JobName)
	if err := os.MkdirAll(job, 0o700); err != nil {
		return nil, err
	}
	for a := 1; a <= o.Attempts; a++ {
		name := fmt.Sprintf("%s__%d", o.JobName, a)
		d.trial(ctx, taskDir, filepath.Join(job, name), name, o, lim, im, iot, prov)
	}
	return ReadJob(job)
}

// images of a job: agent and verifier are engine image IDs (containers are created by
// them); the tags and context digests are cache hints.
type images struct {
	agent, agentCtx, agentTag          string
	verifier, verifierCtx, verifierTag string
	agentImage, verifierImage          image
}

// trialResult is the result.json of one trial (the fields ReadJob reads, plus the
// container identities of the trial).
type trialResult struct {
	TrialName      string          `json:"trial_name"`
	VerifierResult *verifierResult `json:"verifier_result"`
	ExceptionInfo  *exceptionInfo  `json:"exception_info"`
	StartedAt      string          `json:"started_at"`
	FinishedAt     string          `json:"finished_at"`
	AgentExecution *execution      `json:"agent_execution"`
	Container      map[string]any  `json:"hint_container"`
}

type verifierResult struct {
	Rewards map[string]float64 `json:"rewards"`
}
type exceptionInfo struct {
	ExceptionType string `json:"exception_type"`
}
type execution struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// trial runs one attempt; every failure is recorded as the trial's exception, never as a
// pass.
func (d *Docker) trial(ctx context.Context, taskDir, dir, name string, o RunOptions, lim taskLimits, im images, iot *ioTask, prov map[string]any) {
	_ = os.MkdirAll(filepath.Join(dir, "verifier"), 0o700)
	r := trialResult{TrialName: name, StartedAt: stamp(), Container: map[string]any{"runner": DockerRunnerVersion, "engine": d.Version,
		"agent_image": im.agent, "agent_context": im.agentCtx, "verifier_image": im.verifier, "verifier_context": im.verifierCtx, "provenance": prov}}
	defer func() {
		r.FinishedAt = stamp()
		b, _ := json.MarshalIndent(r, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
	}()
	fail := func(kind string) { r.ExceptionInfo = &exceptionInfo{ExceptionType: kind} }
	exec := &execution{StartedAt: stamp()}
	r.AgentExecution = exec
	artifact := filepath.Join(dir, "agent", "final.tar")
	var digest string
	var err error
	if o.Agent == "test-agent" || o.Agent == "opencode" {
		lr := d.livePhase(ctx, taskDir, o, lim, im.agent, artifact)
		if lr.agentBinary != nil {
			r.Container["agent_binary"] = lr.agentBinary
		}
		if lr.study != nil {
			r.Container["study_launch"] = lr.study
		}
		if lr.prelaunch != nil {
			r.Container["prelaunch_tree"] = lr.prelaunch
		}
		if lr.output != nil {
			r.Container["agent_output"] = lr.output
		}
		digest, err = lr.digest, lr.err
		// the ledger as read after the teardown; never the seal-time snapshot (C7BJ-16)
		r.Container["ledger"], r.Container["ledger_final"], r.Container["ledger_sealed_at"] = lr.ledger, lr.final, lr.sealedAt
		if !lr.final {
			r.Container["teardown_errors"] = lr.teardown
		}
	} else {
		digest, err = d.agentPhase(ctx, taskDir, o.Agent, lim, im.agent, artifact)
	}
	exec.FinishedAt = stamp()
	if err != nil {
		var te timeoutError
		var ee exitError
		var gr gateRefusal
		if errors.As(err, &gr) {
			fail(ErrGateRefused)
		} else if errors.As(err, &te) {
			fail("AgentTimeoutError")
		} else if errors.As(err, &ee) {
			fail("NonZeroAgentExitCodeError")
		} else {
			fail("AgentContainerError")
		}
		r.Container["error"] = err.Error()
		return
	}
	r.Container["final_tree"] = digest
	if iot != nil {
		reward, prov, err := d.ioPhase(ctx, taskDir, iot, lim, im.agent, artifact, dir)
		r.Container["io"] = prov
		if err != nil { // no verdict: never a pass, and no trust statement
			var te timeoutError
			if errors.As(err, &te) {
				fail("VerifierTimeoutError")
			} else {
				fail("VerifierRewardError")
			}
			r.Container["error"] = err.Error()
			r.Container["result_trust"], r.Container["check_trust"] = TrustUnknown, map[string]string{}
			return
		}
		// the verdict came from the runner on captured bytes; no candidate or reference code
		// ran where it was made (07B-J §14)
		r.Container["result_trust"], r.Container["check_trust"] = TrustProcessSeparated, map[string]string{"B1": TrustProcessSeparated}
		r.VerifierResult = &verifierResult{Rewards: map[string]float64{"reward": reward}}
		return
	}
	reward, checks, err := d.verifierPhase(ctx, lim, im.verifier, artifact, dir)
	// trust from the topology that ran: the verifier container executes the candidate's code
	// together with every check it prints (07B-J §11.3)
	ct := map[string]string{}
	for _, c := range checks {
		ct[c] = TrustSameProcess
	}
	r.Container["result_trust"], r.Container["check_trust"] = TrustSameProcess, ct
	if err != nil {
		var te timeoutError
		if errors.As(err, &te) {
			fail("VerifierTimeoutError")
		} else {
			fail("VerifierRewardError")
		}
		r.Container["error"] = err.Error()
		return
	}
	r.VerifierResult = &verifierResult{Rewards: map[string]float64{"reward": reward}}
}

type timeoutError struct{ phase string }

func (t timeoutError) Error() string { return t.phase + " timed out" }

func (d *Docker) hostConfig(lim taskLimits) *container.HostConfig {
	pids := int64(1024)
	return &container.HostConfig{NetworkMode: "none", SecurityOpt: []string{"no-new-privileges:true"},
		Resources: container.Resources{NanoCPUs: int64(lim.cpus * 1e9), Memory: lim.memoryMB << 20, PidsLimit: &pids}}
}

// agentPhase runs the agent in a fresh container, stops it, and writes its checked /app
// tree as the trial artifact; it returns the artifact digest.
func (d *Docker) agentPhase(ctx context.Context, taskDir, agent string, lim taskLimits, img, artifact string) (string, error) {
	return d.applyPhase(ctx, filepath.Join(taskDir, "solution"), agent, lim, img, artifact)
}

// applyPhase is agentPhase with an explicit solution directory (the oracle's solve.sh
// tree). The agent's output (bounded) is kept next to the artifact as oracle.txt, where the
// qualification probe reports what it saw.
func (d *Docker) applyPhase(ctx context.Context, solution, agent string, lim taskLimits, img, artifact string) (string, error) {
	cmd := []string{"sh", "-c", "true"} // nop: the starting state is the final state
	if agent == "oracle" {
		cmd = []string{"sh", "/solution/solve.sh"}
	}
	cid, err := d.create(ctx, &container.Config{Image: img, Cmd: cmd, WorkingDir: "/app"}, d.hostConfig(lim))
	if err != nil {
		return "", err
	}
	defer d.remove(cid)
	if agent == "oracle" { // the reference enters only the oracle's container, never an agent image
		sol, err := tarDir(solution, "solution")
		if err != nil {
			return "", err
		}
		if _, err := d.cli.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(sol)}); err != nil {
			return "", err
		}
	}
	runErr := d.runToEnd(ctx, cid, lim.agent, "agent")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		return "", err
	}
	if logs, err := d.cli.ContainerLogs(ctx, cid, client.ContainerLogsOptions{ShowStdout: true}); err == nil {
		var out bytes.Buffer
		_, _ = stdcopy.StdCopy(&limitWriter{w: &out, n: 64 << 10}, io.Discard, logs)
		logs.Close()
		_ = os.WriteFile(filepath.Join(filepath.Dir(artifact), "oracle.txt"), out.Bytes(), 0o600)
	}
	if runErr != nil {
		return "", runErr
	}
	// the container has stopped: nothing changes the tree during the copy
	src, err := d.cli.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: "/app"})
	if err != nil {
		return "", err
	}
	defer src.Content.Close()
	return freezeTree(src.Content, artifact)
}

// runToEnd starts a container and waits for it to exit within the limit; on timeout or
// cancellation the container is stopped (then killed) and the attempt fails. A nonzero exit
// status or a container error is an error of the phase (C7BI-1): output written before it
// never counts.
func (d *Docker) runToEnd(ctx context.Context, id string, limit time.Duration, phase string) error {
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	w := d.cli.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-w.Result:
		if res.Error != nil && res.Error.Message != "" {
			return fmt.Errorf("%s container: %s", phase, res.Error.Message)
		}
		if res.StatusCode != 0 {
			return exitError{phase, res.StatusCode}
		}
		return nil
	case err := <-w.Error:
		t := 5
		_, _ = d.cli.ContainerStop(context.Background(), id, client.ContainerStopOptions{Timeout: &t})
		if wctx.Err() != nil {
			return timeoutError{phase}
		}
		return err
	}
}

type exitError struct {
	phase string
	code  int64
}

func (e exitError) Error() string { return fmt.Sprintf("%s exited with status %d", e.phase, e.code) }

func (d *Docker) remove(id string) { _ = d.removeErr(id) }

// removeErr force-removes a container (bounded) and reports a failure.
func (d *Docker) removeErr(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		return fmt.Errorf("container %s: %v", id, err)
	}
	return nil
}

// verifierPhase runs the verifier image without network on a copy of the frozen tree and
// reads its outputs: the check lines (stdout, bounded) and the reward.
func (d *Docker) verifierPhase(ctx context.Context, lim taskLimits, img, artifact, dir string) (float64, []string, error) {
	cid, err := d.create(ctx, &container.Config{Image: img, Cmd: []string{"sh", "-c", "mkdir -p /logs/verifier && sh /tests/test.sh"}, WorkingDir: "/app"},
		d.hostConfig(lim))
	if err != nil {
		return 0, nil, err
	}
	defer d.remove(cid)
	f, err := os.Open(artifact)
	if err != nil {
		return 0, nil, err
	}
	_, err = d.cli.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: "/", Content: f})
	f.Close()
	if err != nil {
		return 0, nil, err
	}
	var checks []string // the checks the verifier printed (root's stdout)
	runErr := d.runToEnd(ctx, cid, lim.verifier, "verifier")
	logs, err := d.cli.ContainerLogs(ctx, cid, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err == nil {
		var out, errb bytes.Buffer
		_, _ = stdcopy.StdCopy(&limitWriter{w: &out, n: 4 << 20}, &limitWriter{w: &errb, n: 1 << 20}, logs)
		logs.Close()
		_ = os.WriteFile(filepath.Join(dir, "verifier", "test-stdout.txt"), out.Bytes(), 0o600)
		for _, ln := range strings.Split(out.String(), "\n") {
			if c := checkLine.FindStringSubmatch(strings.TrimSpace(ln)); c != nil {
				checks = append(checks, c[1])
			}
		}
	}
	if runErr != nil {
		return 0, nil, runErr
	}
	b, err := d.readSmall(ctx, cid, "/logs/verifier/reward.txt")
	if err != nil {
		return 0, nil, fmt.Errorf("no reward file")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, nil, fmt.Errorf("unreadable reward value")
	}
	// the generated verifier's own network record (the isolation check reads it): only a whole
	// file that is exactly one of the two records is kept; anything else stays absent (missing
	// evidence, never a pass)
	if n, err := d.readSmall(ctx, cid, "/logs/verifier/network.txt"); err == nil {
		switch rec := strings.TrimSpace(string(n)); rec {
		case "HINT_VERIFIER_NET=blocked", "HINT_VERIFIER_NET=reachable":
			_ = os.WriteFile(filepath.Join(dir, "verifier", "network.txt"), []byte(rec+"\n"), 0o600)
		}
	}
	return v, checks, nil
}

// maxSmallFile bounds the verifier's small result files (reward, network record).
const maxSmallFile = 64

// readSmall reads one whole regular file of at most maxSmallFile bytes from a stopped
// container. A larger, non-regular, cut or unreadable file, or an archive with another entry,
// is an error: evidence is never shortened into a valid value.
func (d *Docker) readSmall(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := d.cli.CopyFromContainer(ctx, cid, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, err
	}
	defer res.Content.Close()
	tr := tar.NewReader(io.LimitReader(res.Content, 64<<10))
	h, err := tr.Next()
	switch {
	case err != nil:
		return nil, fmt.Errorf("unreadable %s: %v", path, err)
	case h.Typeflag != tar.TypeReg:
		return nil, fmt.Errorf("%s is not a regular file", path)
	case h.Size < 0 || h.Size > maxSmallFile:
		return nil, fmt.Errorf("%s has %d bytes (limit %d)", path, h.Size, maxSmallFile)
	}
	b := make([]byte, h.Size)
	if _, err := io.ReadFull(tr, b); err != nil {
		return nil, fmt.Errorf("%s is cut: %v", path, err)
	}
	if _, err := tr.Next(); err != io.EOF {
		return nil, fmt.Errorf("%s: the archive is not exactly one complete file", path)
	}
	return b, nil
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := int64(len(p))
		if k > l.n {
			k = l.n
		}
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// Limits of a frozen tree.
const (
	maxTreeFiles = 200000
	maxTreeBytes = 2 << 30
)

// freezeTree copies the tar stream of a stopped container's /app into the artifact file
// and checks it on the way: regular files and directories with their modes; symlinks
// kept (never followed on the host) when they resolve inside the tree; hard links to an
// earlier regular file of the tree; device files, FIFOs, sockets, non-canonical or
// duplicate paths, entries below a symlink and any path that leaves the tree make the
// attempt invalid; file count, content size and total raw archive bytes are bounded.
// Complete tar end blocks and zero file/record padding are required. It returns the artifact digest;
// a refused tree leaves no artifact.
func freezeTree(r io.Reader, artifact string) (string, error) {
	return freezeTreeBounded(r, artifact, maxPrelaunchArchiveBytes)
}

// The separate limit permits small offline boundary tests; production always uses the
// fixed raw archive bound above, in addition to content and entry limits.
func freezeTreeBounded(r io.Reader, artifact string, rawLimit int64) (string, error) {
	f, err := os.OpenFile(artifact, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	raw := &treeArchiveReader{source: r, limit: rawLimit}
	tr := tar.NewReader(raw)
	var size, padding int64
	tree := map[string]*tar.Header{} // canonical path below app -> entry
	err = func() error {
		for {
			before := raw.read
			raw.padding = padding
			hd, err := tr.Next()
			if err == io.EOF {
				if err := raw.finish(before, padding); err != nil {
					return err
				}
				return checkLinks(tree)
			}
			if err != nil {
				return err
			}
			padding = (tarBlockBytes - hd.Size%tarBlockBytes) % tarBlockBytes
			p, ok := treePath(hd.Name)
			if !ok {
				return fmt.Errorf("path outside the tree or not canonical: %q", hd.Name)
			}
			if _, dup := tree[p]; dup {
				return fmt.Errorf("duplicate path: %q", hd.Name)
			}
			switch hd.Typeflag {
			case tar.TypeDir, tar.TypeReg:
			case tar.TypeSymlink:
				if hd.Linkname == "" || path.IsAbs(hd.Linkname) {
					return fmt.Errorf("symlink leaves the tree: %q", hd.Name)
				}
			case tar.TypeLink:
				t, ok := treePath(hd.Linkname)
				if !ok || tree[t] == nil || tree[t].Typeflag != tar.TypeReg {
					return fmt.Errorf("hard link not to an earlier file of the tree: %q", hd.Name)
				}
			default:
				return fmt.Errorf("unsupported file type %q: %q", string(hd.Typeflag), hd.Name)
			}
			tree[p] = &tar.Header{Typeflag: hd.Typeflag, Linkname: hd.Linkname}
			size += hd.Size
			if len(tree) > maxTreeFiles || size > maxTreeBytes {
				return fmt.Errorf("the tree is too large")
			}
			if err := tw.WriteHeader(hd); err != nil {
				return err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}()
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(artifact)
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// treePath maps an archive name to its path below app ("" is app itself). Every component
// must be a plain name: no empty, "." or ".." component, so a dotted file name such as
// config..bak is fine and a traversal is not.
func treePath(name string) (string, bool) {
	name = strings.TrimSuffix(name, "/")
	if name == "app" {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, "app/")
	if !ok {
		return "", false
	}
	for _, c := range strings.Split(rest, "/") {
		if c == "" || c == "." || c == ".." {
			return "", false
		}
	}
	return rest, true
}

// maxLinkHops bounds symlink resolution like the kernel's limit; a longer chain or a cycle
// refuses the tree.
const maxLinkHops = 40

// checkLinks validates the whole link graph of a frozen tree with filesystem resolution
// semantics (C7BI-2), without touching the host: no entry lies below a symlink, and every
// symlink resolves, component by component and through chains of symlinks, without ever
// leaving app. A missing component is taken lexically, so a dangling link cannot point out
// of the tree either.
func checkLinks(tree map[string]*tar.Header) error {
	for p := range tree {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if e := tree[d]; e != nil && e.Typeflag != tar.TypeDir {
				return fmt.Errorf("entry below a non-directory: %q", "app/"+p)
			}
		}
	}
	for p, e := range tree {
		if e.Typeflag != tar.TypeSymlink {
			continue
		}
		var stack []string // resolved components below app
		if d := path.Dir(p); d != "." {
			stack = strings.Split(d, "/")
		}
		queue := strings.Split(e.Linkname, "/")
		hops := 0
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			switch c {
			case "", ".":
				continue
			case "..":
				if len(stack) == 0 {
					return fmt.Errorf("symlink leaves the tree: %q", "app/"+p)
				}
				stack = stack[:len(stack)-1]
				continue
			}
			stack = append(stack, c)
			if t := tree[strings.Join(stack, "/")]; t != nil && t.Typeflag == tar.TypeSymlink {
				if hops++; hops > maxLinkHops || path.IsAbs(t.Linkname) {
					return fmt.Errorf("symlink cycle or escape: %q", "app/"+p)
				}
				stack = stack[:len(stack)-1]
				queue = append(strings.Split(t.Linkname, "/"), queue...)
			}
		}
	}
	return nil
}

// build builds the image of a build context directory (only that directory) and returns
// the engine's image ID from the build itself, the cache tag and the context digest. The
// build always runs (the engine's layer cache makes a rebuild cheap): an existing tag is
// never trusted as the image of a context, because a tag can be moved (C5).
func (d *Docker) build(ctx context.Context, dir, kind string) (id, tag, contextDigest string, err error) {
	b, err := tarDir(dir, "")
	if err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256(b)
	contextDigest = "sha256:" + hex.EncodeToString(sum[:])
	tag = "hint-eval-" + kind + ":" + hex.EncodeToString(sum[:10])
	res, err := d.cli.ImageBuild(ctx, bytes.NewReader(b), client.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true})
	if err != nil {
		return "", "", "", err
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	// every line of the build stream must be a protocol message, and the stream must end
	// cleanly: an aux ID before a read failure, an overflow or a malformed line is no
	// build (C7BJ-12)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m struct {
			Error string `json:"error"`
			Aux   struct {
				ID string `json:"ID"`
			} `json:"aux"`
		}
		if json.Unmarshal(line, &m) != nil {
			return "", "", "", errors.New("malformed build stream")
		}
		if m.Error != "" {
			return "", "", "", errors.New(m.Error)
		}
		if strings.HasPrefix(m.Aux.ID, "sha256:") {
			id = m.Aux.ID
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", "", fmt.Errorf("incomplete build stream: %w", err)
	}
	if id == "" {
		return "", "", "", fmt.Errorf("the build reported no image ID")
	}
	if _, err := d.cli.ImageInspect(ctx, id); err != nil {
		return "", "", "", fmt.Errorf("the image was not built")
	}
	return id, tag, contextDigest, nil
}

// tarDir is a deterministic tar of a directory (sorted, fixed times and owners) with an
// optional prefix; symlinks are kept as symlinks.
func tarDir(dir, prefix string) ([]byte, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, p)
		name := filepath.ToSlash(rel)
		if prefix != "" {
			name = prefix + "/" + name
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return nil, err
			}
		}
		hd, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil, err
		}
		hd.Name, hd.ModTime, hd.Uid, hd.Gid, hd.Uname, hd.Gname = name, time.Unix(0, 0), 0, 0, "", ""
		hd.AccessTime, hd.ChangeTime, hd.Format = time.Time{}, time.Time{}, tar.FormatPAX
		if info.IsDir() {
			hd.Name += "/"
		}
		if err := tw.WriteHeader(hd); err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
