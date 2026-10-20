package runner

// C7BJ-17 regressions on a synthetic engine API (no engine, no model): the verifier's small
// result files are read whole or not at all; the persisted network record is exactly one of
// the two values, and a reward is never shortened into a valid value.
import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// oneFile is the archive of one regular file as the engine sends it.
func oneFile(name, body string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	return b.Bytes()
}

// verifierFake runs verifierPhase against a fake engine whose verifier container holds the
// given archives (by path; absent: the engine reports no such file) and returns the reward,
// the error and the persisted network record ("" when none was written).
func verifierFake(t *testing.T, files map[string][]byte) (float64, error, string) {
	t.Helper()
	img := "sha256:" + strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/containers/create"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			fmt.Fprint(w, `{"Id":"v1"}`)
		case strings.HasSuffix(p, "/json") && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Id":"v1","Image":%q,"HostConfig":{"NetworkMode":"none","SecurityOpt":["no-new-privileges:true"]},"Mounts":[]}`, img)
		case strings.HasSuffix(p, "/archive") && r.Method == "PUT":
			w.WriteHeader(200)
		case strings.HasSuffix(p, "/archive"):
			b, ok := files[r.URL.Query().Get("path")]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"no such file"}`)
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"f","size":0,"mode":420}`)))
			w.Write(b)
		case strings.HasSuffix(p, "/start"):
			w.WriteHeader(204)
		case strings.HasSuffix(p, "/wait"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"StatusCode":0,"Error":null}`)
		case strings.HasSuffix(p, "/logs"):
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			w.WriteHeader(200)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithScheme("http"), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	d := &Docker{cli: c}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "verifier"), 0o700)
	artifact := filepath.Join(dir, "final.tar")
	os.WriteFile(artifact, oneFile("app/a.txt", "x"), 0o600)
	v, _, err := d.verifierPhase(context.Background(), taskLimits{verifier: time.Minute, cpus: 1, memoryMB: 128}, img, artifact, dir)
	rec, _ := os.ReadFile(filepath.Join(dir, "verifier", "network.txt"))
	return v, err, string(rec)
}

func TestVerifierNetworkRecordIsWholeOrAbsent(t *testing.T) {
	const blocked, reachable = "HINT_VERIFIER_NET=blocked", "HINT_VERIFIER_NET=reachable"
	reward := oneFile("reward.txt", "0\n")
	cut := oneFile("network.txt", blocked+"\n")
	cut = cut[:512+len(blocked)-4] // the header and part of the payload
	var two bytes.Buffer
	tw := tar.NewWriter(&two)
	for _, n := range []string{"network.txt", "other.txt"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(blocked))})
		tw.Write([]byte(blocked))
	}
	tw.Close()
	var link bytes.Buffer
	tw = tar.NewWriter(&link)
	tw.WriteHeader(&tar.Header{Name: "network.txt", Typeflag: tar.TypeSymlink, Linkname: "/etc/hostname"})
	tw.Close()
	for name, c := range map[string]struct {
		archive []byte
		want    string
	}{
		"blocked":                {oneFile("network.txt", blocked+"\n"), blocked + "\n"},
		"reachable":              {oneFile("network.txt", reachable+"\n"), reachable + "\n"},
		"contradictory oversize": {oneFile("network.txt", blocked+"\n"+strings.Repeat(" ", 80)+reachable+"\n"), ""},
		"oversize prefix":        {oneFile("network.txt", blocked+strings.Repeat("\n", 60)), ""},
		"other value":            {oneFile("network.txt", "HINT_VERIFIER_NET=maybe\n"), ""},
		"cut payload":            {cut, ""},
		"two entries":            {two.Bytes(), ""},
		"symlink":                {link.Bytes(), ""},
		"absent":                 {nil, ""},
	} {
		files := map[string][]byte{"/logs/verifier/reward.txt": reward}
		if c.archive != nil {
			files["/logs/verifier/network.txt"] = c.archive
		}
		v, err, rec := verifierFake(t, files)
		if err != nil || v != 0 || rec != c.want {
			t.Errorf("%s: reward %v err %v record %q, want %q", name, v, err, rec, c.want)
		}
	}
}

func TestVerifierRewardIsWhole(t *testing.T) {
	for name, c := range map[string]struct {
		archive []byte
		ok      bool
	}{
		"pass":                {oneFile("reward.txt", "1\n"), true},
		"oversize after pass": {oneFile("reward.txt", "1\n"+strings.Repeat(" ", 80)+"0\n"), false},
		"cut":                 {oneFile("reward.txt", "1\n")[:513], false},
	} {
		v, err, _ := verifierFake(t, map[string][]byte{"/logs/verifier/reward.txt": c.archive})
		if (err == nil) != c.ok || !c.ok && v != 0 || c.ok && v != 1 {
			t.Errorf("%s: reward %v err %v", name, v, err)
		}
	}
}
