package conformance_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

const mediaType = "application/vnd.hint+json; version=0.1"

// mockServer implements the exchange binding of exchange-v0.1 §3: the API carries the
// same documents as the file mode, byte for byte.
type mockServer struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (m *mockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/hint/v0.1/documents":
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/vnd.hint+json") {
			http.Error(w, "", http.StatusUnsupportedMediaType)
			return
		}
		body, _ := io.ReadAll(r.Body)
		code, docID, digest := "", any(nil), ""
		dec, err := hintconf.Decode(body)
		if err != nil || schemas.Validate(dec) != nil {
			code = "schema_invalid"
		} else {
			doc, _ := hintconf.Plain(dec)
			docID, digest = doc["document_id"], doc["content_digest"].(string)
			if fs := hintconf.Check(doc, nil); len(fs) > 0 {
				code = fs[0].Code
				if code != "digest_mismatch" && code != "content_forbidden" && code != "namespace_mismatch" {
					code = "reference_unresolved"
				}
				if fs[0].Code == hintconf.CodeDigest {
					code = "digest_mismatch"
				}
			}
		}
		payload := map[string]any{"accepted": []any{}, "rejected": []any{}}
		status := http.StatusCreated
		if code == "" {
			m.mu.Lock()
			m.docs[docID.(string)] = body
			m.mu.Unlock()
			payload["accepted"] = []any{map[string]any{"document_id": docID, "content_digest": digest}}
		} else {
			if docID == nil || !regexpHex(docID) {
				docID = nil
			}
			payload["rejected"] = []any{map[string]any{"document_id": docID, "code": code}}
			status = http.StatusUnprocessableEntity
		}
		writeDoc(w, status, receipt(payload))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/hint/v0.1/documents/"):
		m.mu.Lock()
		b, ok := m.docs[strings.TrimPrefix(r.URL.Path, "/hint/v0.1/documents/")]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mediaType)
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func regexpHex(v any) bool {
	s, _ := v.(string)
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func receipt(payload map[string]any) map[string]any {
	d := map[string]any{
		"hint_version": "0.1.0", "document_type": "receipt", "document_id": strings.Repeat("ab", 16),
		"project_namespace": hintconf.NamespaceID(seqKey()), "repositories": []any{},
		"producer":   map[string]any{"tool": "hint-server", "version": "0.1.0"},
		"created_at": "2026-10-11T12:00:00Z",
		"redaction": map[string]any{"policy_id": strings.Repeat("cd", 16), "profile": "detailed", "record_types": []any{},
			"free_text": "denied", "time_resolution_seconds": 0},
		"payload": payload,
	}
	dg, _ := hintconf.Digest(payload)
	d["content_digest"] = dg
	return d
}

func seqKey() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func writeDoc(w http.ResponseWriter, status int, d map[string]any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(d)
}

// TestFileAndAPIRoundTrip sends every valid document through a file round trip and a mock
// API round trip. Both must return the same bytes; the API receipt must itself be a valid
// HINT document; an invalid document must be rejected with a receipt code.
func TestFileAndAPIRoundTrip(t *testing.T) {
	srv := httptest.NewServer(&mockServer{docs: map[string][]byte{}})
	defer srv.Close()
	post := func(b []byte) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/hint/v0.1/documents", mediaType, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		dec, err := hintconf.Decode(rb)
		if err != nil {
			t.Fatal(err)
		}
		if err := schemas.Validate(dec); err != nil {
			t.Fatalf("receipt is not a valid HINT document: %v", err)
		}
		doc, _ := hintconf.Plain(dec)
		return resp.StatusCode, doc
	}
	files := validFiles(t)
	for _, f := range files {
		orig, _ := os.ReadFile(f)
		// file mode
		path := filepath.Join(t.TempDir(), "doc.hint.json")
		if err := os.WriteFile(path, orig, 0o600); err != nil {
			t.Fatal(err)
		}
		fromFile, _ := os.ReadFile(path)
		// API mode
		status, rcpt := post(orig)
		if status != http.StatusCreated {
			t.Errorf("%s: POST status %d: %v", f, status, rcpt["payload"])
			continue
		}
		var d map[string]any
		_ = json.Unmarshal(orig, &d)
		resp, err := http.Get(srv.URL + "/hint/v0.1/documents/" + d["document_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		fromAPI, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(fromFile, orig) || !bytes.Equal(fromAPI, orig) {
			t.Errorf("%s: round trip changed the document", f)
		}
	}
	for _, name := range []string{"content-prompt.json", "digest-mismatch.json", "dangling-relation.json"} {
		b, _ := os.ReadFile(filepath.Join("invalid", name))
		status, rcpt := post(b)
		rej := rcpt["payload"].(map[string]any)["rejected"].([]any)
		if status != http.StatusUnprocessableEntity || len(rej) != 1 {
			t.Errorf("%s: status %d, receipt %v", name, status, rcpt["payload"])
		}
	}
}

// TestImportResolution runs the client import validation on every case, from a file and
// through the mock API; both modes must give the expected result.
func TestImportResolution(t *testing.T) {
	var expected map[string]struct {
		Local    string
		Expected hintconf.ImportResult
	}
	readJSON(t, filepath.Join("resolve", "expected.json"), &expected)
	srv := httptest.NewServer(&mockServer{docs: map[string][]byte{}})
	defer srv.Close()
	pol := policy(t)
	for name, e := range expected {
		var local hintconf.LocalState
		readJSON(t, filepath.Join("resolve", e.Local), &local)
		raw, _ := os.ReadFile(filepath.Join("resolve", "cases", name))
		fileRes := hintconf.ImportRecommendations(raw, schemas, local, pol)
		if !reflect.DeepEqual(norm(fileRes), norm(e.Expected)) {
			t.Errorf("%s (file): got %+v, want %+v", name, fileRes, e.Expected)
		}
		// API mode: the client fetches the same document from the server
		resp, err := http.Post(srv.URL+"/hint/v0.1/documents", mediaType, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var rc map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&rc)
		resp.Body.Close()
		var d map[string]any
		_ = json.Unmarshal(raw, &d)
		if resp.StatusCode != http.StatusCreated {
			// the server rejects what the client rejects at document level, with a receipt code
			rej, _ := rc["payload"].(map[string]any)["rejected"].([]any)
			if resp.StatusCode != http.StatusUnprocessableEntity || len(rej) != 1 || fileRes.Document == "accepted" {
				t.Errorf("%s (API): status %d, receipt %v, file result %q", name, resp.StatusCode, rej, fileRes.Document)
			}
			continue
		}
		g, err := http.Get(srv.URL + "/hint/v0.1/documents/" + d["document_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		fromAPI, _ := io.ReadAll(g.Body)
		g.Body.Close()
		apiRes := hintconf.ImportRecommendations(fromAPI, schemas, local, pol)
		if !reflect.DeepEqual(norm(apiRes), norm(fileRes)) {
			t.Errorf("%s: API result %+v differs from file result %+v", name, apiRes, fileRes)
		}
	}
}

func norm(r hintconf.ImportResult) hintconf.ImportResult {
	if len(r.Plans) == 0 {
		r.Plans = nil
	}
	return r
}
