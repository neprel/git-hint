package api

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const economicNS = "11111111111111111111111111111111"

// Independent synthetic source-shape fixture. It does not represent measured evidence.
//
//go:embed testdata/economic-sidecar.synthetic.json
var economicFixture string

func economicP(t *testing.T) EconomicPolicy {
	t.Helper()
	p, e := NewEconomicPolicy("10", "0.95")
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestEconomicSidecarStrictProjection(t *testing.T) {
	p := economicP(t)
	a, e := ParseEconomicAssessment([]byte(economicFixture), economicNS, p)
	if e != nil || a == nil || a.CanonicalTokens == nil || *a.CanonicalTokens != 37 || a.Attempts != 2 {
		t.Fatalf("%+v %v", a, e)
	}
	unknown := strings.ReplaceAll(economicFixture, `"canonical_tokens":37`, `"canonical_tokens":null`)
	unknown = strings.ReplaceAll(unknown, `"counter_semantics":"hint-task-tokens-v1"`, `"counter_semantics":"unknown"`)
	unknown = strings.ReplaceAll(unknown, `"coverage":[1,1]`, `"coverage":[0,1]`)
	if a, e := ParseEconomicAssessment([]byte(unknown), economicNS, p); e != nil || a.CanonicalTokens != nil || a.Coverage != [2]int64{0, 1} {
		t.Fatalf("unknown: %+v %v", a, e)
	}
	changes := [][2]string{
		{`"horizon_unit":"accepted_completion"`, `"horizon_unit":"attempted_task"`},
		{`"risk":"conservative_cost_bounds"`, `"risk":"optimistic"`},
		{`"correctness_guard":"paired_accepted_fraction_lower_bound_nonnegative"`, `"correctness_guard":"none"`},
		{`"objective":"canonical_model_tokens"`, `"objective":"wall_time"`},
		{`"code":{}`, `"code":{"x":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + `}`},
		{`"code":{}`, `"code":{"x":[` + strings.Repeat("0,", 33000) + `0]}`},
		{`"code":{}`, `"code":{"` + strings.Repeat("k", 257) + `":0}`},
		{`"code":{}`, `"code":{"x":"` + strings.Repeat("v", 16385) + `"}`},

		{`"product_ready":false`, `"product_ready":null`}, {`"product_ready":false`, `"product_ready":true`}, {`"product_ready":false,`, ``},
		{`"chosen":"COLLECT_MORE_EVIDENCE"`, `"chosen":"RECOMMEND"`}, {`"chosen":"COLLECT_MORE_EVIDENCE"`, `"chosen":null`},
		{`"recommendation_document_id":null`, `"recommendation_document_id":"44444444444444444444444444444444"`},
		{`"version":"economic-request-v1"`, `"version":"economic-request-v2"`}, {`"horizon":10`, `"horizon":11`}, {`"threshold":0.95`, `"threshold":0.9`},
		{`"namespace":"11111111111111111111111111111111"`, `"namespace":"44444444444444444444444444444444"`},
		{`"canonical_tokens":37`, `"canonical_tokens":37.0`}, {`"canonical_tokens":37`, `"canonical_tokens":true`}, {`"coverage":[1,1]`, `"coverage":[2,1]`}, {`"coverage":[1,1]`, `"coverage":[0,1]`}, {`"attempts":2`, `"attempts":0`},
		{`"whole_project_cost":null`, `"whole_project_cost":0`}, {`"effect":null`, `"effect":0`}, {`"reasons":["whole_project_cost_population_missing"]`, `"reasons":["raw\ntext"]`},
		{`"input_watermark":{"documents":3,"reports":1}`, `"input_watermark":1`}, {`"input_watermark":{"documents":3,"reports":1}`, `"input_watermark":{"documents":null,"reports":1}`},
		{`"alternatives":[]`, `"alternatives":null`}, {`"as_of":{`, `"as_of":{"extra":true,`}, {`"coverage":[1,1]`, `"Coverage":[1,1]`},
		{`"product_ready":false`, `"product_ready":false,"product_ready":false`},
	}
	for i, c := range changes {
		t.Run(c[0]+string(rune('a'+i)), func(t *testing.T) {
			a, e := ParseEconomicAssessment([]byte(strings.ReplaceAll(economicFixture, c[0], c[1])), economicNS, p)
			if e == nil || a != nil {
				t.Fatal("invalid sidecar accepted")
			}
		})
	}
	for _, raw := range [][]byte{append([]byte(economicFixture), []byte(` {}`)...), []byte(economicFixture[:100]), []byte(strings.Repeat(" ", MaxEconomicResponseBytes+1)), append([]byte{0xff}, []byte(economicFixture)...)} {
		if a, e := ParseEconomicAssessment(raw, economicNS, p); e == nil || a != nil {
			t.Fatal("malformed sidecar accepted")
		}
	}
	if _, e := ParseEconomicAssessment([]byte(economicFixture), "44444444444444444444444444444444", p); e == nil {
		t.Fatal("namespace mismatch")
	}
}
func TestEconomicPolicyBounds(t *testing.T) {
	for _, v := range [][2]string{{"0", "0.95"}, {"01", "0.95"}, {"1000001", "0.95"}, {"1", "0"}, {"1", ".95"}, {"1", "NaN"}, {"1", "1.1"}, {"1", "0.1234567"}, {"1", ""}} {
		if _, e := NewEconomicPolicy(v[0], v[1]); e == nil {
			t.Fatal(v)
		}
	}
	for _, v := range [][2]string{{"1", "0.000001"}, {"1000000", "1.000000"}} {
		if _, e := NewEconomicPolicy(v[0], v[1]); e != nil {
			t.Fatal(v, e)
		}
	}
}
func TestEconomicAuthenticatedJSONTransportNoFallback(t *testing.T) {
	t.Setenv("HINT_SERVER_TOKEN", "synthetic-token")
	for _, tc := range []struct {
		content string
		status  int
		body    string
		good    bool
	}{
		{"application/json", 200, economicFixture, true}, {"application/vnd.hint+json", 200, economicFixture, false}, {"application/json-evil", 200, economicFixture, false}, {"application/json", 400, `{"error":"invalid_economic_policy"}`, false},
	} {
		t.Run(tc.content+strconv.Itoa(tc.status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/hint/v0.1/recommendations" || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Accept") != "application/json" {
					t.Error("transport")
				}
				q := r.URL.Query()
				if len(q) != 8 || q.Get("economic_horizon") != "10" || q.Get("economic_correctness_guard") != "paired_accepted_fraction_lower_bound_nonnegative" || q.Get("project_namespace") != economicNS {
					t.Error(q)
				}
				w.Header().Set("Content-Type", tc.content)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c, e := New(srv.URL)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.EconomicRecommendations(context.Background(), economicNS, economicP(t))
			if (e == nil) != tc.good || calls.Load() != 1 {
				t.Fatalf("response error=%v calls=%d", e, calls.Load())
			}
		})
	}
}
