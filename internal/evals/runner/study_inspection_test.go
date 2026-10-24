package runner

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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

type studyFakeOptions struct {
	preexisting, ambiguous, ancestor, launcher, inventory string
	loader, mismatch                                      bool
	hook                                                  func(LaunchFacts) error
	failStart                                             bool // the agent's start request fails
}
type storedStudyFile struct {
	header tar.Header
	body   []byte
}
type studyFakeState struct {
	mu     sync.Mutex
	files  map[string]storedStudyFile
	starts int
	argv   []string
	dir    string
}

func studyArchive(files map[string]storedStudyFile) []byte {
	var keys []string
	for name := range files {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, name := range keys {
		file := files[name]
		h := file.header
		h.Name = name
		h.Size = int64(len(file.body))
		_ = w.WriteHeader(&h)
		if h.Typeflag == tar.TypeReg {
			_, _ = w.Write(file.body)
		}
	}
	_ = w.Close()
	return b.Bytes()
}
func studyFake(t *testing.T, o studyFakeOptions) (trialResult, *Counter, *studyFakeState) {
	t.Helper()
	d, s := fakeQ4Docker(t, false)
	live, _ := fakeModel(t)
	c := live.NewTrial(10)
	st := &studyFakeState{files: map[string]storedStudyFile{}, dir: t.TempDir()}
	isAgent := func(p string) bool {
		id := path.Base(path.Dir(p))
		s.mu.Lock()
		defer s.mu.Unlock()
		v := s.containers[id]
		if v == nil {
			return false
		}
		cmd, _ := v.cfg["Cmd"].([]any)
		return len(cmd) > 0 && cmd[0] == studyLauncher
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.Contains(p, "/images/") && strings.HasSuffix(p, "/json") {
			json.NewEncoder(w).Encode(map[string]any{"Id": fakeGateID, "Os": "linux", "Architecture": "arm64"})
			return
		}
		if strings.HasSuffix(p, "/archive") {
			source := r.URL.Query().Get("path")
			if strings.HasPrefix(source, "/logs/verifier/") {
				w.WriteHeader(404)
				return
			}
			if r.Method == "PUT" {
				if isAgent(p) {
					tr := tar.NewReader(r.Body)
					st.mu.Lock()
					for {
						h, e := tr.Next()
						if e == io.EOF {
							break
						}
						if e != nil {
							t.Error(e)
							break
						}
						body, _ := io.ReadAll(tr)
						st.files[h.Name] = storedStudyFile{*h, body}
					}
					st.mu.Unlock()
				}
				w.WriteHeader(200)
				return
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if o.ambiguous == source {
				w.WriteHeader(500)
				return
			}
			files := map[string]storedStudyFile{}
			root := path.Base(source)
			if source == "/" {
				root = "."
			}
			h := tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0755, Format: tar.FormatPAX}
			switch source {
			case "/", "/usr", "/usr/bin", "/opt", "/opt/hint", "/opt/hint/bin":
				if o.ancestor == "mode" && source == "/usr" {
					h.Mode = 0700
				}
				if o.ancestor == source {
					h.Typeflag = tar.TypeSymlink
					h.Linkname = "elsewhere"
				}
				files[h.Name] = storedStudyFile{header: h}
			case "/usr/bin/env":
				h.Name = "env"
				h.Typeflag = tar.TypeReg
				body := []byte("recorded executable bytes; never executed")
				if o.launcher == "owner" {
					h.Uid = 65534
				}
				if o.launcher == "private" {
					h.Mode = 0700
				}
				if o.launcher == "writable" {
					h.Mode = 0777
				}
				if o.launcher == "link" {
					h.Typeflag = tar.TypeSymlink
					h.Linkname = "other"
					body = nil
				}
				if o.launcher == "directory" {
					h.Typeflag = tar.TypeDir
					body = nil
				}
				if o.launcher == "empty" {
					body = nil
				}
				files[h.Name] = storedStudyFile{h, body}
			case studyRoot:
				found := false
				for name, file := range st.files {
					if strings.TrimSuffix(name, "/") == "opt/hint/study" || strings.HasPrefix(name, "opt/hint/study/") {
						files[strings.TrimPrefix(name, "opt/hint/")] = file
						found = true
					}
				}
				if !found {
					if o.preexisting == source {
						files["study/"] = storedStudyFile{header: tar.Header{Typeflag: tar.TypeDir, Mode: 0555}}
					} else {
						w.WriteHeader(404)
						return
					}
				}
				if found && o.inventory != "" && o.inventory != "cut" {
					if o.inventory == "too_many" {
						for i := 0; i < studyInspectionEntries; i++ {
							files[fmt.Sprintf("study/extra-%d", i)] = storedStudyFile{header: tar.Header{Typeflag: tar.TypeDir, Mode: 0555}}
						}
					} else if o.inventory == "too_large" {
						files["study/large"] = storedStudyFile{header: tar.Header{Typeflag: tar.TypeReg, Mode: 0600}, body: bytes.Repeat([]byte("x"), int(studyInspectionBytes+1))}
					} else if o.inventory == "config" {
						file := files["study/config/opencode.json"]
						file.body = []byte(`{"permissions":[{"action":"subagent","resource":"*","effect":"allow"}]}`)
						files["study/config/opencode.json"] = file
					} else {
						files["study/"+o.inventory] = storedStudyFile{header: tar.Header{Typeflag: tar.TypeReg, Mode: 0600, Uid: 65534, Gid: 65534}, body: []byte("unexpected")}
					}
				}
			case "/opt/hint/bin/opencode":
				if o.preexisting != source {
					w.WriteHeader(404)
					return
				}
				files["opencode"] = storedStudyFile{header: tar.Header{Typeflag: tar.TypeReg, Mode: 0755}, body: []byte("old")}
			case "/app":
				for name, file := range st.files {
					if strings.TrimSuffix(name, "/") == "app" || strings.HasPrefix(name, "app/") {
						files[name] = file
					}
				}
			default:
				t.Errorf("unexpected archive source %q", source)
				w.WriteHeader(500)
				return
			}
			b := studyArchive(files)
			if source == studyRoot && o.inventory == "cut" && len(st.files) > 0 {
				b = b[:512]
			}
			stat, _ := json.Marshal(map[string]any{"name": root, "size": 0, "mode": uint32(0755), "mtime": "2026-10-24T00:00:00Z", "linkTarget": ""})
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			w.Write(b)
			return
		}
		if strings.HasSuffix(p, "/json") && strings.Contains(p, "/containers/") {
			rec := httptest.NewRecorder()
			s.serve(rec, r)
			var v map[string]any
			json.Unmarshal(rec.Body.Bytes(), &v)
			v["State"] = map[string]any{"Status": "created", "Running": false, "Pid": 0}
			cfg, _ := v["Config"].(map[string]any)
			if cmd, ok := cfg["Cmd"].([]any); ok && len(cmd) > 0 && cmd[0] == studyLauncher {
				env, _ := cfg["Env"].([]any)
				env = append(env, "OPENCODE_CONFIG=/image-override")
				if o.loader {
					env = append(env, "LD_PRELOAD=/unreviewed")
				}
				cfg["Env"] = env
				st.mu.Lock()
				st.argv = nil
				for _, a := range cmd {
					st.argv = append(st.argv, a.(string))
				}
				st.mu.Unlock()
			}
			json.NewEncoder(w).Encode(v)
			return
		}
		if strings.HasSuffix(p, "/logs") {
			id := path.Base(path.Dir(p))
			s.mu.Lock()
			v := s.containers[id]
			probe := v != nil && fmt.Sprint(v.cfg["Cmd"]) == "[ldd --version]"
			s.mu.Unlock()
			if probe {
				body := []byte("ldd (GNU libc) 2.36\n")
				var header [8]byte
				header[0] = 1
				binary.BigEndian.PutUint32(header[4:], uint32(len(body)))
				w.Write(header[:])
				w.Write(body)
				return
			}
		}
		if strings.HasSuffix(p, "/start") && isAgent(p) {
			st.mu.Lock()
			st.starts++
			st.mu.Unlock()
			if o.failStart {
				http.Error(w, `{"message":"synthetic start failure"}`, http.StatusInternalServerError)
				return
			}
		}
		if strings.HasSuffix(p, "/wait") && isAgent(p) {
			req := httptest.NewRequest("POST", "http://proxy/t/"+c.token+"/chat/completions", strings.NewReader(`{"model":"m"}`))
			live.srv.Handler.ServeHTTP(httptest.NewRecorder(), req)
		}
		s.serve(w, r)
	}))
	t.Cleanup(api.Close)
	cli, err := client.New(client.WithHost(api.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	d.cli = cli
	key, err := d.probeKey(context.Background(), fakeGateID)
	if err != nil {
		t.Fatal(err)
	}
	d.probes = map[string]GateProbe{key: {OK: true, Key: key, Image: fakeGateID}}
	tgz := makeTgz(t, fileE("package/bin/opencode", "synthetic binary; not executed"))
	old := OpenCodePins[DockerOpenCodeVersion]
	OpenCodePins[DockerOpenCodeVersion] = map[string]string{"linux-arm64": sha512Hex(tgz)}
	t.Cleanup(func() { OpenCodePins[DockerOpenCodeVersion] = old })
	file := filepath.Join(t.TempDir(), "binary.tgz")
	os.WriteFile(file, tgz, 0600)
	planned, err := DockerLaunchDigestWithPolicy(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	if o.mismatch {
		planned = "sha256:" + strings.Repeat("0", 64)
	}
	task := t.TempDir()
	os.MkdirAll(filepath.Join(task, "environment", "repo"), 0755)
	os.WriteFile(filepath.Join(task, "instruction.md"), []byte("do"), 0600)
	d.trial(context.Background(), task, st.dir, "t__1", RunOptions{Agent: "opencode", Attempts: 1, Live: live, Counter: c, OpenCode: &OpenCodeSource{Version: DockerOpenCodeVersion, TgzFile: file, CacheDir: t.TempDir()}, ExpectTarget: "linux-arm64", StudyLaunchPolicy: StudyLaunchSingleRootV1, CapturePrelaunchTree: true, ExpectLaunchDigest: planned, BeforeAgentStart: o.hook}, taskLimits{agent: time.Second, verifier: time.Second, cpus: 1, memoryMB: 128}, images{agent: fakeGateID, verifier: fakeGateID}, nil, nil)
	var result trialResult
	raw, err := os.ReadFile(filepath.Join(st.dir, "result.json"))
	if err != nil || json.Unmarshal(raw, &result) != nil {
		t.Fatal(err)
	}
	return result, c, st
}
func studyReceipt(t *testing.T, r trialResult) StudyLaunchReceipt {
	t.Helper()
	var receipt StudyLaunchReceipt
	b, _ := json.Marshal(r.Container["study_launch"])
	if json.Unmarshal(b, &receipt) != nil {
		t.Fatal("missing study receipt")
	}
	return receipt
}

func TestStudyFakeEnginePreparesControlsWithoutGrantingEligibility(t *testing.T) {
	r, c, s := studyFake(t, studyFakeOptions{})
	receipt := studyReceipt(t, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if receipt.State != "prepared" || receipt.Policy != StudyLaunchSingleRootV1 || receipt.BinaryValidation != "not_run" || receipt.Eligibility != "requires_real_binary_validation" || !prelaunchDigestPattern.MatchString(receipt.InventoryDigest) || !prelaunchDigestPattern.MatchString(receipt.LauncherDigest) || receipt.ImageID != fakeGateID || s.starts != 1 || c.Requests() != 1 {
		t.Fatalf("receipt %+v starts %d requests %d exception %+v", receipt, s.starts, c.Requests(), r.ExceptionInfo)
	}
	if len(s.argv) < 2 || s.argv[0] != studyLauncher || s.argv[1] != "-i" || strings.Contains(strings.Join(s.argv, "\n"), "/image-override") {
		t.Fatal("argv inherited image config")
	}
	var capture PrelaunchTreeCapture
	b, _ := json.Marshal(r.Container["prelaunch_tree"])
	json.Unmarshal(b, &capture)
	if receipt.ExecutionDigest != capture.ExecutionDigest || receipt.StartedMs == nil || receipt.FinishedMs == nil || *receipt.FinishedMs > capture.StartedMs || *receipt.StartedMs > *receipt.FinishedMs {
		t.Fatal("receipt/capture binding mismatch")
	}
}
func TestStudyFakeEngineRefusesBeforeAgentStart(t *testing.T) {
	for name, o := range map[string]studyFakeOptions{"planned mismatch": {mismatch: true}, "preexisting state": {preexisting: studyRoot}, "preexisting binary": {preexisting: "/opt/hint/bin/opencode"}, "ambiguous absence": {ambiguous: studyRoot}, "ancestor link": {ancestor: "/usr"}, "ancestor inaccessible": {ancestor: "mode"}, "loader injection": {loader: true}, "launcher link": {launcher: "link"}, "launcher owner": {launcher: "owner"}, "launcher writable": {launcher: "writable"}, "launcher empty": {launcher: "empty"}, "launcher directory": {launcher: "directory"}, "launcher inaccessible": {launcher: "private"}, "config override": {inventory: "config"}, "credentials": {inventory: "runtime/data/auth.json"}, "plugin": {inventory: "config/plugin/custom.ts"}, "command": {inventory: "config/commands/custom.md"}, "saved state": {inventory: "runtime/state/permissions.json"}, "truncated inventory": {inventory: "cut"}, "too many entries": {inventory: "too_many"}, "oversized inventory": {inventory: "too_large"}} {
		t.Run(name, func(t *testing.T) {
			r, c, s := studyFake(t, o)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.starts != 0 || c.Requests() != 0 || r.ExceptionInfo == nil {
				t.Fatalf("started %d requests %d exception %+v", s.starts, c.Requests(), r.ExceptionInfo)
			}
			if receipt := studyReceipt(t, r); receipt.State == "prepared" {
				t.Fatal("failed controls prepared")
			}
		})
	}
}
