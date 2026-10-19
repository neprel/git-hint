package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func tarOf(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg && h.Size == 0 {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	tw.Close()
	return &b
}

// C3: the frozen tree keeps files, directories, inside symlinks and hard links; anything
// that leaves the tree or is not a plain file type invalidates the attempt and leaves no
// artifact.
func TestFreezeTree(t *testing.T) {
	dir := t.TempDir()
	ok := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, &tar.Header{Name: "app/a.go", Typeflag: tar.TypeReg, Mode: 0o644},
		&tar.Header{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "a.go"}, &tar.Header{Name: "app/hard", Typeflag: tar.TypeLink, Linkname: "app/a.go"})
	d1, err := freezeTree(bytes.NewReader(ok.Bytes()), filepath.Join(dir, "ok.tar"))
	if err != nil || !strings.HasPrefix(d1, "sha256:") {
		t.Fatalf("good tree: %v %s", err, d1)
	}
	d2, _ := freezeTree(bytes.NewReader(ok.Bytes()), filepath.Join(dir, "ok2.tar"))
	if d1 != d2 {
		t.Fatal("the digest of the same tree differs")
	}
	for name, h := range map[string]*tar.Header{
		"absolute symlink":   {Name: "app/evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"escaping symlink":   {Name: "app/evil", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
		"escaping hard link": {Name: "app/evil", Typeflag: tar.TypeLink, Linkname: "etc/passwd"},
		"path outside":       {Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644},
		"dot-dot path":       {Name: "app/../etc/x", Typeflag: tar.TypeReg, Mode: 0o644},
		"device":             {Name: "app/dev", Typeflag: tar.TypeChar, Mode: 0o644},
		"fifo":               {Name: "app/fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".tar")
		if _, err := freezeTree(tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, h), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s: a refused tree left an artifact", name)
		}
	}
}

func TestReadLimitsOfAnExportedTask(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSettings
	s.AgentSeconds, s.VerifierSeconds = 120, 60
	os.WriteFile(filepath.Join(dir, "task.toml"), []byte(taskTOML("hint/x-v1", 1, "x", s)), 0o644)
	l, err := readLimits(dir)
	if err != nil || l.agent != 120*time.Second || l.verifier != 60*time.Second || l.lan || l.cpus != s.CPUs || l.memoryMB != int64(s.MemoryMB) {
		t.Fatalf("limits %+v %v", l, err)
	}
	s.Mode, s.LANHost = LANModel, "host.docker.internal"
	os.WriteFile(filepath.Join(dir, "task.toml"), []byte(taskTOML("hint/x-v1", 1, "x", s)), 0o644)
	if l, _ := readLimits(dir); !l.lan {
		t.Fatal("a LAN task is not recognized")
	}
}

// baseImage is a local image for the engine tests (no pull in the test).
const baseImage = "ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55"

func dockerOrSkip(t *testing.T) *Docker {
	t.Helper()
	if os.Getenv("HINT_DOCKER_TESTS") != "1" {
		t.Skip("set HINT_DOCKER_TESTS=1 to run the container runner against the local engine")
	}
	d, err := FindDocker(context.Background())
	if err != nil {
		t.Skip(err)
	}
	if _, err := d.cli.ImageInspect(context.Background(), baseImage); err != nil {
		t.Skipf("the base image %s is not present locally", baseImage)
	}
	return d
}

// syntheticTask is a task directory in the layout Export writes: environment/ (the agent
// image), tests/ (the verifier image with the hidden check), solution/ (the reference).
func syntheticTask(t *testing.T, solve string, agentSeconds int) string {
	t.Helper()
	dir := t.TempDir()
	w := func(rel, s string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	s := DefaultSettings
	s.AgentSeconds, s.VerifierSeconds = agentSeconds, 60
	w("task.toml", taskTOML("hint/synthetic-v1", 1, "synthetic", s), 0o644)
	w("environment/Dockerfile", "FROM "+baseImage+"\nWORKDIR /app\nCOPY repo/ /app/\n", 0o644)
	w("environment/repo/a.txt", "broken\n", 0o644)
	w("tests/Dockerfile", "FROM "+baseImage+"\nCOPY . /tests\n", 0o644)
	w("tests/hidden_check.txt", "HINTCANARY_HIDDEN_TEST\n", 0o644)
	w("tests/test.sh", `#!/bin/sh
mkdir -p /logs/verifier
net=$(ls /sys/class/net | tr '\n' ' ')
echo "NET $net"
if [ "$(cat /app/a.txt)" = "fixed" ]; then echo "CHECK C1 PASS"; echo 1 > /logs/verifier/reward.txt; else echo "CHECK C1 FAIL"; echo 0 > /logs/verifier/reward.txt; fi
`, 0o755)
	w("solution/solve.sh", solve, 0o755)
	w("solution/files/a.txt", "fixed", 0o644)
	return dir
}

const solveFixed = "#!/bin/sh\nset -eu\ncp -R /solution/files/. /app/\n"

// The vertical slice against the local engine: nop fails, the oracle passes, the agent
// image holds no hidden test or reference, the verifier has no network interface but lo,
// the frozen tree and the container identities are recorded, ReadJob reads the job.
func TestDockerRunnerVerticalSlice(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	task := syntheticTask(t, solveFixed, 120)
	jobs := t.TempDir()
	ctx := context.Background()
	nop, err := d.Run(ctx, task, RunOptions{Agent: "nop", JobsDir: jobs, JobName: "base"})
	if err != nil || len(nop) != 1 || nop[0].Outcome() != "failed" || nop[0].Checks["C1"] != "fail" {
		b, _ := os.ReadFile(filepath.Join(jobs, "base", "base__1", "result.json"))
		t.Fatalf("nop: %+v %v\n%s", nop, err, b)
	}
	ora, err := d.Run(ctx, task, RunOptions{Agent: "oracle", Attempts: 2, JobsDir: jobs, JobName: "ref"})
	if err != nil || len(ora) != 2 || ora[0].Outcome() != "passed" || ora[1].Outcome() != "passed" {
		t.Fatalf("oracle: %+v %v", ora, err)
	}
	out, _ := os.ReadFile(filepath.Join(jobs, "ref", "ref__1", "verifier", "test-stdout.txt"))
	if strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]) != "NET lo" {
		t.Fatalf("the verifier has a network: %q", out)
	}
	var r trialResult
	b, _ := os.ReadFile(filepath.Join(jobs, "ref", "ref__1", "result.json"))
	json.Unmarshal(b, &r)
	if !strings.HasPrefix(r.Container["final_tree"], "sha256:") || r.Container["runner"] != DockerRunnerVersion || r.Container["agent_image"] == "" {
		t.Fatalf("identities %v", r.Container)
	}
	// C1: the agent image holds neither the hidden tests nor the reference
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: r.Container["agent_image"], Cmd: []string{"sh", "-c", "test ! -e /tests && test ! -e /solution && ! grep -rqs HINTCANARY_HIDDEN_TEST / --exclude-dir=proc --exclude-dir=sys"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.remove(c.ID)
	if err := d.runToEnd(ctx, c.ID, time.Minute, "probe"); err != nil {
		t.Fatal(err)
	}
	ins, _ := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if ins.Container.State == nil || ins.Container.State.ExitCode != 0 {
		t.Fatal("the agent image contains hidden material")
	}
}

// A hung agent times out and fails the attempt; a final tree with an escaping symlink
// invalidates the attempt; neither is a pass.
func TestDockerRunnerRefusals(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	ctx := context.Background()
	jobs := t.TempDir()
	slow, err := d.Run(ctx, syntheticTask(t, "#!/bin/sh\nsleep 60\n", 3), RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "slow"})
	if err != nil || slow[0].Outcome() != "timeout" {
		t.Fatalf("timeout: %+v %v", slow, err)
	}
	evil, err := d.Run(ctx, syntheticTask(t, solveFixed+"ln -s /etc/passwd /app/evil\n", 120), RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "evil"})
	if err != nil || evil[0].Outcome() != "infrastructure_error" {
		t.Fatalf("escaping symlink: %+v %v", evil, err)
	}
	if _, err := d.Run(ctx, syntheticTask(t, solveFixed, 120), RunOptions{Agent: "opencode", JobsDir: jobs, JobName: "live"}); err == nil {
		t.Fatal("a live agent ran without the model gate")
	}
}
