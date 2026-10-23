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
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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
// field is no collision: the field alone never matches the whole key. One equality is
// supported: the key may equal the declared model (publicModelField), because the scan exempts
// exactly that one field of the summary (summaryPublic) and nothing else; a key that is only a
// part of the model name is still refused. The names of the fields are reported, never the key.
func keyCollision(key string, exported map[string]string) error {
	if key == "" {
		return nil
	}
	forms := keyForms(key)
	var hit []string
	for name, v := range exported {
		if name == publicModelField && v == key {
			continue
		}
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

// publicModelField is the exported field that may equal the key (the owner's LAN model accepts
// its public model ID as the key).
const publicModelField = "model"

// publicField is a field that the harness itself writes with a declared public value: the JSON
// object path in one named output. When the key equals that value, the scan reads a copy of
// the output in which exactly this field's value is replaced (maskPublic). So with such a key
// the scan checks where else the key is placed or disclosed (any other field, the agent's
// output, archives, logs, credential contexts); it cannot show that the shared public bytes
// never appear, because the harness writes them on purpose.
type publicField struct {
	output, path, value string
}

// summaryPublic is the one public field of a session summary: the model of the trial's agent
// record, which the harness copies from the trial manifest.
func summaryPublic(output, model string) publicField {
	return publicField{output, "trial.agent.model", model}
}

// publicPlaceholder replaces a public field's value in a scan copy or a log rendering.
const publicPlaceholder = "(declared public value)"

// maskPublic returns a copy of b in which the value of each field is the placeholder. A field
// is replaced only when b is valid JSON, its path occurs exactly once and its value is written
// exactly as JSON writes the declared value; otherwise b keeps that field and the scan reads
// it. b itself is never changed.
func maskPublic(b []byte, fields []publicField) []byte {
	if len(fields) == 0 || !json.Valid(b) {
		return b
	}
	var spans [][2]int
	for _, f := range fields {
		want, _ := json.Marshal(f.value)
		if at := jsonPathSpans(b, f.path); len(at) == 1 && at[0][0] >= 0 && bytes.Equal(b[at[0][0]:at[0][1]], want) {
			spans = append(spans, at[0])
		}
	}
	if len(spans) == 0 {
		return b
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	out, prev := make([]byte, 0, len(b)), 0
	for _, s := range spans {
		if s[0] < prev {
			continue
		}
		out = append(append(out, b[prev:s[0]]...), `"`+publicPlaceholder+`"`...)
		prev = s[1]
	}
	return append(out, b[prev:]...)
}

// jsonPathSpans finds every value at the dotted path in the JSON document b (object keys;
// array elements by index): the byte span of a scalar value, [-1 -1] for an object or array.
func jsonPathSpans(b []byte, path string) [][2]int {
	type frame struct {
		obj, wantKey bool
		key          string
		idx          int
	}
	var stack []frame
	here := func() string {
		parts := make([]string, len(stack))
		for i, f := range stack {
			if parts[i] = f.key; !f.obj {
				parts[i] = strconv.Itoa(f.idx)
			}
		}
		return strings.Join(parts, ".")
	}
	next := func() {
		if n := len(stack); n > 0 {
			if stack[n-1].obj {
				stack[n-1].wantKey = true
			} else {
				stack[n-1].idx++
			}
		}
	}
	var spans [][2]int
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		pos := int(dec.InputOffset())
		tok, err := dec.Token()
		if err != nil {
			return spans
		}
		if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].wantKey {
			if k, ok := tok.(string); ok {
				stack[n-1].key, stack[n-1].wantKey = k, false
				continue
			}
		}
		if tok == json.Delim('}') || tok == json.Delim(']') {
			stack = stack[:len(stack)-1]
			next()
			continue
		}
		open := tok == json.Delim('{') || tok == json.Delim('[')
		if here() == path {
			if open {
				spans = append(spans, [2]int{-1, -1})
			} else {
				start := pos
				for start < len(b) && strings.IndexByte(" \t\r\n:,", b[start]) >= 0 {
					start++
				}
				spans = append(spans, [2]int{start, int(dec.InputOffset())})
			}
		}
		if open {
			stack = append(stack, frame{obj: tok == json.Delim('{'), wantKey: true})
			continue
		}
		next()
	}
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
// gzip-compressed ones. A public field of an output is masked in the scan copy only when the
// key equals its declared value (maskPublic).
func keyHits(key string, outputs map[string][]byte, public ...publicField) (hits, incomplete []string) {
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
		var mine []publicField
		for _, f := range public {
			if f.output == n && f.value == key {
				mine = append(mine, f)
			}
		}
		ks.scan(n, maskPublic(outputs[n], mine), 0)
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
		"equal to a field":      "opencode",
		"part of the model":     "i0", // the summary's model field is masked only for a key equal to it
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
		"equal to the declared model":  "ai01", // supported: the scan masks exactly the summary's model field
	}
	for name, key := range accepted {
		if err := keyCollision(key, exported); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// TestPreflightAgreesWithTheScan: the preflight accepts exactly the keys whose scan over
// exported metadata is clean (the summary with its one public field, the log with the summary
// as the harness logs it), and the scan still catches the whole key.
func TestPreflightAgreesWithTheScan(t *testing.T) {
	summary := []byte(`{"trial":{"agent":{"model":"ai01","name":"opencode","version":"2.0.26"}},"note":"a\u003cb","enc":"dG9rM24="}`)
	meta := map[string][]byte{
		"summary.json": summary,
		"test.log":     append([]byte("summary at http://127.0.0.1:10011/v1\n"), maskPublic(summary, []publicField{summaryPublic("", "ai01")})...),
	}
	exported := map[string]string{"model": "ai01", "base_url": "http://127.0.0.1:10011/v1", "agent": "opencode", "agent_version": "2.0.26", "note": "a<b", "enc": "dG9rM24="}
	for _, key := range []string{"ai01-Synthetic-Long-Secret-9f3k", "AI01", "ai01", "i0", "10011", `\u003c`, "tok3n", "s3cr3t-Distinct-Value"} {
		hits, inc := keyHits(key, meta, summaryPublic("summary.json", "ai01"))
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
	for _, key := range []string{"same", "word", "ame-wor", "opencode", "0.1.0"} {
		t.Setenv("HINT_LOCAL_MODEL_KEY", key) // parts of the model name, other exported fields
		refused, _ := liveGate(liveModel(t))
		if refused == nil || !strings.Contains(refused.Error(), "0 model requests") || strings.Contains(refused.Error(), key) {
			t.Fatalf("preflight: %v", refused)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Fatalf("the endpoint was contacted %d times before the refusal", n)
	}
	// equal to the declared model (supported), and a key that contains the model name
	for _, key := range []string{"same-word", "same-word-Synthetic-Long-Secret"} {
		t.Setenv("HINT_LOCAL_MODEL_KEY", key)
		if refused, unreachable := liveGate(liveModel(t)); refused != nil || unreachable != nil {
			t.Fatalf("accepted key: %v %v", refused, unreachable)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); conns.Load() != 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections %d, want one TCP check per accepted key", n)
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
	// the session-1 shape (key equal to the model name) without a declared public field: the raw
	// scan fires on every occurrence (TestPublicModelField has the one exemption)
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

// session1Summary is the stored summary of §24 session 1 byte for byte, with the model name
// (which was also the key) replaced by the synthetic declared model "lan-m1".
const session1Summary = `{
  "ledgers": [
    {
      "bypass": "enforced",
      "counts": {
        "admitted": 6,
        "cancelled": 0,
        "complete": 6,
        "error": 0,
        "late": 0,
        "missing_usage": 0,
        "rejected_limit": 0,
        "truncated": 0,
        "unknown": 0,
        "unreadable": 0,
        "usage_conflict": 0,
        "usage_partial": 0
      },
      "reasons": [],
      "state": "complete"
    }
  ],
  "ledgers_not_written": 0,
  "model_requests": 6,
  "opencode": "2.0.26",
  "outcomes": {
    "passed": 1
  },
  "report_error": "\u003cnil\u003e",
  "runner": "hint-docker 0.1.0",
  "series_may_continue": false,
  "trial": {
    "agent": {
      "model": "lan-m1",
      "model_check": "verified",
      "name": "opencode",
      "version": "2.0.26"
    },
    "budget": {
      "max_requests": 8,
      "max_seconds": 300,
      "stop_reason": "completed"
    },
    "checks": [
      {
        "check_id": "C1",
        "status": "pass"
      },
      {
        "check_id": "C2",
        "status": "pass"
      }
    ],
    "counters": {
      "agent_seconds": 98.121723,
      "cache_tokens": null,
      "input_tokens": null,
      "output_tokens": null,
      "requests": 6,
      "retries": 0,
      "wall_seconds": 103.703054
    },
    "outcome": "passed",
    "result_trust": "same_process",
    "runner": {
      "name": "hint-docker",
      "version": "0.1.0"
    }
  },
  "trials": 1,
  "verdict": "the model key appears in an output",
  "wall_seconds": 106
}`

// TestPublicModelField: with a key equal to the declared model, only the summary's one public
// field (trial.agent.model, written exactly as JSON writes the declared value, once) is masked
// in a scan copy; every other place of the same literal is still a hit, nothing is rewritten.
func TestPublicModelField(t *testing.T) {
	const model = "lan-m1"
	pub := summaryPublic("summary.json", model)
	scan := func(key string, outputs map[string][]byte) []string {
		t.Helper()
		before := map[string]string{}
		for n, b := range outputs {
			before[n] = string(b)
		}
		hits, inc := keyHits(key, outputs, pub)
		if inc != nil {
			t.Fatalf("incomplete: %v", inc)
		}
		for n, b := range outputs {
			if string(b) != before[n] {
				t.Fatalf("%s: the scan changed the stored output", n)
			}
		}
		return hits
	}
	s1 := []byte(session1Summary)
	if h := scan(model, map[string][]byte{"summary.json": s1}); h != nil {
		t.Errorf("session 1 summary: %v", h)
	}
	if h, _ := keyHits(model, map[string][]byte{"summary.json": s1}); len(h) != 1 {
		t.Errorf("session 1 summary without the public field: %v, want the one hit", h)
	}
	field := func(path string, v any) []byte { // session 1 with one more field set
		var d map[string]any
		json.Unmarshal(s1, &d)
		m, parts := d, strings.Split(path, ".")
		for _, p := range parts[:len(parts)-1] {
			m = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = v
		b, _ := json.MarshalIndent(d, "", "  ")
		return b
	}
	caught := map[string]map[string][]byte{
		"unrelated summary field":     {"summary.json": field("trial.agent.name", model)},
		"top-level summary field":     {"summary.json": field("note", "x "+model)},
		"model field in a ledger":     {"summary.json": field("ledgers", []any{map[string]any{"model": model}})},
		"same path in another output": {"summary.json": s1, "report.json": s1},
		"agent stdout":                {"summary.json": s1, "stdout": []byte("using " + model)},
		"authorization header":        {"summary.json": s1, "log": []byte("Authorization: Bearer " + model)},
		"api key field":               {"summary.json": s1, "cfg": []byte(`{"apiKey": "` + model + `"}`)},
		"environment dump":            {"summary.json": s1, "env": []byte("HINT_LOCAL_MODEL_KEY=" + model)},
		"archive entry":               {"summary.json": s1, "run.tar": tarOf(false, entry{"agent/opencode.txt", "model " + model})},
		"gzip archive entry":          {"summary.json": s1, "run.tar": tarOf(false, entry{"agent/final.tar.gz", string(tarOf(true, entry{"cfg.json", model}))})},
		"base64 in stdout":            {"summary.json": s1, "stdout": []byte(base64.StdEncoding.EncodeToString([]byte(model)))},
		// the field itself is exempt only as the harness writes it: once, exactly the declared value
		"model field, another value":  {"summary.json": field("trial.agent.model", model+":latest")},
		"model field, path twice":     {"summary.json": []byte(`{"trial":{"agent":{"model":"lan-m1","model":"other"}}}`)}, // JSON reads "other"
		"model field not a string":    {"summary.json": []byte(`{"trial":{"agent":{"model":["lan-m1"]}}}`)},
		"summary with trailing bytes": {"summary.json": append(append([]byte{}, s1...), '}')},
		"summary cut after the field": {"summary.json": s1[:bytes.Index(s1, []byte(`"model_check"`))]},
	}
	for name, outputs := range caught {
		h := scan(model, outputs)
		if len(h) == 0 {
			t.Errorf("%s: not caught", name)
		}
		if bytes.Equal(outputs["summary.json"], s1) && strings.Contains(fmt.Sprint(h), "summary.json:") {
			t.Errorf("%s: the public field itself hit: %v", name, h)
		}
	}
	// a longer key that starts with the model: no exemption, the whole key is still searched
	long := model + "-Synthetic-Long-Secret"
	if h := scan(long, map[string][]byte{"summary.json": s1, "stdout": []byte("using " + model)}); h != nil {
		t.Errorf("longer key, model only: %v", h)
	}
	if h := scan(long, map[string][]byte{"summary.json": s1, "stdout": []byte("x " + long)}); fmt.Sprint(h) != "[stdout: raw]" {
		t.Errorf("longer key in stdout: %v", h)
	}
	if h := scan(long, map[string][]byte{"summary.json": field("trial.agent.model", long)}); len(h) == 0 {
		t.Errorf("longer key in the model field: not caught")
	}
	// the field as JSON writes it: a declared model with an escaped character is masked too
	esc := field("trial.agent.model", "a<b")
	if h, _ := keyHits("a<b", map[string][]byte{"summary.json": esc}, summaryPublic("summary.json", "a<b")); h != nil {
		t.Errorf("escaped declared model: %v", h)
	}
	// the scan copy and the log rendering: valid JSON, only the field differs
	masked := maskPublic(s1, []publicField{pub})
	var a, b map[string]any
	json.Unmarshal(s1, &a)
	if err := json.Unmarshal(masked, &b); err != nil {
		t.Fatal(err)
	}
	if b["trial"].(map[string]any)["agent"].(map[string]any)["model"] != publicPlaceholder {
		t.Errorf("the field is not masked")
	}
	b["trial"].(map[string]any)["agent"].(map[string]any)["model"] = model
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the rendering changes more than the field")
	}
	if string(s1) != session1Summary {
		t.Errorf("the rendering changed the summary")
	}
	// an incomplete scan stays incomplete with the exemption
	cut := tarOf(false, entry{"a", "clean"})[:600]
	if _, inc := keyHits(model, map[string][]byte{"summary.json": s1, "run.tar": cut}, pub); inc == nil {
		t.Errorf("a cut archive is complete")
	}
}

// TestLiveOutputScanFiles: the runner's scan of the stored files agrees with the session's: the
// actual summary file and the go test log as the harness writes them are clean for a key equal
// to the model; the same literal in any other file or field is caught; an unreadable file fails.
func TestLiveOutputScanFiles(t *testing.T) {
	const model = "lan-m1"
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "test.log")
	pub := summaryPublic(liveSummaryFile, model)
	os.WriteFile(filepath.Join(dir, liveSummaryFile), []byte(session1Summary), 0o600)
	os.WriteFile(filepath.Join(dir, "opencode-trial-run.tar"), tarOf(false, entry{"agent/opencode.txt", "done"}), 0o600)
	os.WriteFile(log, append([]byte("=== RUN   TestLiveOpenCodeTrial\n    live_test.go: summary:\n"), maskPublic([]byte(session1Summary), []publicField{pub})...), 0o600)
	read := func() map[string][]byte {
		t.Helper()
		out, err := liveFiles([]string{dir, log})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if h, inc := keyHits(model, read(), pub); h != nil || inc != nil {
		t.Fatalf("stored session: %v %v", h, inc)
	}
	// the old unmasked log line (session 1) and a summary under another name are caught
	os.WriteFile(log, []byte(`"model": "`+model+`"`), 0o600)
	os.WriteFile(filepath.Join(dir, "copy-of-summary.json"), []byte(session1Summary), 0o600)
	if h, _ := keyHits(model, read(), pub); fmt.Sprint(h) != "[copy-of-summary.json: raw test.log: raw]" {
		t.Errorf("hits %v", h)
	}
	// a symlink is not read: the scan fails instead of skipping it
	os.Symlink(log, filepath.Join(dir, "link"))
	if _, err := liveFiles([]string{dir}); err == nil {
		t.Errorf("a symlink was accepted")
	}
}
