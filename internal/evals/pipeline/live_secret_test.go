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
// output scan cannot tell the key from an identifier that the harness exports on purpose (the
// model name, the endpoint, the agent and runner names and versions) when such a field contains
// the whole key in a form the scan searches (keyForms; case-sensitive like the scan), as the
// field is written and as JSON writes it. Such a key is refused. A key that merely contains a
// field is no collision: the field alone never matches the whole key. The names of the fields
// are reported, never the key.
func keyCollision(key string, exported map[string]string) error {
	if key == "" {
		return nil
	}
	forms := keyForms(key)
	var hit []string
	for name, v := range exported {
		j, _ := json.Marshal(v)
		for _, f := range forms {
			if bytes.Contains([]byte(v), f) || bytes.Contains(j, f) {
				hit = append(hit, name)
				break
			}
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

// Limits of the archive scan. Reaching one makes the scan incomplete, never clean.
var (
	scanMaxDepth   = 4         // nested containers (tar, gzip) below an output
	scanMaxEntries = 10000     // archive entries in all outputs together
	scanMaxBytes   = 256 << 20 // extracted and decompressed bytes in all outputs together
)

// keyScan is one scan of the outputs of a session. hits are "output[/entry#n...]: form[+label]";
// incomplete names every part that could not be read to its end (parse, decompression or read
// error, a limit). Neither ever holds the key value. An incomplete scan is not a clean scan.
type keyScan struct {
	forms      map[string][]byte
	hits       []string
	incomplete []string
	entries    int
	bytes      int
}

// keyHits scans every named output in name order; archives are read entry by entry in their
// order (a later entry with the same name never replaces an earlier one), also nested and
// gzip-compressed ones.
func keyHits(key string, outputs map[string][]byte) (hits, incomplete []string) {
	if key == "" {
		return nil, nil
	}
	ks := &keyScan{forms: keyForms(key)}
	names := make([]string, 0, len(outputs))
	for n := range outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ks.scan(n, outputs[n], 0)
	}
	sort.Strings(ks.hits)
	return ks.hits, ks.incomplete
}

func (ks *keyScan) scan(name string, b []byte, depth int) {
	for form, f := range ks.forms {
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
			ks.hits = append(ks.hits, h)
			i = at + len(f)
		}
	}
	gz, tr := len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b, len(b) >= 263 && string(b[257:262]) == "ustar"
	if !gz && !tr {
		return
	}
	if depth >= scanMaxDepth {
		ks.incomplete = append(ks.incomplete, name+": depth limit")
		return
	}
	if gz {
		ks.gunzip(name, b, depth)
	} else {
		ks.untar(name, b, depth)
	}
}

// take reads r to its end within the remaining byte budget; ok is false (and the part is
// recorded as incomplete) on a read error or when the budget is spent.
func (ks *keyScan) take(name string, r io.Reader) ([]byte, bool) {
	left := scanMaxBytes - ks.bytes
	data, err := io.ReadAll(io.LimitReader(r, int64(left)+1))
	ks.bytes += len(data)
	switch {
	case err != nil:
		ks.incomplete = append(ks.incomplete, fmt.Sprintf("%s: read: %v", name, err))
		return nil, false
	case len(data) > left:
		ks.incomplete = append(ks.incomplete, name+": byte limit")
		return nil, false
	}
	return data, true
}

func (ks *keyScan) gunzip(name string, b []byte, depth int) {
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		ks.incomplete = append(ks.incomplete, fmt.Sprintf("%s: gzip: %v", name, err))
		return
	}
	if data, ok := ks.take(name, z); ok {
		ks.scan(name+"|gunzip", data, depth+1)
	}
}

func (ks *keyScan) untar(name string, b []byte, depth int) {
	r := bytes.NewReader(b)
	tr := tar.NewReader(r)
	end := 0 // where the last entry's data and padding end
	for n := 1; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			// the reader also reports EOF for an archive cut at an entry boundary: a complete
			// archive has the two zero blocks of the end marker after the last entry
			if len(b) < end+1024 || !bytes.Equal(b[end:end+1024], make([]byte, 1024)) {
				ks.incomplete = append(ks.incomplete, fmt.Sprintf("%s: tar end marker missing after entry %d", name, n-1))
			}
			return
		}
		if err != nil {
			ks.incomplete = append(ks.incomplete, fmt.Sprintf("%s: tar entry %d: %v", name, n, err))
			return
		}
		if ks.entries++; ks.entries > scanMaxEntries {
			ks.incomplete = append(ks.incomplete, name+": entry limit")
			return
		}
		end = len(b) - r.Len() + int((h.Size+511)/512*512)
		entry := fmt.Sprintf("%s/%s#%d", name, h.Name, n)
		switch h.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
			ks.scan(entry+"(link)", []byte(h.Linkname), depth+1) // a link target is text too
			continue
		default:
			ks.incomplete = append(ks.incomplete, fmt.Sprintf("%s: entry type %q not scanned", entry, h.Typeflag))
			continue
		}
		data, ok := ks.take(entry, tr)
		if !ok {
			return
		}
		ks.scan(entry, data, depth+1)
	}
}

func TestKeyCollisionPreflight(t *testing.T) {
	exported := map[string]string{"model": "ai01", "base_url": "http://127.0.0.1:10011/v1", "agent": "opencode", "agent_version": "2.0.26", "runner": "hint-docker 0.1.0", "note": "a<b", "enc": "dG9rM24=", "query": "q=a+b"}
	refused := map[string]string{
		"equal to a field":      "ai01",
		"substring of a field":  "10011",
		"substring, other":      "docker 0.1",
		"json-escaped in field": `\u003c`, // "a<b" is written as "a\u003cb" in JSON output
		"base64 form in field":  "tok3n",  // "enc" holds base64("tok3n")
		"url-escaped in field":  "a b",    // "query" holds url.QueryEscape("a b")
	}
	for name, key := range refused {
		err := keyCollision(key, exported)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: the refusal prints the key", name)
		}
	}
	accepted := map[string]string{
		"empty":                        "",
		"distinct":                     "s3cr3t-Distinct-Value",
		"prefix-only (contains model)": "ai01-Synthetic-Long-Secret-9f3k",
		"contains a whole field":       "x-hint-docker 0.1.0-y",
		"other case than the field":    "AI01", // the scan is case-sensitive: "ai01" never matches "AI01"
	}
	for name, key := range accepted {
		if err := keyCollision(key, exported); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// TestPreflightAgreesWithTheScan: the preflight accepts exactly the keys whose scan over
// exported metadata is clean, and the scan still catches the whole key.
func TestPreflightAgreesWithTheScan(t *testing.T) {
	meta := map[string][]byte{
		"summary.json": []byte(`{"agent":{"model":"ai01","name":"opencode","version":"2.0.26"},"note":"a\u003cb","enc":"dG9rM24="}`),
		"test.log":     []byte(`"model": "ai01" at http://127.0.0.1:10011/v1`),
	}
	exported := map[string]string{"model": "ai01", "base_url": "http://127.0.0.1:10011/v1", "agent": "opencode", "agent_version": "2.0.26", "note": "a<b", "enc": "dG9rM24="}
	for _, key := range []string{"ai01-Synthetic-Long-Secret-9f3k", "AI01", "ai01", "10011", `\u003c`, "tok3n", "s3cr3t-Distinct-Value"} {
		hits, inc := keyHits(key, meta)
		if inc != nil {
			t.Fatalf("incomplete: %v", inc)
		}
		if refused := keyCollision(key, exported) != nil; refused != (len(hits) > 0) {
			t.Errorf("key %d chars: preflight refused %v, metadata hits %v", len(key), refused, hits)
		}
		if h, _ := keyHits(key, map[string][]byte{"stdout": []byte("x " + key + " y")}); len(h) == 0 {
			t.Errorf("key %d chars: the whole key in stdout is not caught", len(key))
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
	for _, key := range []string{"same-word", "same", "word", "ame-wor"} {
		t.Setenv("HINT_LOCAL_MODEL_KEY", key) // equal, and substrings of the model name
		refused, _ := liveGate(liveModel(t))
		if refused == nil || !strings.Contains(refused.Error(), "0 model requests") || strings.Contains(refused.Error(), key) {
			t.Fatalf("preflight: %v", refused)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Fatalf("the endpoint was contacted %d times before the refusal", n)
	}
	t.Setenv("HINT_LOCAL_MODEL_KEY", "same-word-Synthetic-Long-Secret") // contains the model name: no collision
	if refused, unreachable := liveGate(liveModel(t)); refused != nil || unreachable != nil {
		t.Fatalf("prefix-only key: %v %v", refused, unreachable)
	}
	for deadline := time.Now().Add(2 * time.Second); conns.Load() != 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections %d, want the one TCP check", n)
	}
}

type entry struct{ name, data string }

// tarOf builds a tar archive with the entries in the given order (optionally gzip-compressed).
func tarOf(gz bool, entries ...entry) []byte {
	var buf bytes.Buffer
	var w io.Writer = &buf
	var z *gzip.Writer
	if gz {
		z = gzip.NewWriter(&buf)
		w = z
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.data)), Typeflag: tar.TypeReg})
		tw.Write([]byte(e.data))
	}
	tw.Close()
	if z != nil {
		z.Close()
	}
	return buf.Bytes()
}

func gzipOf(data []byte) []byte {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write(data)
	z.Close()
	return buf.Bytes()
}

func TestKeyHits(t *testing.T) {
	const key = `Zq7<k"ey\x`
	j, _ := json.Marshal(map[string]string{"note": key})
	inner := tarOf(true, entry{"agent/opencode.txt", "tool output " + base64.StdEncoding.EncodeToString([]byte(key))})
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
		"plain archive entry":       {map[string][]byte{"run.tar": tarOf(false, entry{"result.json", "{}"}, entry{"agent/opencode.txt", key})}, []string{"run.tar/agent/opencode.txt#2: raw", "run.tar: raw"}},
		"nested gzip archive entry": {map[string][]byte{"run.tar": tarOf(false, entry{"result.json", "{}"}, entry{"agent/final.tar.gz", string(inner)})}, []string{"run.tar/agent/final.tar.gz#2|gunzip/agent/opencode.txt#1: base64", "run.tar/agent/final.tar.gz#2|gunzip: base64"}},
		"gzip text":                 {map[string][]byte{"log.gz": gzipOf([]byte("x=" + key))}, []string{"log.gz: raw", "log.gz|gunzip: raw"}}, // short text is stored nearly literally,
		// C7BJ-19: a later entry with the same name does not hide an earlier one
		"duplicate name, secret first": {map[string][]byte{"run.tar": tarOf(false, entry{"same.tar.gz", string(tarOf(true, entry{"out", key}))}, entry{"same.tar.gz", "clean"})}, []string{"run.tar/same.tar.gz#1|gunzip/out#1: raw", "run.tar/same.tar.gz#1|gunzip: raw"}},
		"duplicate name, secret last":  {map[string][]byte{"run.tar": tarOf(false, entry{"same.tar.gz", "clean"}, entry{"same.tar.gz", string(tarOf(true, entry{"out", key}))})}, []string{"run.tar/same.tar.gz#2|gunzip/out#1: raw", "run.tar/same.tar.gz#2|gunzip: raw"}},
		"summary and log, clean":       {map[string][]byte{"summary.json": []byte(`{"model":"ai01"}`), "test.log": []byte(`"model": "ai01"`)}, nil},
		"clean archives":               {map[string][]byte{"run.tar": tarOf(false, entry{"a", "x"}, entry{"b.tar.gz", string(tarOf(true, entry{"c", "y"}))})}, nil},
	}
	for name, c := range cases {
		got, incomplete := keyHits(key, c.outputs)
		if fmt.Sprint(got) != fmt.Sprint(c.want) || incomplete != nil {
			t.Errorf("%s: hits %v incomplete %v, want %v", name, got, incomplete, c.want)
		}
		for _, h := range got {
			if strings.Contains(h, key) {
				t.Errorf("%s: a hit prints the key", name)
			}
		}
	}
	// benign model metadata under a distinct credential: the model name is everywhere, no hit
	meta := map[string][]byte{"summary.json": []byte(`{"agent":{"model":"ai01","name":"opencode","version":"2.0.26"}}`), "test.log": []byte(`model ai01 at http://127.0.0.1:10011/v1`)}
	if h, inc := keyHits("s3cr3t-Distinct-Value", meta); h != nil || inc != nil {
		t.Errorf("benign metadata: %v %v", h, inc)
	}
	// the session-1 shape (key equal to the model name): the raw scan still fires, which is why
	// the preflight refuses that configuration before any request
	if h, _ := keyHits("ai01", meta); len(h) != 2 {
		t.Errorf("colliding key: %v", h)
	}
}

// TestKeyScanIncomplete: a part that cannot be read to its end makes the scan incomplete,
// never clean, and the session verdict fails.
func TestKeyScanIncomplete(t *testing.T) {
	const key = "s3cr3t-Distinct-Value"
	good := tarOf(false, entry{"a", "clean"}, entry{"b", "clean too"})
	gz := tarOf(true, entry{"a", "clean"})
	nest := func(levels int) []byte {
		b := tarOf(false, entry{"leaf", "clean"})
		for range levels {
			b = tarOf(false, entry{"next.tar", string(b)})
		}
		return b
	}
	for name, c := range map[string]struct {
		out  []byte
		want string
		set  func()
	}{
		"tar cut in entry data": {tarOf(false, entry{"a", strings.Repeat("x", 2000)})[:1000], "read", nil},
		"tar cut in padding":    {good[:600], "end marker missing after entry 1", nil},
		"tar cut at an entry":   {good[:1024], "end marker missing after entry 1", nil},
		"corrupt tar header":    {append(append([]byte{}, good[:148]...), append([]byte("XXXXXXXX"), good[156:]...)...), "tar entry 1", nil},
		"truncated gzip":        {gz[:len(gz)-10], "read", nil},
		"corrupt gzip header":   {append([]byte{0x1f, 0x8b, 0xff}, gz[3:]...), "gzip", nil},
		"depth limit":           {nest(6), "depth limit", nil},
		"entry limit":           {good, "entry limit", func() { scanMaxEntries = 1 }},
		"byte limit":            {good, "byte limit", func() { scanMaxBytes = 6 }},
		"gzip bomb, byte limit": {gzipOf(make([]byte, 1<<20)), "byte limit", func() { scanMaxBytes = 1 << 10 }},
	} {
		t.Run(name, func(t *testing.T) {
			d, e, b := scanMaxDepth, scanMaxEntries, scanMaxBytes
			defer func() { scanMaxDepth, scanMaxEntries, scanMaxBytes = d, e, b }()
			if c.set != nil {
				c.set()
			}
			hits, inc := keyHits(key, map[string][]byte{"run.tar": c.out})
			if hits != nil || len(inc) == 0 || !strings.Contains(fmt.Sprint(inc), c.want) {
				t.Fatalf("hits %v incomplete %v, want %q", hits, inc, c.want)
			}
			if (liveSession{KeyScanIncomplete: inc}).verdict() == nil {
				t.Fatal("an incomplete scan passes the verdict")
			}
		})
	}
	// controls: the same shapes within the limits are complete
	for _, out := range [][]byte{good, gz, nest(3)} {
		if hits, inc := keyHits(key, map[string][]byte{"run.tar": out}); hits != nil || inc != nil {
			t.Errorf("control: %v %v", hits, inc)
		}
	}
}
