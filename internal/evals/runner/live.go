package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenCodeVersion is the OpenCode release that Harbor installs in the task container
// (npm opencode-ai, released 2026-09-30, MIT). Harbor installs agents from npm only; the
// newest npm release at least two weeks old is pinned. It can differ from the user's own
// OpenCode, and trials record it.
const OpenCodeVersion = "1.18.34"

// Network modes of an exported task.
const (
	Offline  = ""          // no network at run time
	LANModel = "lan-model" // the agent reaches only HINT's model proxy
)

// Counter counts the model requests of one trial and stops them at the limit.
type Counter struct {
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
		if c.requests.Load() >= c.limit {
			c.hit.Store(true)
			c.retries.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		c.requests.Add(1)
		lv.checkModel(c, r)
		r.URL.Path, r.URL.RawPath = "/"+sub, "" // SetURL joins the base path
		r.Header.Del("Authorization")
		rp.ServeHTTP(w, r)
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

// AgentArgs are the Harbor --ak values for OpenCode: pinned version and a configuration
// overlay without auto-update or sharing.
func (lv *Live) AgentArgs(c *Counter) []string {
	cfg, _ := json.Marshal(map[string]any{"autoupdate": false, "share": "disabled"})
	return []string{"version=" + lv.AgentVersion, "opencode_config=" + string(cfg)}
}

// AgentEnv is the runner environment for one trial: the proxy URL and a dummy key (the
// real key stays in the proxy).
func (lv *Live) AgentEnv(c *Counter) []string {
	return []string{"OPENAI_BASE_URL=" + lv.BaseURL(c), "OPENAI_API_KEY=hint-proxy"}
}

// Model is the Harbor model argument.
func (lv *Live) Model() string { return "openai/" + lv.model }

// ErrNoLive is returned when a live trial is requested without a proxy.
var ErrNoLive = errors.New("live trials need the model proxy")


// LaunchDigest is the digest of the effective launch configuration of a live trial: the agent
// arguments, the model selection and the names of the environment variables (values such as
// the per-trial proxy URL and the dummy key are not configuration and never enter it).
func LaunchDigest(ro RunOptions) string {
	var names []string
	for _, e := range ro.Env {
		n, _, _ := strings.Cut(e, "=")
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(map[string]any{"agent": ro.Agent, "agent_args": ro.AgentArgs, "env_names": names, "model": ro.Model})
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}


// maxCheckedBody bounds the request body that the model check reads (larger bodies are
// forwarded unread and count as unverified).
const maxCheckedBody = 32 << 20

// checkModel reads the model field of a forwarded request (the body is restored unchanged)
// and counts it against the selected model.
func (lv *Live) checkModel(c *Counter, r *http.Request) {
	if r.Body == nil || r.Method != http.MethodPost {
		c.unverified.Add(1)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxCheckedBody+1))
	rest := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), rest), rest}
	if err != nil || len(b) > maxCheckedBody {
		c.unverified.Add(1)
		return
	}
	model, ok := requestModel(b)
	if !ok {
		c.unverified.Add(1)
		return
	}
	if model == lv.model {
		c.matched.Add(1)
	} else {
		c.mismatched.Add(1)
	}
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
		if key != "model" || json.Unmarshal(v, &model) != nil {
			return "", false // a case alias or a non-string value
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') || found != 1 {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false // trailing data after the object
	}
	return model, true
}
