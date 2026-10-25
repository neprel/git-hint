package referenceengine

// The shared synthetic SDK handler runs only under owned test listeners. It does not execute commands.
import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/moby/moby/client"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const Image = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

type State struct {
	InfoID  string
	Running map[string]bool
	Stops   int
	Waiting chan struct{}
	Once    sync.Once
	Mu      sync.Mutex
	Cfg     map[string]struct {
		Image      string
		Cmd        []string
		OpenStdin  bool
		HostConfig map[string]any
	}
	Removed         map[string]bool
	Creates, Builds int
	CaseStarts      int
	BeforeCreate    func(bool)
	Copied          [][]byte
	Mode            string
	Archive         []byte
}

// The same fake handler supports TCP legacy controls and explicit Unix policy tests.
func Start(t *testing.T, Mode string, listener net.Listener) (*client.Client, *State) {
	t.Helper()
	var source bytes.Buffer
	tw := tar.NewWriter(&source)
	_ = tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755})
	_ = tw.WriteHeader(&tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644, Size: 1, ModTime: time.Unix(123, 0)})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	f := &State{Cfg: map[string]struct {
		Image      string
		Cmd        []string
		OpenStdin  bool
		HostConfig map[string]any
	}{}, Removed: map[string]bool{}, Mode: Mode, Archive: source.Bytes(), Running: map[string]bool{}, Waiting: make(chan struct{}), InfoID: "synthetic-engine"}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		f.Mu.Lock()
		Mode := f.Mode
		f.Mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "/version") {
			fmt.Fprint(w, `{"Version":"synthetic-1","ApiVersion":"1.56","MinAPIVersion":"1.24"}`)
			return
		}
		if strings.HasSuffix(path, "/info") {
			if Mode == "info_slow" {
				select {
				case <-time.After(600 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			f.Mu.Lock()
			id := f.InfoID
			f.Mu.Unlock()
			fmt.Fprintf(w, `{"ID":%q,"OSType":"linux","Architecture":"aarch64","ServerVersion":"synthetic-1"}`, id)
			return
		}
		if strings.HasSuffix(path, "/build") {
			_, _ = io.Copy(io.Discard, r.Body)
			f.Mu.Lock()
			f.Builds++
			f.Mu.Unlock()
			if Mode == "build_malformed" {
				fmt.Fprintln(w, "bad")
				return
			}
			fmt.Fprintf(w, "{\"aux\":{\"ID\":%q}}\n", Image)
			return
		}
		if strings.Contains(path, "/images/") && strings.HasSuffix(path, "/json") {
			fmt.Fprintf(w, `{"Id":%q,"Os":"linux","Architecture":"arm64"}`, Image)
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
			if f.BeforeCreate != nil {
				f.BeforeCreate(c.OpenStdin)
			}
			f.Mu.Lock()
			f.Creates++
			id := fmt.Sprintf("r%d", f.Creates)
			f.Cfg[id] = c
			f.Mu.Unlock()
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
		f.Mu.Lock()
		c := f.Cfg[id]
		Removed := f.Removed[id]
		Running := f.Running[id]
		caseStarts := f.CaseStarts
		f.Mu.Unlock()
		switch {
		case strings.HasSuffix(path, "/json"):
			if Mode == "inspect_late" && !Removed {
				<-r.Context().Done()
				return
			}
			if Removed {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"not found"}`)
				return
			}
			if Mode == "inspect_fail" {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"synthetic"}`)
				return
			}
			host := c.HostConfig
			if Mode == "network" {
				host = map[string]any{"NetworkMode": "bridge", "SecurityOpt": []string{"no-new-privileges:true"}}
			}
			exitStatus := 0
			if Mode == "exit" || Mode == "inspect_exit" {
				exitStatus = 3
			}
			if Mode == "running_after_wait" && caseStarts >= 3 {
				Running = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Image": c.Image, "Config": map[string]any{"Cmd": c.Cmd}, "HostConfig": host, "State": map[string]any{"Running": Running, "ExitCode": exitStatus}, "NetworkSettings": map[string]any{"Networks": map[string]any{"none": map[string]any{}}}, "Mounts": []any{}})
		case r.Method == "DELETE":
			if Mode == "remove_late" {
				<-r.Context().Done()
				return
			}
			if Mode == "remove_fail" {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"synthetic"}`)
				return
			}
			f.Mu.Lock()
			f.Removed[id] = true
			f.Mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/start"):
			f.Mu.Lock()
			f.Running[id] = true
			if c.OpenStdin {
				f.CaseStarts++
			}
			f.Mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/stop"), strings.HasSuffix(path, "/kill"):
			f.Mu.Lock()
			f.Stops++
			f.Mu.Unlock()
			if Mode == "stop_late" {
				<-r.Context().Done()
				return
			}
			f.Mu.Lock()
			f.Running[id] = false
			f.Mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/wait"):
			if Mode == "wait_late" || Mode == "cancel_wait" || Mode == "stop_late" {
				f.Once.Do(func() { close(f.Waiting) })
				<-r.Context().Done()
				return
			}
			f.Mu.Lock()
			f.Running[id] = false
			f.Mu.Unlock()
			status := 0
			if Mode == "exit" || Mode == "wait_exit" {
				status = 3
			}
			fmt.Fprintf(w, `{"StatusCode":%d,"Error":null}`, status)
		case strings.HasSuffix(path, "/logs"):
			if Mode == "logs_cut" {
				_, _ = w.Write([]byte{1, 0, 0})
				return
			}
			w.WriteHeader(200)
		case strings.HasSuffix(path, "/archive"):
			if r.Method == "PUT" {
				b, _ := io.ReadAll(r.Body)
				if c.OpenStdin {
					f.Mu.Lock()
					f.Copied = append(f.Copied, b)
					f.Mu.Unlock()
				}
				w.WriteHeader(200)
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"app","mode":2147484141}`)))
			b := f.Archive
			if Mode == "tar_cut" {
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
			if Mode == "output_hang" {
				time.Sleep(1500 * time.Millisecond)
			}
			out := []byte("OK\n")
			if Mode == "disagree" && id == "r3" {
				out = []byte("DIFFERENT\n")
			}
			h := make([]byte, 8)
			h[0] = 1
			binary.BigEndian.PutUint32(h[4:], uint32(len(out)))
			_, _ = conn.Write(h)
			if Mode == "output_cut" {
				out = out[:1]
			}
			_, _ = conn.Write(out)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	if listener != nil {
		srv.Listener = listener
	}
	srv.Start()
	t.Cleanup(srv.Close)
	var opts []client.Opt
	if listener != nil {
		opts = []client.Opt{client.WithHost("unix://" + listener.Addr().String()), client.WithAPIVersion("1.56")}
	} else {
		opts = []client.Opt{client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")), client.WithScheme("http"), client.WithAPIVersion("1.56")}
	}
	cli, err := client.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, f
}
