package runner

// Synthetic engine only. No Docker/OpenCode/model, store, key or external endpoint.
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

type referenceFake struct {
	running map[string]bool
	stops   int
	waiting chan struct{}
	once    sync.Once
	mu      sync.Mutex
	cfg     map[string]struct {
		Image      string
		Cmd        []string
		OpenStdin  bool
		HostConfig map[string]any
	}
	removed         map[string]bool
	creates, builds int
	caseStarts      int
	beforeCreate    func(bool)
	copied          [][]byte
	mode            string
	archive         []byte
}

func referenceEngine(t *testing.T, mode string) (*Docker, *referenceFake) {
	t.Helper()
	var source bytes.Buffer
	tw := tar.NewWriter(&source)
	_ = tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755})
	_ = tw.WriteHeader(&tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644, Size: 1, ModTime: time.Unix(123, 0)})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	f := &referenceFake{cfg: map[string]struct {
		Image      string
		Cmd        []string
		OpenStdin  bool
		HostConfig map[string]any
	}{}, removed: map[string]bool{}, mode: mode, archive: source.Bytes(), running: map[string]bool{}, waiting: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "/info") {
			fmt.Fprint(w, `{"ID":"synthetic-engine","OSType":"linux","Architecture":"aarch64","ServerVersion":"synthetic-1"}`)
			return
		}
		if strings.HasSuffix(path, "/build") {
			_, _ = io.Copy(io.Discard, r.Body)
			f.mu.Lock()
			f.builds++
			f.mu.Unlock()
			if mode == "build_malformed" {
				fmt.Fprintln(w, "bad")
				return
			}
			fmt.Fprintf(w, "{\"aux\":{\"ID\":%q}}\n", fakeImage)
			return
		}
		if strings.Contains(path, "/images/") && strings.HasSuffix(path, "/json") {
			fmt.Fprintf(w, `{"Id":%q,"Os":"linux","Architecture":"arm64"}`, fakeImage)
			return
		}
		if strings.HasSuffix(path, "/containers/create") {
			var c struct {
				Image      string
				Cmd        []string
				OpenStdin  bool
				HostConfig map[string]any
			}
			if json.NewDecoder(r.Body).Decode(&c) != nil {
				t.Error("bad create")
			}
			if f.beforeCreate != nil {
				f.beforeCreate(c.OpenStdin)
			}
			f.mu.Lock()
			f.creates++
			id := fmt.Sprintf("r%d", f.creates)
			f.cfg[id] = c
			f.mu.Unlock()
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"Id":%q}`, id)
			return
		}
		parts := strings.Split(path, "/")
		id := ""
		for i, p := range parts {
			if p == "containers" && i+1 < len(parts) {
				id = parts[i+1]
			}
		}
		f.mu.Lock()
		c := f.cfg[id]
		removed := f.removed[id]
		running := f.running[id]
		f.mu.Unlock()
		switch {
		case strings.HasSuffix(path, "/json"):
			if mode == "inspect_late" && !removed {
				<-r.Context().Done()
				return
			}
			if removed {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"not found"}`)
				return
			}
			if mode == "inspect_fail" {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"synthetic"}`)
				return
			}
			host := c.HostConfig
			if mode == "network" {
				host = map[string]any{"NetworkMode": "bridge", "SecurityOpt": []string{"no-new-privileges:true"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Image": c.Image, "Config": map[string]any{"Cmd": c.Cmd}, "HostConfig": host, "State": map[string]any{"Running": running, "ExitCode": 0}, "NetworkSettings": map[string]any{"Networks": map[string]any{"none": map[string]any{}}}, "Mounts": []any{}})
		case r.Method == "DELETE":
			if mode == "remove_late" {
				<-r.Context().Done()
				return
			}
			if mode == "remove_fail" {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"synthetic"}`)
				return
			}
			f.mu.Lock()
			f.removed[id] = true
			f.mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/start"):
			f.mu.Lock()
			f.running[id] = true
			if c.OpenStdin {
				f.caseStarts++
			}
			f.mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/stop"), strings.HasSuffix(path, "/kill"):
			f.mu.Lock()
			f.stops++
			f.mu.Unlock()
			if mode == "stop_late" {
				<-r.Context().Done()
				return
			}
			f.mu.Lock()
			f.running[id] = false
			f.mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/wait"):
			if mode == "wait_late" || mode == "cancel_wait" || mode == "stop_late" {
				f.once.Do(func() { close(f.waiting) })
				<-r.Context().Done()
				return
			}
			f.mu.Lock()
			f.running[id] = false
			f.mu.Unlock()
			status := 0
			if mode == "exit" {
				status = 3
			}
			fmt.Fprintf(w, `{"StatusCode":%d,"Error":null}`, status)
		case strings.HasSuffix(path, "/logs"):
			if mode == "logs_cut" {
				_, _ = w.Write([]byte{1, 0, 0})
				return
			}
			w.WriteHeader(200)
		case strings.HasSuffix(path, "/archive"):
			if r.Method == "PUT" {
				b, _ := io.ReadAll(r.Body)
				if c.OpenStdin {
					f.mu.Lock()
					f.copied = append(f.copied, b)
					f.mu.Unlock()
				}
				w.WriteHeader(200)
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"app","mode":2147484141}`)))
			b := f.archive
			if mode == "tar_cut" {
				b = b[:len(b)-1024]
			}
			_, _ = w.Write(b)
		case strings.HasSuffix(path, "/attach"):
			conn, b, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = b.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = b.Flush()
			_, _ = io.Copy(io.Discard, conn)
			if mode == "output_hang" {
				time.Sleep(1500 * time.Millisecond)
			}
			out := []byte("OK\n")
			if mode == "disagree" && id == "r3" {
				out = []byte("DIFFERENT\n")
			}
			h := make([]byte, 8)
			h[0] = 1
			binary.BigEndian.PutUint32(h[4:], uint32(len(out)))
			_, _ = conn.Write(h)
			if mode == "output_cut" {
				out = out[:1]
			}
			_, _ = conn.Write(out)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithScheme("http"), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Docker{cli: cli}, f
}
func referenceTaskOptions(t *testing.T) (string, ReferencePrepareOptions) {
	t.Helper()
	dir := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	// Exact runner limits, not model limits. ioTaskDir default fixture uses other settings.
	text, e := os.ReadFile(filepath.Join(dir, "task.toml"))
	if e != nil {
		t.Fatal(e)
	}
	s := string(text)
	s = strings.ReplaceAll(s, "memory_mb = 64", "memory_mb = 2048")
	s = strings.ReplaceAll(s, "cpus = 1", "cpus = 2")
	_ = os.WriteFile(filepath.Join(dir, "task.toml"), []byte(s), 0600)
	iot, e := readIOTask(dir)
	if e != nil {
		t.Fatal(e)
	}
	ref, e := tarDir(filepath.Join(dir, "tests", "io", "reference"), "solution")
	if e != nil {
		t.Fatal(e)
	}
	env, e := tarDir(filepath.Join(dir, "environment"), "")
	if e != nil {
		t.Fatal(e)
	}
	return dir, ReferencePrepareOptions{OutputDir: filepath.Join(t.TempDir(), "new"), Expected: ReferenceInputIdentity{iot.specDigest, iot.inputsDigest, referenceDigest(ref), referenceDigest(env)}, policy: &referencePreparationPolicy{identity: "synthetic-private-test-policy-v1", operation: 30 * time.Second, resolve: time.Second, commit: time.Second, build: time.Second, apply: time.Second, overhead: time.Millisecond, cleanup: 300 * time.Millisecond, archiveCap: 1 << 20, totalCap: 4 << 20}}
}
func TestReferencePrepareDefaultDisabled(t *testing.T) {
	a, e := (&Docker{}).PrepareReference(context.Background(), "not-read", ReferencePrepareOptions{})
	if e == nil || a.State != "unknown" || len(a.FailureCodes) != 1 || a.FailureCodes[0] != "policy_disabled" {
		t.Fatal(a, e)
	}
}
func TestReferencePrepareActualFakePath(t *testing.T) {
	d, f := referenceEngine(t, "")
	task, o := referenceTaskOptions(t)
	a, e := d.PrepareReference(context.Background(), task, o)
	if e != nil {
		t.Fatalf("%+v %v", a, e)
	}
	if a.State != "prepared_reference_observed" || a.FinalTAR.Domain != referenceTARDomain || a.OracleOutputsDigest == "" || len(a.Cases) != 4 {
		t.Fatalf("%+v", a)
	}
	b, e := os.ReadFile(filepath.Join(o.OutputDir, "final.tar"))
	if e != nil || referenceDigest(b) != a.FinalTAR.Digest {
		t.Fatal(e)
	}
	// Golden expected B1 hash independently spells the existing byte recipe.
	if a.OracleOutputsDigest != referenceDigest([]byte("lower\x000\x003\x00OK\nmixed\x000\x003\x00OK\n")) {
		t.Fatalf("output recipe changed: %+v", a)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 5 || f.builds != 1 || len(f.copied) != 4 || !bytes.Equal(f.copied[0], f.copied[1]) || len(f.removed) != 5 {
		t.Fatalf("creates=%d builds=%d copied=%d removed=%d", f.creates, f.builds, len(f.copied), len(f.removed))
	}
	for _, p := range a.Phases[1:] {
		if !p.Started || !p.WaitObserved || !p.Removed || !p.Exited || !p.NetworkNone || p.ContainerRef == "" || p.CommandDigest == "" {
			t.Fatalf("incomplete %+v", p)
		}
	}
	for _, c := range f.cfg {
		if c.Image != fakeImage {
			t.Fatal("image changed")
		}
		if !c.OpenStdin && !strings.Contains(strings.Join(c.Cmd, " "), "/solution/solve.sh") {
			t.Fatal("non reference execution")
		}
	}
}
func TestReferencePrepareMutations(t *testing.T) {
	for _, mode := range []string{"build_malformed", "inspect_fail", "network", "remove_fail", "exit", "logs_cut", "tar_cut", "output_cut", "output_hang", "disagree"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := referenceEngine(t, mode)
			task, o := referenceTaskOptions(t)
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" || len(a.FailureCodes) == 0 {
				t.Fatalf("accepted %+v %v", a, e)
			}
		})
	}
}
func TestReferencePrepareInputAndScheduleBeforeEngine(t *testing.T) {
	for _, mode := range []string{"input", "capacity", "cancelled", "output_exists"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, "")
			task, o := referenceTaskOptions(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "input":
				o.Expected.SpecDigest = "different"
			case "capacity":
				o.policy.operation = time.Second
			case "cancelled":
				cancel()
			case "output_exists":
				_ = os.Mkdir(o.OutputDir, 0700)
			}
			a, e := d.PrepareReference(ctx, task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal(a, e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.builds != 0 || f.creates != 0 {
				t.Fatal("engine used")
			}
		})
	}
}
func TestReferencePrepareBoundedJoin(t *testing.T) {
	p := &referencePhase{joinEnd: time.Now().Add(10 * time.Millisecond)}
	if e := p.join(make(chan error)); e == nil || p.err == nil {
		t.Fatal("unjoined stream passed")
	}
	ch := make(chan error, 1)
	ch <- nil
	p = &referencePhase{joinEnd: time.Now().Add(time.Second)}
	if e := p.join(ch); e != nil {
		t.Fatal(e)
	}
}
func TestReferencePrepareArchiveCapAndHeaderDomain(t *testing.T) {
	d, f := referenceEngine(t, "")
	task, o := referenceTaskOptions(t)
	o.policy.archiveCap = 512
	a, e := d.PrepareReference(context.Background(), task, o)
	if e == nil || a.State != "unknown" {
		t.Fatal(a, e)
	}
	if _, e = os.Stat(filepath.Join(o.OutputDir, "final.tar")); !os.IsNotExist(e) {
		t.Fatal("partial artifact retained")
	}
	var other bytes.Buffer
	tw := tar.NewWriter(&other)
	_ = tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(456, 0)})
	_ = tw.WriteHeader(&tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644, Size: 1, ModTime: time.Unix(123, 0)})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	x := filepath.Join(t.TempDir(), "x")
	y := filepath.Join(t.TempDir(), "y")
	dx, e := freezeTree(bytes.NewReader(f.archive), x)
	if e != nil {
		t.Fatal(e)
	}
	dy, e := freezeTree(bytes.NewReader(other.Bytes()), y)
	if e != nil || dx == dy {
		t.Fatal("metadata erased", e)
	}
}

func TestReferencePrepareLateRPCsCannotBecomeEvidence(t *testing.T) {
	for _, mode := range []string{"wait_late", "inspect_late", "remove_late", "stop_late"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, mode)
			task, o := referenceTaskOptions(t)
			o.policy.build = 30 * time.Millisecond
			o.policy.apply = 30 * time.Millisecond
			o.policy.cleanup = 60 * time.Millisecond
			start := time.Now()
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal("late execution accepted", a, e)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("cleanup renewed budget")
			}
			if mode == "inspect_late" {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.creates != 1 || len(f.removed) != 1 {
					t.Fatal("lost created identity", f.creates, len(f.removed))
				}
			}
		})
	}
}
func TestReferencePrepareJoinRejectsLateReadyWorker(t *testing.T) {
	ch := make(chan error, 1)
	ch <- nil
	p := &referencePhase{joinEnd: time.Now().Add(-time.Millisecond)}
	if p.join(ch) == nil || p.err == nil {
		t.Fatal("late worker became evidence")
	}
}

func TestReferencePrepareClosedChannelIsMissingEvidence(t *testing.T) {
	ch := make(chan error)
	close(ch)
	p := &referencePhase{joinEnd: time.Now().Add(time.Second)}
	if p.join(ch) == nil || p.err == nil {
		t.Fatal("missing worker result accepted")
	}
}

func TestReferencePrepareSourceAndRetainedArtifactCannotChange(t *testing.T) {
	for _, mode := range []string{"reference", "retained_tar"} {
		t.Run(mode, func(t *testing.T) {
			d, f := referenceEngine(t, "")
			task, o := referenceTaskOptions(t)
			f.beforeCreate = func(isCase bool) {
				if mode == "reference" && !isCase {
					if e := os.WriteFile(filepath.Join(task, "tests", "io", "reference", "solve.sh"), []byte("changed\n"), 0755); e != nil {
						t.Error(e)
					}
				}
				if mode == "retained_tar" && isCase {
					p := filepath.Join(o.OutputDir, "final.tar")
					file, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
					if e != nil {
						t.Error(e)
						return
					}
					_, _ = file.Write(make([]byte, 512))
					_ = file.Close()
				}
			}
			a, e := d.PrepareReference(context.Background(), task, o)
			if e == nil || a.State != "unknown" {
				t.Fatal("source substitution accepted", a, e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.caseStarts != 0 {
				t.Fatal("changed artifact executed", f.caseStarts)
			}
		})
	}
}

func TestReferencePrepareCancellationRetainsCleanupBudget(t *testing.T) {
	d, f := referenceEngine(t, "cancel_wait")
	task, o := referenceTaskOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.waiting:
			cancel()
		case <-ctx.Done():
		}
	}()
	a, e := d.PrepareReference(ctx, task, o)
	if e == nil || a.State != "unknown" {
		t.Fatal("cancelled work became success", a, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stops != 1 || len(f.removed) != 1 {
		t.Fatal("cancelled work lost original cleanup", f.stops, len(f.removed))
	}
}

func TestReferencePrepareOrdinaryIOPhaseGolden(t *testing.T) {
	d, _ := referenceEngine(t, "")
	task := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	iot, e := readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	if e = os.Mkdir(filepath.Join(dir, "verifier"), 0700); e != nil {
		t.Fatal(e)
	}
	reward, prov, e := d.ioPhase(context.Background(), task, iot, taskLimits{agent: time.Second, verifier: 20 * time.Second, cpus: 2, memoryMB: 2048}, fakeImage, fakeTree(t), dir)
	if e != nil || reward != 1 {
		t.Fatal(reward, e)
	}
	golden := "TEST B1 cases/lower::output PASS\nTEST B1 cases/mixed::output PASS\nCHECK B1 PASS\n"
	b, e := os.ReadFile(filepath.Join(dir, "verifier", "test-stdout.txt"))
	if e != nil || string(b) != golden {
		t.Fatalf("ordinary output changed %q %v", b, e)
	}
	expected := map[string]any{"interface": IOInterface, "spec": iot.specDigest, "inputs": iot.inputsDigest, "oracle_runs": OracleRuns, "verdict_scope": "equivalence_to_reference_on_declared_cases", "reference_tree": "sha256:897cf7e72a6771a5d5650ff4f770128f8fba6cd8210e889319f0bca073a755c6", "oracle_outputs": referenceDigest([]byte("lower\x000\x003\x00OK\nmixed\x000\x003\x00OK\n")), "candidate_outputs": referenceDigest([]byte("lower\x000\x00false\x00false\x003\x00OK\nmixed\x000\x00false\x00false\x003\x00OK\n")), "network": "engine_inspect:blocked"}
	if referenceJSON(prov) != referenceJSON(expected) {
		t.Fatalf("ordinary provenance changed %+v", prov)
	}
}
