package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"dot component":      {Name: "app/./x", Typeflag: tar.TypeReg, Mode: 0o644},
		"empty component":    {Name: "app//x", Typeflag: tar.TypeReg, Mode: 0o644},
		"empty symlink":      {Name: "app/e", Typeflag: tar.TypeSymlink},
	} {
		refused(t, dir, name, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, h)
	}
}

func refused(t *testing.T, dir, name string, entries ...*tar.Header) {
	t.Helper()
	p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".tar")
	if _, err := freezeTree(tarOf(t, entries...), p); err == nil {
		t.Errorf("%s: accepted", name)
	}
	if _, err := os.Stat(p); err == nil {
		t.Errorf("%s: a refused tree left an artifact", name)
	}
}

// C7BI-2/3: the link graph is resolved like the filesystem would (chains, "..", cycles) and
// never on the host; dotted file names are ordinary names.
func TestFreezeTreeLinkGraph(t *testing.T) {
	dir := t.TempDir()
	root := &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}
	sub := &tar.Header{Name: "app/sub/", Typeflag: tar.TypeDir, Mode: 0o755}
	file := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644} }
	sym := func(n, to string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeSymlink, Linkname: to} }
	hard := func(n, to string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeLink, Linkname: to} }
	for name, entries := range map[string][]*tar.Header{
		"dotted names":      {root, file("app/config..bak"), file("app/..hidden"), file("app/x.."), sym("app/l", "config..bak")},
		"chain inside":      {root, sub, file("app/sub/f"), sym("app/a", "sub"), sym("app/b", "a/f"), sym("app/c", "a/../sub/f")},
		"dot link":          {root, sym("app/a", "."), sym("app/b", "a/a/a")},
		"up and back":       {root, sub, file("app/f"), sym("app/sub/l", "../f")},
		"dangling inside":   {root, sym("app/d", "missing/x")},
		"hard link to file": {root, file("app/f"), hard("app/h", "app/f")},
	} {
		if _, err := freezeTree(tarOf(t, entries...), filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".tar")); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	for name, entries := range map[string][]*tar.Header{
		"dot then out":            {root, sym("app/a", "."), sym("app/evil", "a/../outside")},
		"dir link then out":       {root, sub, sym("app/a", "sub"), sym("app/evil", "a/../../outside")},
		"nested link out":         {root, sub, sym("app/sub/up", ".."), sym("app/evil", "sub/up/../x")},
		"chain to absolute":       {root, sym("app/a", "/etc"), sym("app/b", "a/passwd")},
		"cycle":                   {root, sym("app/x", "y"), sym("app/y", "x"), sym("app/z", "x/f")},
		"dangling then up":        {root, sym("app/d", "missing/../../x")},
		"entry below a symlink":   {root, sym("app/a", "."), file("app/a/f")},
		"entry below a file":      {root, file("app/f"), file("app/f/g")},
		"duplicate path":          {root, file("app/f"), file("app/f")},
		"hard link to a symlink":  {root, sym("app/s", "x"), hard("app/h", "app/s")},
		"hard link to later file": {root, hard("app/h", "app/f"), file("app/f")},
		"hard link to missing":    {root, hard("app/h", "app/none")},
		"hard link to directory":  {root, sub, hard("app/h", "app/sub")},
	} {
		refused(t, dir, name, entries...)
	}
}

// C7BI-1: a nonzero exit status of a phase is an error, whatever the phase printed before.
func TestRunToEndExitStatus(t *testing.T) {
	for _, tc := range []struct {
		wait string
		ok   bool
	}{{`{"StatusCode":0,"Error":null}`, true}, {`{"StatusCode":17,"Error":null}`, false}, {`{"StatusCode":0,"Error":{"Message":"oom"}}`, false}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/start"):
				w.WriteHeader(http.StatusNoContent)
			case strings.HasSuffix(r.URL.Path, "/wait"):
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.wait))
			default:
				t.Errorf("unexpected %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		c, err := client.New(client.WithHost(s.URL), client.WithAPIVersion("1.56"))
		if err != nil {
			t.Fatal(err)
		}
		d := Docker{cli: c}
		if err := d.runToEnd(context.Background(), "x", time.Second, "verifier"); (err == nil) != tc.ok {
			t.Errorf("wait %s: %v", tc.wait, err)
		}
		c.Close()
		s.Close()
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
	// a task with an agent allowlist (the former Harbor LAN format) is recognized and refused
	allow := strings.Replace(taskTOML("hint/x-v1", 1, "x", s), "[agent]\n", "[agent]\nnetwork_mode = \"allowlist\"\n", 1)
	os.WriteFile(filepath.Join(dir, "task.toml"), []byte(allow), 0o644)
	if l, _ := readLimits(dir); !l.lan {
		t.Fatal("an allowlist task is not recognized")
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
	if ft, _ := r.Container["final_tree"].(string); !strings.HasPrefix(ft, "sha256:") || r.Container["runner"] != DockerRunnerVersion || r.Container["agent_image"] == "" ||
		ora[0].ResultTrust != TrustSameProcess || ora[0].TrustOver([]string{"C1"}) != TrustSameProcess || ora[0].TrustOver([]string{"C1", "C2"}) != TrustUnknown {
		t.Fatalf("identities %v", r.Container)
	}
	// C1: the agent image holds neither the hidden tests nor the reference
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: r.Container["agent_image"].(string), Cmd: []string{"sh", "-c", "test ! -e /tests && test ! -e /solution && ! grep -rqs HINTCANARY_HIDDEN_TEST / --exclude-dir=proc --exclude-dir=sys"}},
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
// invalidates the attempt; a nonzero exit of the agent or the verifier fails it; none is a
// pass.
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
	// a verifier that prints passing checks and writes reward 1, then exits 17, is no pass
	forged := syntheticTask(t, solveFixed, 120)
	os.WriteFile(filepath.Join(forged, "tests", "test.sh"), []byte("#!/bin/sh\nmkdir -p /logs/verifier\necho 'CHECK C1 PASS'\necho 1 > /logs/verifier/reward.txt\nexit 17\n"), 0o755)
	late, err := d.Run(ctx, forged, RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "late"})
	if err != nil || late[0].Outcome() != "verifier_error" {
		t.Fatalf("verifier exit 17: %+v %v", late, err)
	}
	// an agent that exits nonzero fails the attempt
	bad, err := d.Run(ctx, syntheticTask(t, solveFixed+"exit 3\n", 120), RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "bad"})
	if err != nil || bad[0].Outcome() != "infrastructure_error" || bad[0].Exception != "NonZeroAgentExitCodeError" {
		t.Fatalf("agent exit 3: %+v %v", bad, err)
	}
	if _, err := d.Run(ctx, syntheticTask(t, solveFixed, 120), RunOptions{Agent: "opencode", JobsDir: jobs, JobName: "live"}); err == nil {
		t.Fatal("a live agent ran without the model gate")
	}
}

// The verifier's own network record reaches the job (the isolation check reads it); a record
// that is not one of the two exact values is dropped (missing evidence, never a pass).
func TestDockerVerifierNetworkRecord(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	for rec, want := range map[string]string{"HINT_VERIFIER_NET=blocked": "HINT_VERIFIER_NET=blocked\n", "HINT_VERIFIER_NET=maybe": ""} {
		task := syntheticTask(t, solveFixed, 120)
		os.WriteFile(filepath.Join(task, "tests", "test.sh"), []byte("#!/bin/sh\nmkdir -p /logs/verifier\necho CHECK C1 FAIL\necho 0 > /logs/verifier/reward.txt\necho "+rec+" > /logs/verifier/network.txt\n"), 0o755)
		jobs := t.TempDir()
		if _, err := d.Run(context.Background(), task, RunOptions{Agent: "nop", JobsDir: jobs, JobName: "net"}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(jobs, "net", "net__1", "verifier", "network.txt"))
		if string(b) != want {
			t.Fatalf("%s: record %q", rec, b)
		}
	}
}
