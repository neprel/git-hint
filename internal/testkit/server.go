// Package testkit holds test helpers: a mock HINT server and fixture repositories. It is
// used by tests only. The mock server returns a fixed-form recommendation for the uploaded
// snapshot; it implements no analysis.
package testkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"

	"github.com/neprel/git-hint/internal/hint"
)

const mediaType = "application/vnd.hint+json; version=0.1"

// Server is a mock HINT server (exchange-v0.1 §3).
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	Docs     map[string][]byte // document_id -> raw bytes as received
	Requests [][]byte          // every POST body, in order
	Fail     bool              // answer 503 to every request (simulated outage)
	// Mutate changes the recommendation payload before it is sealed (for negative tests).
	Mutate func(doc map[string]any)
}

// NewServer starts a mock server.
func NewServer() *Server {
	s := &Server{Docs: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fail := s.Fail
	s.mu.Unlock()
	if fail {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/hint/v0.1/documents":
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/vnd.hint+json") {
			http.Error(w, "", http.StatusUnsupportedMediaType)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		s.mu.Lock()
		s.Requests = append(s.Requests, body)
		s.mu.Unlock()
		code, id, digest := check(body)
		payload := map[string]any{"accepted": []any{}, "rejected": []any{}}
		status := http.StatusCreated
		if code == "" {
			s.mu.Lock()
			s.Docs[id] = body
			s.mu.Unlock()
			payload["accepted"] = []any{map[string]any{"document_id": id, "content_digest": digest}}
		} else {
			var did any
			if len(id) == 32 {
				did = id
			}
			payload["rejected"] = []any{map[string]any{"document_id": did, "code": code}}
			status = http.StatusUnprocessableEntity
		}
		write(w, status, envelope("receipt", "00000000000000000000000000000000", nil, nil, payload))
	case r.Method == http.MethodGet && r.URL.Path == "/hint/v0.1/recommendations":
		ns := r.URL.Query().Get("project_namespace")
		doc := s.recommend(ns)
		if doc == nil {
			http.NotFound(w, r)
			return
		}
		write(w, http.StatusOK, doc)
	default:
		http.NotFound(w, r)
	}
}

func check(body []byte) (code, id, digest string) {
	sch, err := hint.LoadSchemas()
	if err != nil {
		return "schema_invalid", "", ""
	}
	dec, err := hint.Decode(body)
	if err != nil || sch.Validate(dec) != nil {
		return "schema_invalid", "", ""
	}
	doc, _ := hint.Plain(dec)
	id, _ = doc["document_id"].(string)
	digest, _ = doc["content_digest"].(string)
	if f := hint.Check(doc, nil); len(f) > 0 {
		switch f[0].Code {
		case hint.CodeDigest, hint.CodeContent, hint.CodeNamespace:
			return f[0].Code, id, digest
		}
		return "reference_unresolved", id, digest
	}
	return "", id, digest
}

// Snapshots returns the uploaded repository_snapshot payloads of a namespace.
func (s *Server) snapshots(ns string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	ids := make([]string, 0, len(s.Docs))
	for id := range s.Docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var d map[string]any
		if json.Unmarshal(s.Docs[id], &d) == nil && d["document_type"] == "repository_snapshot" && d["project_namespace"] == ns {
			out = append(out, d)
		}
	}
	return out
}

// recommend builds one plan per uploaded snapshot set: an EXTRACT of the first function
// into its parent file's directory, plus a NO_CHANGE refusal. Fixed form, no analysis.
func (s *Server) recommend(ns string) map[string]any {
	snaps := s.snapshots(ns)
	if len(snaps) == 0 {
		return nil
	}
	last := snaps[len(snaps)-1]
	p := last["payload"].(map[string]any)
	var repoIDs []any
	var snapRefs []any
	for _, x := range p["snapshots"].([]any) {
		sm := x.(map[string]any)
		repoIDs = append(repoIDs, sm["repo_id"])
		snapRefs = append(snapRefs, map[string]any{"repo_id": sm["repo_id"], "snapshot_id": sm["snapshot_id"], "digest": sm["digest"]})
	}
	var fn, file, dir string
	parents := map[string]string{}
	kinds := map[string]string{}
	for _, x := range p["entities"].([]any) {
		e := x.(map[string]any)
		id := e["entity_id"].(string)
		kinds[id] = e["kind"].(string)
		if pid, ok := e["parent_id"].(string); ok {
			parents[id] = pid
		}
	}
	ids := make([]string, 0, len(kinds))
	for id := range kinds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if kinds[id] == "function" && fn == "" {
			fn = id
		}
	}
	if fn == "" {
		return nil
	}
	file = parents[fn]
	dir = parents[file]
	created, expires := "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z"
	plan := map[string]any{
		"plan_id": hexID("plan", ns, fn), "plan_schema": "0.1.0", "project_namespace": ns,
		"repo_ids": repoIDs, "snapshots": snapRefs, "intervention_class": "REFACTOR_CODE", "transformation": "EXTRACT",
		"decision": "recommend", "targets": []any{fn, file},
		"preconditions": []any{map[string]any{"check": "snapshot_matches"}, map[string]any{"check": "tests_pass"}},
		"steps": []any{map[string]any{"step_id": 1, "action": "EXTRACT", "targets": []any{fn}, "destination": dir,
			"depends_on": []any{}, "requires_approval": true, "support": "supported"},
			map[string]any{"step_id": 2, "action": "RUN_CHECKS", "targets": []any{file}, "depends_on": []any{1},
				"requires_approval": false, "support": "supported"}},
		"reasons": []any{map[string]any{"code": "context_size"}},
		"predictions": []any{map[string]any{"class": "forecast", "metric": "context_bytes", "unit": "bytes",
			"per": "task", "interval": map[string]any{"lower": -900, "upper": -100, "level": 0.8}}},
		"simulation":         map[string]any{"level": "L1", "calibration_status": "uncalibrated", "out_of_distribution": true},
		"risk_constraints":   []any{map[string]any{"kind": "test", "requirement": "tests_unchanged_pass", "deterministic": true}},
		"required_approvals": []any{"owner"}, "created_at": created, "expires_at": expires,
	}
	refuse := map[string]any{
		"plan_id": hexID("plan", ns, "no-change"), "plan_schema": "0.1.0", "project_namespace": ns,
		"repo_ids": repoIDs, "snapshots": snapRefs, "intervention_class": "NO_CHANGE", "transformation": nil,
		"decision": "refuse_uncertain", "targets": []any{}, "preconditions": []any{}, "steps": []any{},
		"reasons": []any{map[string]any{"code": "weak_evidence"}}, "risk_constraints": []any{},
		"required_approvals": []any{}, "created_at": created, "expires_at": expires,
	}
	doc := envelope("recommendation_set", ns, repoIDs, []string{"Plan"}, map[string]any{"plans": []any{plan, refuse}})
	if s.Mutate != nil {
		s.Mutate(doc)
		reseal(doc)
	}
	return doc
}

func hexID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func envelope(docType, ns string, repos []any, types []string, payload map[string]any) map[string]any {
	if repos == nil {
		repos = []any{}
	}
	rt := []any{}
	for _, t := range types {
		rt = append(rt, t)
	}
	d := map[string]any{"hint_version": "0.1.0", "document_type": docType, "document_id": hexID(docType, ns),
		"project_namespace": ns, "repositories": repos,
		"producer":   map[string]any{"tool": "hint-server", "version": "0.1.0"},
		"created_at": "2026-01-01T00:00:00Z",
		"redaction": map[string]any{"policy_id": hexID("policy"), "profile": "detailed", "record_types": rt,
			"free_text": "denied", "time_resolution_seconds": 0},
		"payload": payload}
	reseal(d)
	return d
}

// Reseal recomputes the content digest of a document map.
func reseal(d map[string]any) {
	b, _ := json.Marshal(d["payload"])
	var plain any
	_ = json.Unmarshal(b, &plain)
	dg, _ := hint.Digest(plain)
	d["content_digest"] = dg
}

func write(w http.ResponseWriter, status int, d map[string]any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(d)
}

// SetFail switches the simulated outage on or off.
func (s *Server) SetFail(v bool) {
	s.mu.Lock()
	s.Fail = v
	s.mu.Unlock()
}

// Posted returns a copy of all POST bodies.
func (s *Server) Posted() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte{}, s.Requests...)
}

// RecommendationFor returns the recommendation the server would send for a namespace.
func (s *Server) RecommendationFor(ns string) []byte {
	d := s.recommend(ns)
	if d == nil {
		return nil
	}
	b, _ := json.Marshal(d)
	return b
}
