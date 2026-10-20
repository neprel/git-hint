package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter counts the model requests of one trial, stops them at the limit and keeps the
// trial's model ledger (ledger.go).
type Counter struct {
	ledger   ledger
	token    string
	limit    int64
	requests atomic.Int64
	retries  atomic.Int64
	hit      atomic.Bool
	// the model field of every forwarded request against the selected model
	matched, mismatched, unverified atomic.Int64
}

// ModelCheck says whether the trial used the selected model: verified (every forwarded
// request named it), mismatch (a request named another model: a runtime switch, subagent or
// fallback), unverified (a request without a readable model field) or not_observed (no
// request). Only verified supports an effective model with provenance launch_parameter.
func (c *Counter) ModelCheck() string {
	switch {
	case c.mismatched.Load() > 0:
		return "mismatch"
	case c.unverified.Load() > 0:
		return "unverified"
	case c.matched.Load() > 0:
		return "verified"
	}
	return "not_observed"
}

func (c *Counter) Requests() int64 { return c.requests.Load() }
func (c *Counter) Retries() int64  { return c.retries.Load() }
func (c *Counter) LimitHit() bool  { return c.hit.Load() }

// Live is a running model proxy for live agent trials. It forwards to the LAN model
// endpoint of the eval policy, injects the key (the container never sees it), counts
// requests per trial and answers 429 beyond the trial's limit.
type Live struct {
	ContainerHost string // host name the container uses (host.docker.internal)
	AgentVersion  string // OpenCode version installed in the container
	ModelLabel    string // model name recorded in trials
	model         string
	port          int
	srv           *http.Server
	mu            sync.Mutex
	trials        map[string]*Counter
	upstream      *url.URL
	key           string
	log           func(string)
}

// LiveConfig describes the LAN model.
type LiveConfig struct {
	BaseURL      string // http://<private address>/v1
	Model        string
	KeyEnv       string
	AgentVersion string
	Listen       string // address the proxy listens on (default 127.0.0.1:0)
}

// StartLive starts the proxy. The key is read from the environment variable named in the
// policy; it is never written to arguments, files or logs.
func StartLive(cfg LiveConfig) (*Live, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("the LAN model URL is not usable")
	}
	key := os.Getenv(cfg.KeyEnv)
	if key == "" {
		return nil, fmt.Errorf("the environment variable %s with the LAN model key is not set", cfg.KeyEnv)
	}
	listen := cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	lv := &Live{ContainerHost: "host.docker.internal", AgentVersion: cfg.AgentVersion, ModelLabel: cfg.Model, model: cfg.Model,
		port: ln.Addr().(*net.TCPAddr).Port, trials: map[string]*Counter{}, upstream: u, key: key, log: func(string) {}}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = u.Host
			r.Out.Header.Set("Authorization", "Bearer "+lv.key)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { w.WriteHeader(http.StatusBadGateway) },
	}
	lv.srv = &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path: /t/<token>/<upstream path below the base URL>
		rest, ok := strings.CutPrefix(r.URL.Path, "/t/")
		token, sub, _ := strings.Cut(rest, "/")
		lv.mu.Lock()
		c := lv.trials[token]
		lv.mu.Unlock()
		if !ok || c == nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.HasSuffix(sub, "models") && r.Method == http.MethodGet {
			// model lists are answered locally: no upstream list download
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": lv.model, "object": "model"}}})
			return
		}
		e, refused := c.admit() // atomic against the seal and the limit
		if e == nil {
			w.WriteHeader(refused)
			return
		}
		c.setModel(e, lv.checkModel(c, r))
		r.URL.Path, r.URL.RawPath = "/"+sub, "" // SetURL joins the base path
		r.Header.Del("Authorization")
		rec := &usageRecorder{ResponseWriter: w}
		defer func() {
			// the reverse proxy aborts with http.ErrAbortHandler when the agent goes away
			// mid-response: the entry is cancelled (what arrived is kept), then the abort goes on
			if p := recover(); p != nil {
				_, u := rec.result()
				c.finish(e, EntryCancelled, u)
				panic(p)
			}
		}()
		rp.ServeHTTP(rec, r)
		state, u := rec.result()
		switch {
		case r.Context().Err() != nil:
			state = EntryCancelled // the agent went away; what arrived is kept
		case !rec.started:
			state = EntryCancelled
		case rec.status < 200 || rec.status > 299:
			state = EntryError
		}
		c.finish(e, state, u)
	})}
	go lv.srv.Serve(ln)
	return lv, nil
}

// Close stops the proxy.
func (lv *Live) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return lv.srv.Shutdown(ctx)
}

// NewTrial registers a trial with its request limit.
func (lv *Live) NewTrial(limit int) *Counter {
	b := make([]byte, 12)
	rand.Read(b)
	c := &Counter{token: hex.EncodeToString(b), limit: int64(limit)}
	lv.mu.Lock()
	lv.trials[c.token] = c
	lv.mu.Unlock()
	return c
}

// BaseURL is the model URL that the container uses for one trial.
func (lv *Live) BaseURL(c *Counter) string {
	return fmt.Sprintf("http://%s:%d/t/%s", lv.ContainerHost, lv.port, c.token)
}

// AgentEnv is the runner environment for one trial: the proxy URL and a dummy key (the
// real key stays in the proxy).
func (lv *Live) AgentEnv(c *Counter) []string {
	return []string{"OPENAI_BASE_URL=" + lv.BaseURL(c), "OPENAI_API_KEY=hint-proxy"}
}

// ErrNoLive is returned when a live trial is requested without a proxy.
var ErrNoLive = errors.New("live trials need the model proxy")

// maxCheckedBody bounds the request body that the model check reads (larger bodies are
// forwarded unread and count as unverified).
const maxCheckedBody = 32 << 20

// checkModel reads the model field of a forwarded request (the body is restored unchanged),
// counts it against the selected model and returns the request's label. A verified label means
// the request named the selected model, not that the upstream served those weights.
func (lv *Live) checkModel(c *Counter, r *http.Request) string {
	if r.Body == nil || r.Method != http.MethodPost {
		c.unverified.Add(1)
		return "unverified"
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxCheckedBody+1))
	rest := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), rest), rest}
	if err != nil || len(b) > maxCheckedBody {
		c.unverified.Add(1)
		return "unverified"
	}
	model, ok := requestModel(b)
	if !ok {
		c.unverified.Add(1)
		return "unverified"
	}
	if model == lv.model {
		c.matched.Add(1)
		return "verified"
	}
	c.mismatched.Add(1)
	return "mismatch"
}

// requestModel returns the model of a request body only when it is unambiguous: the body is
// one JSON object whose top level holds exactly one key "model" (exact, lowercase) with a
// string value, and no other key that equals "model" ignoring case (Go would match such an
// alias, the upstream would not). Anything else is not a verifiable selection.
func requestModel(b []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", false
	}
	var model string
	found := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", false
		}
		if !strings.EqualFold(key, "model") {
			continue
		}
		found++
		var m *string
		if key != "model" || json.Unmarshal(v, &m) != nil || m == nil {
			return "", false // a case alias, a non-string or a null value
		}
		model = *m
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') || found != 1 {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false // trailing data after the object
	}
	return model, true
}
