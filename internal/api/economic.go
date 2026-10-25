package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxEconomicResponseBytes = 2 << 20

var economicInteger = regexp.MustCompile(`^[1-9][0-9]{0,6}$`)
var economicDecimal = regexp.MustCompile(`^(0\.[0-9]{1,6}|1(\.0{1,6})?)$`)
var economicRef = regexp.MustCompile(`^[0-9a-f]{32}$`)
var economicReason = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
var errEconomic = errors.New("invalid or unsupported economic assessment")

// EconomicPolicy supplies preferences only. It supplies no evidence or default horizon.
type EconomicPolicy struct {
	Horizon   int
	Threshold string
}

func NewEconomicPolicy(horizon, threshold string) (EconomicPolicy, error) {
	if !economicInteger.MatchString(horizon) || !economicDecimal.MatchString(threshold) {
		return EconomicPolicy{}, errors.New("economic horizon must be 1-1000000; threshold must be a decimal in (0,1] with at most six fraction digits")
	}
	h, _ := strconv.Atoi(horizon)
	p, _ := strconv.ParseFloat(threshold, 64)
	if h > 1000000 || p <= 0 {
		return EconomicPolicy{}, errEconomic
	}
	return EconomicPolicy{h, threshold}, nil
}
func (p EconomicPolicy) wire() map[string]any {
	threshold, _ := strconv.ParseFloat(p.Threshold, 64)
	return map[string]any{"version": "economic-policy-v1", "objective": "canonical_model_tokens", "horizon": json.Number(strconv.Itoa(p.Horizon)), "horizon_unit": "accepted_completion", "threshold": threshold, "risk": "conservative_cost_bounds", "correctness_guard": "paired_accepted_fraction_lower_bound_nonnegative"}
}
func (p EconomicPolicy) query() (url.Values, error) {
	checked, e := NewEconomicPolicy(strconv.Itoa(p.Horizon), p.Threshold)
	if e != nil || checked != p {
		return nil, errEconomic
	}
	q := url.Values{}
	for k, v := range p.wire() {
		q.Set("economic_"+k, strings.TrimSpace(toEconomicString(v)))
	}
	q.Set("economic_threshold", p.Threshold)
	return q, nil
}
func toEconomicString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// EconomicAssessment is a descriptive sidecar projection, never an importable HINT plan.
type EconomicAssessment struct {
	Policy           EconomicPolicy
	CanonicalTokens  *int64
	Coverage         [2]int64
	Attempts         *int64
	CounterSemantics string
	Reasons          []string
}

// EconomicRecommendations uses the normal authenticated transport with a distinct JSON media type.
func (c *Client) EconomicRecommendations(ctx context.Context, ns string, p EconomicPolicy) (*EconomicAssessment, error) {
	q, e := p.query()
	if e != nil {
		return nil, e
	}
	if !economicRef.MatchString(ns) {
		return nil, errEconomic
	}
	q.Set("project_namespace", ns)
	status, raw, e := c.doMedia(ctx, http.MethodGet, "/hint/v0.1/recommendations?"+q.Encode(), nil, "application/json", "application/json")
	if e != nil {
		return nil, e
	}
	if status != http.StatusOK {
		return nil, errors.New("the server refused the economic request")
	}
	return ParseEconomicAssessment(raw, ns, p)
}

// economicJSON rejects duplicates and bounds syntax before any semantic projection.
func economicJSON(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > MaxEconomicResponseBytes || !utf8.Valid(raw) {
		return nil, errEconomic
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		nodes++
		if depth > 32 || nodes > 32768 {
			return nil, errEconomic
		}
		t, e := d.Token()
		if e != nil {
			return nil, errEconomic
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					s, ok := k.(string)
					if e != nil || !ok || len(s) > 256 {
						return nil, errEconomic
					}
					if _, yes := m[s]; yes {
						return nil, errEconomic
					}
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					m[s] = v
				}
				t, e = d.Token()
				if e != nil || t != json.Delim('}') {
					return nil, errEconomic
				}
				return m, nil
			case '[':
				a := []any{}
				for d.More() {
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				t, e = d.Token()
				if e != nil || t != json.Delim(']') {
					return nil, errEconomic
				}
				return a, nil
			}
			return nil, errEconomic
		}
		if s, ok := t.(string); ok && len(s) > 16384 {
			return nil, errEconomic
		}
		return t, nil
	}
	v, e := read(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errEconomic
	}
	return v, nil
}
func economicObject(v any, keys string) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	ks := strings.Fields(keys)
	if len(m) != len(ks) {
		return nil, false
	}
	for _, k := range ks {
		if _, yes := m[k]; !yes {
			return nil, false
		}
	}
	return m, true
}
func economicInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, e := strconv.ParseInt(string(n), 10, 64)
	return i, e == nil && i >= 0
}
func economicPolicyEqual(v any, p EconomicPolicy) bool {
	m, ok := economicObject(v, "version objective horizon horizon_unit threshold risk correctness_guard")
	if !ok {
		return false
	}
	expected := p.wire()
	for k, want := range expected {
		if k == "threshold" {
			n, ok := m[k].(json.Number)
			if !ok {
				return false
			}
			f, e := strconv.ParseFloat(string(n), 64)
			if e != nil || f != want {
				return false
			}
		} else if !reflect.DeepEqual(m[k], want) {
			return false
		}
	}
	return true
}

// ParseEconomicAssessment binds the sidecar to the exact namespace and explicit preferences.
// Opaque bounded provenance subtrees are discarded, not interpreted as authority or a plan.
func ParseEconomicAssessment(raw []byte, ns string, p EconomicPolicy) (*EconomicAssessment, error) {
	if _, e := p.query(); e != nil || !economicRef.MatchString(ns) {
		return nil, errEconomic
	}
	v, e := economicJSON(raw)
	if e != nil {
		return nil, e
	}
	top, ok := economicObject(v, "recommendation_document_id evidence_id request_id economic_assessment")
	if !ok || top["recommendation_document_id"] != nil {
		return nil, errEconomic
	}
	for _, k := range []string{"evidence_id", "request_id"} {
		s, ok := top[k].(string)
		if !ok || !economicRef.MatchString(s) {
			return nil, errEconomic
		}
	}
	a, ok := economicObject(top["economic_assessment"], "version policy chosen product_ready alternatives reasons observed_baseline as_of")
	if !ok || a["version"] != "economic-request-v1" || a["chosen"] != "COLLECT_MORE_EVIDENCE" || a["product_ready"] != false || !economicPolicyEqual(a["policy"], p) {
		return nil, errEconomic
	}
	alts, ok := a["alternatives"].([]any)
	if !ok || len(alts) > 5 {
		return nil, errEconomic
	}
	for _, v := range alts {
		if _, ok := v.(map[string]any); !ok {
			return nil, errEconomic
		}
	}
	out := &EconomicAssessment{Policy: p}
	reasons, ok := a["reasons"].([]any)
	if !ok || len(reasons) > 256 {
		return nil, errEconomic
	}
	for _, v := range reasons {
		s, ok := v.(string)
		if !ok || !economicReason.MatchString(s) {
			return nil, errEconomic
		}
		out.Reasons = append(out.Reasons, s)
	}
	b, ok := economicObject(a["observed_baseline"], "label cost_population_scope counter_semantics canonical_tokens coverage attempts whole_project_cost effect")
	if !ok || b["label"] != "observed_received_task_usage" || b["cost_population_scope"] != "received_confirmed_tasks_all_attempts" || b["whole_project_cost"] != nil || b["effect"] != nil {
		return nil, errEconomic
	}
	cov, ok := b["coverage"].([]any)
	if !ok || len(cov) != 2 {
		return nil, errEconomic
	}
	for i := range cov {
		n, ok := economicInt(cov[i])
		if !ok {
			return nil, errEconomic
		}
		out.Coverage[i] = n
	}
	if out.Coverage[0] > out.Coverage[1] {
		return nil, errEconomic
	}
	if b["attempts"] != nil {
		n, valid := economicInt(b["attempts"])
		if !valid || n < out.Coverage[1] {
			return nil, errEconomic
		}
		out.Attempts = &n
	}
	out.CounterSemantics, ok = b["counter_semantics"].(string)
	if !ok {
		return nil, errEconomic
	}
	if b["canonical_tokens"] != nil {
		n, ok := economicInt(b["canonical_tokens"])
		if !ok || out.CounterSemantics != "hint-task-tokens-v1" || out.Coverage[0] != out.Coverage[1] || out.Coverage[0] == 0 || out.Attempts == nil || *out.Attempts < out.Coverage[1] {
			return nil, errEconomic
		}
		out.CanonicalTokens = &n
	} else if out.CounterSemantics != "unknown" || (out.Coverage[0] > 0 && out.Coverage[0] == out.Coverage[1]) {
		return nil, errEconomic
	}
	bind, ok := economicObject(a["as_of"], "version policy namespace evidence_id input_watermark input_digest usage_view producer_identity producer_seals code config variant_pairs candidates finding_scopes")
	if !ok || bind["version"] != "economic-request-v1" || bind["namespace"] != ns || bind["evidence_id"] != top["evidence_id"] || !economicPolicyEqual(bind["policy"], p) {
		return nil, errEconomic
	}
	for _, k := range []string{"input_digest", "usage_view", "producer_identity"} {
		if s, ok := bind[k].(string); !ok || s == "" {
			return nil, errEconomic
		}
	}
	watermark, ok := economicObject(bind["input_watermark"], "documents reports")
	if !ok {
		return nil, errEconomic
	}
	for _, k := range []string{"documents", "reports"} {
		if _, ok := economicInt(watermark[k]); !ok {
			return nil, errEconomic
		}
	}
	for _, k := range []string{"producer_seals", "variant_pairs", "candidates", "finding_scopes"} {
		if _, ok := bind[k].([]any); !ok {
			return nil, errEconomic
		}
	}
	for _, k := range []string{"code", "config"} {
		if _, ok := bind[k].(map[string]any); !ok {
			return nil, errEconomic
		}
	}
	return out, nil
}
