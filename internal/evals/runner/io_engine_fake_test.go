package runner

// Synthetic engine tests from the C7BJ-5..7 review (a fake Docker API; no engine, no model).
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/moby/moby/client"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeEngine(t *testing.T, broken bool, status int64) (*Docker, *string) {
	t.Helper()
	var mu sync.Mutex
	states := map[string]bool{}
	n := 0
	user := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasSuffix(p, "/containers/create") {
			var cfg struct {
				OpenStdin bool
				User      string
			}
			json.NewDecoder(r.Body).Decode(&cfg)
			mu.Lock()
			n++
			id := fmt.Sprint("c", n)
			states[id] = cfg.OpenStdin
			if cfg.OpenStdin {
				user = cfg.User
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"Id":%q}`, id)
			return
		}
		parts := strings.Split(p, "/")
		id := ""
		for i, x := range parts {
			if x == "containers" && i+1 < len(parts) {
				id = parts[i+1]
			}
		}
		if strings.HasSuffix(p, "/archive") {
			if r.Method == "PUT" {
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(200)
				return
			}
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			tw.WriteHeader(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755})
			tw.WriteHeader(&tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
			tw.Write([]byte("x"))
			tw.Close()
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"app","size":0,"mode":2147484141}`)))
			w.Write(b.Bytes())
			return
		}
		if strings.HasSuffix(p, "/attach") {
			c, b, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			defer c.Close()
			b.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			payload := []byte("OK\n")
			hdr := make([]byte, 8)
			hdr[0] = 1
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
			b.Write(hdr)
			b.Write(payload)
			if broken {
				binary.BigEndian.PutUint32(hdr[4:], 100)
				b.Write(hdr)
				b.WriteString("short")
			}
			b.Flush()
			return
		}
		if strings.HasSuffix(p, "/json") && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":%q,"HostConfig":{"NetworkMode":"none"},"NetworkSettings":{"Networks":{"none":{}}}}`, id)
			return
		}
		if strings.HasSuffix(p, "/logs") {
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			w.WriteHeader(200)
			return
		}
		if strings.HasSuffix(p, "/start") {
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(p, "/wait") {
			mu.Lock()
			isCase := states[id]
			mu.Unlock()
			st := int64(0)
			if isCase {
				st = status
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"StatusCode":%d,"Error":null}`, st)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, p)
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	c, e := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithScheme("http"), client.WithAPIVersion("1.56"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return &Docker{cli: c}, &user
}
func fakeTree(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tree.tar")
	_, e := freezeTree(tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}, &tar.Header{Name: "app/f", Typeflag: tar.TypeReg, Mode: 0644}), p)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestIOBrokenAttachCannotPass(t *testing.T) {
	d, _ := fakeEngine(t, true, 0)
	r, e := d.runCase(context.Background(), "image", fakeTree(t), []string{"sh", "tool.sh"}, []byte("x"), time.Second, taskLimits{cpus: 1, memoryMB: 64})
	if e == nil && r.usable() {
		t.Fatalf("broken Docker output frame accepted as complete: status=%d bytes=%q", r.status, r.out)
	}
}
func TestIOReferenceExitCannotPass(t *testing.T) {
	d, _ := fakeEngine(t, false, 3)
	task := ioTaskDir(t, "cat\n", "cat\n", 1, 20, nil)
	iot, e := readIOTask(task)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "verifier"), 0700)
	v, _, e := d.ioPhase(context.Background(), task, iot, taskLimits{agent: time.Second, verifier: 20 * time.Second, cpus: 1, memoryMB: 64}, "image", fakeTree(t), dir)
	if e == nil {
		t.Fatalf("all reference cases exit 3 but verdict returned: reward=%v", v)
	}
}
func TestIOUnprivilegedEntry(t *testing.T) {
	d, user := fakeEngine(t, false, 0)
	r, e := d.runCase(context.Background(), "image", fakeTree(t), []string{"sh", "tool.sh"}, nil, time.Second, taskLimits{cpus: 1, memoryMB: 64})
	if e != nil || !r.usable() {
		t.Fatal(r, e)
	}
	if *user == "" && !strings.Contains(caseScript, "setpriv") {
		t.Fatal("candidate/oracle entry inherits image user; root default in generated image, no privilege drop")
	}
}
