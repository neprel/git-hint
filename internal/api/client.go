// Package api is the HTTP client of the HINT exchange binding (exchange-v0.1 §3). The API
// carries the same documents as the file mode; it is a transport, not a second format.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// MediaType is the HINT document media type.
const MediaType = "application/vnd.hint+json; version=0.1"

// MaxResponseBytes limits every response body.
const MaxResponseBytes = 16 << 20

// Client talks to one HINT server.
type Client struct {
	base  *url.URL
	http  *http.Client
	token string
}

// ErrNetwork marks a transport failure: the package can be sent again later.
var ErrNetwork = errors.New("network failure")

// New checks the server base URL and returns a client. The URL must use https, except a
// loopback host (local tests). User information, a query and a fragment are refused.
// An optional bearer token comes from HINT_SERVER_TOKEN; it is never stored by HINT.
func New(raw string) (*Client, error) {
	if raw == "" {
		return nil, errors.New("no server address; set it with `git hint init --server <url>` or HINT_SERVER_URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("server address: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return nil, errors.New("server address must be scheme://host[:port][/path] without credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, errors.New("server address must use https (http is allowed for a loopback host only)")
		}
	default:
		return nil, fmt.Errorf("server address scheme %q is not supported", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &Client{base: u, token: os.Getenv("HINT_SERVER_TOKEN"), http: &http.Client{
		Timeout: 60 * time.Second,
		// a redirect could move documents to another origin; follow none
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func loopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Origin returns scheme://host of the server.
func (c *Client) Origin() string { return c.base.Scheme + "://" + c.base.Host }

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	u := *c.base
	p, q, _ := strings.Cut(path, "?")
	u.Path += p
	u.RawQuery = q
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", MediaType)
	if body != nil {
		req.Header.Set("Content-Type", MediaType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrNetwork, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrNetwork, err)
	}
	if len(b) > MaxResponseBytes {
		return resp.StatusCode, nil, errors.New("response too large")
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// the server is unavailable or overloaded: retryable, like a transport failure
		return resp.StatusCode, nil, fmt.Errorf("%w: server status %d", ErrNetwork, resp.StatusCode)
	}
	if len(b) > 0 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/vnd.hint+json") {
		return resp.StatusCode, nil, fmt.Errorf("server answered %d with content type %q, not a HINT document", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, b, nil
}

// Receipt is the result of one document upload.
type Receipt struct {
	Accepted bool
	Code     string // rejection code when not accepted
}

// PostDocument sends one document and checks the receipt.
func (c *Client) PostDocument(ctx context.Context, doc []byte, documentID string) (Receipt, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/hint/v0.1/documents", doc)
	if err != nil {
		return Receipt{}, err
	}
	if status >= 500 {
		return Receipt{}, fmt.Errorf("%w: server status %d", ErrNetwork, status)
	}
	if status != http.StatusCreated && status != http.StatusUnprocessableEntity {
		return Receipt{}, fmt.Errorf("unexpected server status %d", status)
	}
	rc, err := parseReceipt(body)
	if err != nil {
		return Receipt{}, err
	}
	for _, a := range list(rc["accepted"]) {
		if m, _ := a.(map[string]any); m["document_id"] == documentID && status == http.StatusCreated {
			return Receipt{Accepted: true}, nil
		}
	}
	for _, r := range list(rc["rejected"]) {
		m, _ := r.(map[string]any)
		if id, _ := m["document_id"].(string); id == documentID || id == "" {
			code, _ := m["code"].(string)
			return Receipt{Code: code}, nil
		}
	}
	return Receipt{}, errors.New("the receipt does not name the document")
}

func parseReceipt(b []byte) (map[string]any, error) {
	sch, err := hint.LoadSchemas()
	if err != nil {
		return nil, err
	}
	dec, err := hint.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("receipt is not JSON: %v", err)
	}
	if err := sch.Validate(dec); err != nil {
		return nil, fmt.Errorf("receipt is not a valid HINT document: %v", err)
	}
	doc, err := hint.Plain(dec)
	if err != nil {
		return nil, err
	}
	if doc["document_type"] != "receipt" {
		return nil, errors.New("server answer is not a receipt")
	}
	p, _ := doc["payload"].(map[string]any)
	return p, nil
}

// Recommendations fetches the recommendation_set of a project namespace. The caller
// validates it with the same import path as a file.
func (c *Client) Recommendations(ctx context.Context, namespace string) ([]byte, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/hint/v0.1/recommendations?project_namespace="+url.QueryEscape(namespace), nil)
	if err != nil {
		return nil, err
	}
	if status >= 500 {
		return nil, fmt.Errorf("%w: server status %d", ErrNetwork, status)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("unexpected server status %d", status)
	}
	return body, nil
}

func list(v any) []any { l, _ := v.([]any); return l }
