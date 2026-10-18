// Package api is the HTTP client of the HINT exchange binding (exchange-v0.1 §3). The API
// carries the same documents as the file mode; it is a transport, not a second format.
package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	return c.doMedia(ctx, method, path, body, MediaType, "application/vnd.hint+json")
}

func (c *Client) doMedia(ctx context.Context, method, path string, body []byte, media, answerPrefix string) (int, []byte, error) {
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
	req.Header.Set("Accept", media)
	if body != nil {
		req.Header.Set("Content-Type", media)
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
	if len(b) > 0 && !strings.HasPrefix(resp.Header.Get("Content-Type"), answerPrefix) {
		return resp.StatusCode, nil, fmt.Errorf("server answered %d with content type %q, not a HINT document", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, b, nil
}

// Receipt is the result of one document upload.
type Receipt struct {
	Accepted bool
	Code     string // rejection code when not accepted
}

// PostDocument sends one document and checks the receipt. A document counts as accepted
// only when the server answers 201 with a valid receipt (schema and semantic rules,
// including its own content digest) whose accepted entry names the sent document ID and
// the sent content digest.
func (c *Client) PostDocument(ctx context.Context, doc []byte, documentID, digest string) (Receipt, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/hint/v0.1/documents", doc)
	if err != nil {
		return Receipt{}, err
	}
	if status != http.StatusCreated && status != http.StatusUnprocessableEntity {
		return Receipt{}, fmt.Errorf("unexpected server status %d", status)
	}
	rc, err := parseReceipt(body)
	if err != nil {
		return Receipt{}, err
	}
	if status == http.StatusCreated {
		for _, a := range list(rc["accepted"]) {
			m, _ := a.(map[string]any)
			if m["document_id"] != documentID {
				continue
			}
			if m["content_digest"] != digest {
				return Receipt{}, fmt.Errorf("the receipt confirms another content digest for document %s", documentID)
			}
			return Receipt{Accepted: true}, nil
		}
		return Receipt{}, fmt.Errorf("the receipt does not confirm document %s", documentID)
	}
	for _, r := range list(rc["rejected"]) {
		m, _ := r.(map[string]any)
		if id, _ := m["document_id"].(string); id == documentID || m["document_id"] == nil {
			code, _ := m["code"].(string)
			return Receipt{Code: code}, nil
		}
	}
	return Receipt{}, errors.New("the rejection receipt does not name the document")
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
	if f := hint.Check(doc, nil); len(f) > 0 {
		return nil, fmt.Errorf("receipt fails the semantic rules: %s", f[0].String())
	}
	p, _ := doc["payload"].(map[string]any)
	return p, nil
}

// Recommendations fetches the recommendation_set of a project namespace. The caller
// validates it with the same import path as a file.
func (c *Client) Recommendations(ctx context.Context, namespace string) ([]byte, error) {
	return c.RecommendationsWith(ctx, namespace, nil)
}

// RecommendationsWith adds the optional forecast request policy (typed query parameters,
// validated by the caller) to the recommendation request. A refused policy is an error with
// the server's reason; nothing falls back to a request without the policy.
func (c *Client) RecommendationsWith(ctx context.Context, namespace string, policy url.Values) ([]byte, error) {
	q := url.Values{"project_namespace": {namespace}}
	for k, v := range policy {
		q[k] = v
	}
	status, body, err := c.do(ctx, http.MethodGet, "/hint/v0.1/recommendations?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest {
		var e struct{ Error, Parameter, Detail string }
		if json.Unmarshal(body, &e) == nil && e.Error == "invalid_forecast_policy" {
			return nil, fmt.Errorf("the server refused the forecast policy: %s: %s", e.Parameter, e.Detail)
		}
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

// EvalReportMediaType is the media type of eval result reports.
const EvalReportMediaType = "application/vnd.hint-evalreport+json"

// EvalReportReceipt is the server's answer to a report upload.
type EvalReportReceipt struct {
	ReportVersion string `json:"report_version"`
	ReportID      string `json:"report_id"`
	ContentDigest string `json:"content_digest"`
	Status        string `json:"status"` // accepted, same, rejected
	Code          string `json:"code,omitempty"`
}

// PostEvalReport sends one eval result report. It counts as accepted only when the server
// answers 201 (new) or 200 (the same report again) naming the sent report ID and digest.
func (c *Client) PostEvalReport(ctx context.Context, body []byte, reportID, digest string) (EvalReportReceipt, error) {
	status, b, err := c.doMedia(ctx, http.MethodPost, "/hint/evalreport/v0.1/reports", body, EvalReportMediaType, EvalReportMediaType)
	if err != nil {
		return EvalReportReceipt{}, err
	}
	var r EvalReportReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return EvalReportReceipt{}, fmt.Errorf("the server answered %d without a report receipt", status)
	}
	switch {
	case status == http.StatusUnprocessableEntity || r.Status == "rejected":
		return r, fmt.Errorf("the server rejected the report: %s", r.Code)
	case (status == http.StatusCreated && r.Status == "accepted" || status == http.StatusOK && r.Status == "same") &&
		r.ReportID == reportID && r.ContentDigest == digest:
		return r, nil
	}
	return r, fmt.Errorf("the server answer did not confirm the report (status %d)", status)
}
