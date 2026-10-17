// Package model calls the optional local model of the eval policy (an OpenAI-compatible
// endpoint on a private network address) under a journaled per-job budget. It never falls
// back to another endpoint, never retries beyond the budget, and treats every answer as
// untrusted text.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
)

// Client is a budgeted chat client for one job.
type Client struct {
	base    *url.URL
	model   string
	key     string
	lib     *library.Library
	job     string
	http    *http.Client
	retries int
}

// ErrBudget is returned when the job has no budget left.
var ErrBudget = errors.New("the local model budget of this job is used up")

// New starts a job with the policy's limits. The key comes from the environment variable
// named in the policy; it is never stored or logged.
func New(p *evals.Policy, l *library.Library, job string) (*Client, error) {
	if err := p.Check(evals.OpModel, ""); err != nil {
		return nil, err
	}
	u, err := url.Parse(p.Model.BaseURL)
	if err != nil || u.Scheme != "http" {
		return nil, errors.New("the local model URL is not usable")
	}
	key := os.Getenv(p.Model.KeyEnv)
	if key == "" {
		return nil, fmt.Errorf("the environment variable %s with the local model key is not set", p.Model.KeyEnv)
	}
	lim := p.Limits()
	if err := l.StartJob(job, "model", library.Budget{Requests: lim.ModelRequests, Retries: lim.ModelRetries, Seconds: lim.ModelSeconds}); err != nil {
		return nil, err
	}
	return &Client{base: u, model: p.Model.Model, key: key, lib: l, job: job, retries: lim.ModelRetries,
		http: &http.Client{Timeout: time.Duration(lim.ModelSeconds) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close ends the job.
func (c *Client) Close() error { return c.lib.EndJob(c.job, "done") }

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Chat sends one request (plus retries within the budget) and returns the answer text.
// Every attempt is journaled before it is sent.
func (c *Client) Chat(ctx context.Context, msgs []Message, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": c.model, "messages": msgs, "temperature": 0, "max_tokens": maxTokens, "stream": false})
	for attempt := 0; ; attempt++ {
		st, err := c.lib.Job(c.job)
		if err != nil {
			return "", err
		}
		if ok, _ := st.Remaining(); !ok {
			return "", ErrBudget
		}
		retry := 0
		if attempt > 0 {
			retry = 1
		}
		if err := c.lib.Consume(c.job, 1, retry, 0, 0); err != nil {
			return "", ErrBudget
		}
		t0 := time.Now()
		text, retryable, err := c.once(ctx, body)
		c.lib.Consume(c.job, 0, 0, 0, time.Since(t0).Seconds())
		if err == nil {
			return text, nil
		}
		if !retryable || attempt >= c.retries {
			return "", err
		}
	}
}

func (c *Client) once(ctx context.Context, body []byte) (string, bool, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", true, errors.New("the local model did not answer")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", true, fmt.Errorf("the local model answered %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("the local model answered %d", resp.StatusCode)
	}
	var r struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Choices) == 0 {
		return "", false, errors.New("the local model answer has no choice")
	}
	return r.Choices[0].Message.Content, false, nil
}
