package pipeline

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// keyCollision is the preflight of a live session, run before any model or agent request: the
// raw secret scan of the outputs cannot tell the key from an identifier that the harness
// exports on purpose (the model name, the endpoint, the agent and runner names and versions),
// so a key that equals, contains or is contained in such a field is refused. The names of the
// fields are reported, never the key.
func keyCollision(key string, exported map[string]string) error {
	if key == "" {
		return nil
	}
	k := strings.ToLower(key)
	var hit []string
	for name, v := range exported {
		v = strings.ToLower(v)
		if v != "" && (strings.Contains(v, k) || strings.Contains(k, v)) {
			hit = append(hit, name)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	return fmt.Errorf("the model key collides with exported non-secret fields %v: the output scan could not tell them apart; use a distinct key (0 model requests)", hit)
}

// keyForms are the encodings of the key that the scan looks for: the raw text first (a bare
// key anywhere fails), then the escaped and encoded forms in which a raw match is not visible.
func keyForms(key string) map[string][]byte {
	forms := map[string][]byte{"raw": []byte(key)}
	add := func(name, v string) {
		if v != key && v != "" {
			forms[name] = []byte(v)
		}
	}
	j, _ := json.Marshal(key)
	add("json_escaped", string(j[1:len(j)-1]))
	add("url_escaped", url.QueryEscape(key))
	add("base64", base64.StdEncoding.EncodeToString([]byte(key)))
	add("base64_bearer", base64.StdEncoding.EncodeToString([]byte("Bearer "+key)))
	return forms
}

// credentialForms are additive labels for a hit: they name the kind of disclosure, they never
// replace the raw scan.
var credentialForms = map[string]*regexp.Regexp{
	"authorization_header": regexp.MustCompile(`(?i)authorization["']?\s*[:=]\s*["']?(bearer|basic)\s+`),
	"api_key_field":        regexp.MustCompile(`(?i)["']?(api[_-]?key|x-api-key|apikey)["']?\s*[:=]\s*["']?`),
	"key_env":              regexp.MustCompile(`(?i)[A-Z0-9_]*(KEY|TOKEN|SECRET)\s*=\s*["']?`),
}

// keyHits scans every named output (archives entry by entry, also nested and gzip-compressed
// ones) for every form of the key. A hit is "output[/entry]: form[+credential label]"; the
// key value is never part of it.
func keyHits(key string, outputs map[string][]byte) []string {
	if key == "" {
		return nil
	}
	forms := keyForms(key)
	var hits []string
	var scan func(name string, b []byte, depth int)
	scan = func(name string, b []byte, depth int) {
		for form, f := range forms {
			for i := 0; ; {
				j := bytes.Index(b[i:], f)
				if j < 0 {
					break
				}
				at := i + j
				h := name + ": " + form
				before := b[max(0, at-40):at]
				for label, re := range credentialForms {
					if loc := re.FindAllIndex(before, -1); len(loc) > 0 && loc[len(loc)-1][1] == len(before) {
						h += "+" + label
					}
				}
				hits = append(hits, h)
				i = at + len(f)
			}
		}
		if depth < 3 {
			for entry, data := range archiveEntries(b) {
				scan(name+"/"+entry, data, depth+1)
			}
		}
	}
	names := make([]string, 0, len(outputs))
	for n := range outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		scan(n, outputs[n], 0)
	}
	sort.Strings(hits)
	return hits
}

// archiveEntries reads b as a tar archive (plain or gzip); anything else has no entries.
func archiveEntries(b []byte) map[string][]byte {
	var r io.Reader = bytes.NewReader(b)
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		z, err := gzip.NewReader(r)
		if err != nil {
			return nil
		}
		r = z
	}
	out := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			break
		}
		out[h.Name] = data
	}
	return out
}

func TestKeyCollisionPreflight(t *testing.T) {
	exported := map[string]string{"model": "ai01", "base_url": "http://127.0.0.1:10011/v1", "agent": "opencode", "agent_version": "2.0.26", "runner": "hint-docker 0.1.0"}
	for _, key := range []string{"ai01", "AI01", "opencode", "2.0.26", "10011", "hint-docker 0.1.0 extra"} {
		if err := keyCollision(key, exported); err == nil {
			t.Errorf("a key that collides with an exported field is accepted")
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("the refusal prints the key")
		}
	}
	for _, key := range []string{"", "s3cr3t-Distinct-Value"} {
		if err := keyCollision(key, exported); err != nil {
			t.Errorf("a distinct key is refused: %v", err)
		}
	}
}

// TestLiveTrialCollisionNeedsNoRequest: with a colliding key the live gate refuses before the
// endpoint is contacted (a listener counts every connection: 0); with a distinct key the same
// gate makes exactly the TCP connect and nothing else.
func TestLiveTrialCollisionNeedsNoRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()
	t.Setenv("HINT_LIVE_MODEL_URL", "http://"+ln.Addr().String()+"/v1")
	t.Setenv("HINT_LIVE_MODEL", "same-word")
	t.Setenv("HINT_LOCAL_MODEL_KEY", "same-word")
	refused, _ := liveGate(liveModel(t))
	if refused == nil || !strings.Contains(refused.Error(), "0 model requests") || strings.Contains(refused.Error(), "same-word") {
		t.Fatalf("preflight: %v", refused)
	}
	time.Sleep(100 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Fatalf("the endpoint was contacted %d times before the refusal", n)
	}
	t.Setenv("HINT_LOCAL_MODEL_KEY", "s3cr3t-Distinct-Value")
	if refused, unreachable := liveGate(liveModel(t)); refused != nil || unreachable != nil {
		t.Fatalf("distinct key: %v %v", refused, unreachable)
	}
	for deadline := time.Now().Add(2 * time.Second); conns.Load() != 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections %d, want the one TCP check", n)
	}
}

func TestKeyHits(t *testing.T) {
	const key = `Zq7<k"ey\x`
	j, _ := json.Marshal(map[string]string{"note": key})
	tarOf := func(gz bool, files map[string]string) []byte {
		var buf bytes.Buffer
		var w io.Writer = &buf
		var z *gzip.Writer
		if gz {
			z = gzip.NewWriter(&buf)
			w = z
		}
		tw := tar.NewWriter(w)
		for n, c := range files {
			tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(c)), Typeflag: tar.TypeReg})
			tw.Write([]byte(c))
		}
		tw.Close()
		if z != nil {
			z.Close()
		}
		return buf.Bytes()
	}
	inner := tarOf(true, map[string]string{"agent/opencode.txt": "tool output " + base64.StdEncoding.EncodeToString([]byte(key))})
	cases := map[string]struct {
		outputs map[string][]byte
		want    []string
	}{
		"bare secret in stdout":     {map[string][]byte{"stdout": []byte("the run printed " + key + " once")}, []string{"stdout: raw"}},
		"bare secret in the report": {map[string][]byte{"report.json": []byte(`{"x":"` + key + `"}`)}, []string{"report.json: raw"}},
		"authorization header":      {map[string][]byte{"log": []byte("Authorization: Bearer " + key)}, []string{"log: raw+authorization_header"}},
		"api key field":             {map[string][]byte{"cfg": []byte(`"apiKey": "` + key + `"`)}, []string{"cfg: raw+api_key_field"}},
		"environment dump":          {map[string][]byte{"env": []byte("HINT_LOCAL_MODEL_KEY=" + key)}, []string{"env: raw+key_env"}},
		"json escaped":              {map[string][]byte{"summary.json": j}, []string{"summary.json: json_escaped"}},
		"url escaped":               {map[string][]byte{"log": []byte("GET /v1?k=" + url.QueryEscape(key))}, []string{"log: url_escaped"}},
		"basic bearer base64":       {map[string][]byte{"log": []byte("x " + base64.StdEncoding.EncodeToString([]byte("Bearer "+key)))}, []string{"log: base64_bearer"}},
		"archive entry, nested gz":  {map[string][]byte{"run.tar": tarOf(false, map[string]string{"result.json": "{}", "agent/final.tar.gz": string(inner)})}, []string{"run.tar/agent/final.tar.gz/agent/opencode.txt: base64"}},
		"summary and log, clean":    {map[string][]byte{"summary.json": []byte(`{"model":"ai01"}`), "test.log": []byte(`"model": "ai01"`)}, nil},
	}
	for name, c := range cases {
		got := keyHits(key, c.outputs)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: hits %v, want %v", name, got, c.want)
		}
		for _, h := range got {
			if strings.Contains(h, key) {
				t.Errorf("%s: a hit prints the key", name)
			}
		}
	}
	// benign model metadata under a distinct credential: the model name is everywhere, no hit
	meta := map[string][]byte{"summary.json": []byte(`{"agent":{"model":"ai01","name":"opencode","version":"2.0.26"}}`), "test.log": []byte(`model ai01 at http://127.0.0.1:10011/v1`)}
	if h := keyHits("s3cr3t-Distinct-Value", meta); h != nil {
		t.Errorf("benign metadata: %v", h)
	}
	// the session-1 shape (key equal to the model name): the raw scan still fires, which is why
	// the preflight refuses that configuration before any request
	if h := keyHits("ai01", meta); len(h) != 2 {
		t.Errorf("colliding key: %v", h)
	}
}
