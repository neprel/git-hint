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
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// DockerRunnerVersion is the version of the container runner's trial procedure; it is
// recorded with every trial.
const DockerRunnerVersion = "hint-docker-runner-0.1.0"

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
	if o.Attempts <= 0 {
		o.Attempts = 1
	}
	switch o.Agent {
	case "nop", "oracle":
	case "opencode":
		return nil, fmt.Errorf("%w: the container runner does not run live agents yet (the model gate is not built)", ErrUnavailable)
	default:
		return nil, fmt.Errorf("agent %q is not allowed", o.Agent)
	}
	lim, err := readLimits(taskDir)
	if err != nil {
		return nil, err
	}
	if lim.lan {
		return nil, fmt.Errorf("%w: LAN model tasks need the model gate, which the container runner does not have yet", ErrUnavailable)
	}
	agentImg, agentCtx, err := d.build(ctx, filepath.Join(taskDir, "environment"), "agent")
	if err != nil {
		return nil, fmt.Errorf("%w: agent image: %v", ErrUnavailable, err)
	}
	verImg, verCtx, err := d.build(ctx, filepath.Join(taskDir, "tests"), "verifier")
	if err != nil {
		return nil, fmt.Errorf("%w: verifier image: %v", ErrUnavailable, err)
	}
	job := filepath.Join(o.JobsDir, o.JobName)
	if err := os.MkdirAll(job, 0o700); err != nil {
		return nil, err
	}
	for a := 1; a <= o.Attempts; a++ {
		name := fmt.Sprintf("%s__%d", o.JobName, a)
		d.trial(ctx, taskDir, filepath.Join(job, name), name, o.Agent, lim, images{agentImg, agentCtx, verImg, verCtx})
	}
	return ReadJob(job)
}

type images struct{ agent, agentCtx, verifier, verifierCtx string }

// trialResult is the result.json of one trial (the fields ReadJob reads, plus the
// container identities of the trial).
type trialResult struct {
	TrialName      string            `json:"trial_name"`
	VerifierResult *verifierResult   `json:"verifier_result"`
	ExceptionInfo  *exceptionInfo    `json:"exception_info"`
	StartedAt      string            `json:"started_at"`
	FinishedAt     string            `json:"finished_at"`
	AgentExecution *execution        `json:"agent_execution"`
	Container      map[string]string `json:"hint_container"`
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
func (d *Docker) trial(ctx context.Context, taskDir, dir, name, agent string, lim taskLimits, im images) {
	_ = os.MkdirAll(filepath.Join(dir, "verifier"), 0o700)
	r := trialResult{TrialName: name, StartedAt: stamp(), Container: map[string]string{"runner": DockerRunnerVersion, "engine": d.Version,
		"agent_image": im.agent, "agent_context": im.agentCtx, "verifier_image": im.verifier, "verifier_context": im.verifierCtx}}
	defer func() {
		r.FinishedAt = stamp()
		b, _ := json.MarshalIndent(r, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
	}()
	fail := func(kind string) { r.ExceptionInfo = &exceptionInfo{ExceptionType: kind} }
	exec := &execution{StartedAt: stamp()}
	r.AgentExecution = exec
	artifact := filepath.Join(dir, "agent", "final.tar")
	digest, err := d.agentPhase(ctx, taskDir, agent, lim, im.agent, artifact)
	exec.FinishedAt = stamp()
	if err != nil {
		var te timeoutError
		if errors.As(err, &te) {
			fail("AgentTimeoutError")
		} else {
			fail("AgentContainerError")
		}
		r.Container["error"] = err.Error()
		return
	}
	r.Container["final_tree"] = digest
	reward, err := d.verifierPhase(ctx, lim, im.verifier, artifact, dir)
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
	cmd := []string{"sh", "-c", "true"} // nop: the starting state is the final state
	if agent == "oracle" {
		cmd = []string{"sh", "/solution/solve.sh"}
	}
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: img, Cmd: cmd, WorkingDir: "/app"}, HostConfig: d.hostConfig(lim)})
	if err != nil {
		return "", err
	}
	defer d.remove(c.ID)
	if agent == "oracle" { // the reference enters only the oracle's container, never an agent image
		sol, err := tarDir(filepath.Join(taskDir, "solution"), "solution")
		if err != nil {
			return "", err
		}
		if _, err := d.cli.CopyToContainer(ctx, c.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(sol)}); err != nil {
			return "", err
		}
	}
	if err := d.runToEnd(ctx, c.ID, lim.agent, "agent"); err != nil {
		return "", err
	}
	// the container has stopped: nothing changes the tree during the copy
	src, err := d.cli.CopyFromContainer(ctx, c.ID, client.CopyFromContainerOptions{SourcePath: "/app"})
	if err != nil {
		return "", err
	}
	defer src.Content.Close()
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		return "", err
	}
	return freezeTree(src.Content, artifact)
}

// runToEnd starts a container and waits for it to exit within the limit; on timeout or
// cancellation the container is stopped (then killed) and the attempt fails.
func (d *Docker) runToEnd(ctx context.Context, id string, limit time.Duration, phase string) error {
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	w := d.cli.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-w.Result:
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

func (d *Docker) remove(id string) {
	_, _ = d.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
}

// verifierPhase runs the verifier image without network on a copy of the frozen tree and
// reads its outputs: the check lines (stdout, bounded) and the reward.
func (d *Docker) verifierPhase(ctx context.Context, lim taskLimits, img, artifact, dir string) (float64, error) {
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: img, Cmd: []string{"sh", "-c", "mkdir -p /logs/verifier && sh /tests/test.sh"}, WorkingDir: "/app"},
		HostConfig: d.hostConfig(lim)})
	if err != nil {
		return 0, err
	}
	defer d.remove(c.ID)
	f, err := os.Open(artifact)
	if err != nil {
		return 0, err
	}
	_, err = d.cli.CopyToContainer(ctx, c.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: f})
	f.Close()
	if err != nil {
		return 0, err
	}
	runErr := d.runToEnd(ctx, c.ID, lim.verifier, "verifier")
	logs, err := d.cli.ContainerLogs(ctx, c.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err == nil {
		var out, errb bytes.Buffer
		_, _ = stdcopy.StdCopy(&limitWriter{w: &out, n: 4 << 20}, &limitWriter{w: &errb, n: 1 << 20}, logs)
		logs.Close()
		_ = os.WriteFile(filepath.Join(dir, "verifier", "test-stdout.txt"), out.Bytes(), 0o600)
	}
	if runErr != nil {
		return 0, runErr
	}
	res, err := d.cli.CopyFromContainer(ctx, c.ID, client.CopyFromContainerOptions{SourcePath: "/logs/verifier/reward.txt"})
	if err != nil {
		return 0, fmt.Errorf("no reward file")
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return 0, fmt.Errorf("unreadable reward file")
	}
	b, _ := io.ReadAll(io.LimitReader(tr, 64))
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, fmt.Errorf("unreadable reward value")
	}
	return v, nil
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
// kept (never followed) when their target stays inside the tree; hard links inside the
// tree; device files, FIFOs, sockets and any path that leaves the tree make the attempt
// invalid; file count and size are bounded. It returns the digest of the artifact.
func freezeTree(r io.Reader, artifact string) (string, error) {
	f, err := os.OpenFile(artifact, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	tr := tar.NewReader(r)
	var files int
	var size int64
	inside := func(p string) bool {
		c := path.Clean(p)
		return c == "app" || strings.HasPrefix(c, "app/")
	}
	err = func() error {
		for {
			hd, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if !inside(hd.Name) || strings.Contains(hd.Name, "..") {
				return fmt.Errorf("path outside the tree: %q", hd.Name)
			}
			switch hd.Typeflag {
			case tar.TypeDir, tar.TypeReg:
			case tar.TypeSymlink:
				if path.IsAbs(hd.Linkname) || !inside(path.Join(path.Dir(hd.Name), hd.Linkname)) {
					return fmt.Errorf("symlink leaves the tree: %q", hd.Name)
				}
			case tar.TypeLink:
				if !inside(hd.Linkname) {
					return fmt.Errorf("hard link leaves the tree: %q", hd.Name)
				}
			default:
				return fmt.Errorf("unsupported file type %q: %q", string(hd.Typeflag), hd.Name)
			}
			files++
			size += hd.Size
			if files > maxTreeFiles || size > maxTreeBytes {
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

// build builds (or reuses) the image of a build context directory. The image tag is the
// digest of the context; the context is only that directory.
func (d *Docker) build(ctx context.Context, dir, kind string) (tag, contextDigest string, err error) {
	b, err := tarDir(dir, "")
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	contextDigest = "sha256:" + hex.EncodeToString(sum[:])
	tag = "hint-eval-" + kind + ":" + hex.EncodeToString(sum[:10])
	if _, err := d.cli.ImageInspect(ctx, tag); err == nil {
		return tag, contextDigest, nil
	}
	res, err := d.cli.ImageBuild(ctx, bytes.NewReader(b), client.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true})
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Error != "" {
			return "", "", errors.New(m.Error)
		}
	}
	if _, err := d.cli.ImageInspect(ctx, tag); err != nil {
		return "", "", fmt.Errorf("the image was not built")
	}
	return tag, contextDigest, nil
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
