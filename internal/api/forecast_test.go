package api

import (
	"net/url"
	"reflect"
	"testing"
)

func TestValidateForecastPolicyBoundsAndPresence(t *testing.T) {
	for _, policy := range []url.Values{nil, {}, {"forecast_horizon_tasks": {"1"}},
		{"forecast_horizon_tasks": {"100000"}, "forecast_threshold": {"1.000000"},
			"assumed_cost_tokens_lower": {"0"}, "assumed_cost_tokens_upper": {"1000000000000"}},
		{"forecast_horizon_tasks": {"1"}, "forecast_threshold": {"0.000001"}}} {
		got, err := ValidateForecastPolicy(policy)
		if err != nil {
			t.Fatalf("%v: %v", policy, err)
		}
		if len(policy) == 0 {
			if got != nil {
				t.Fatalf("missing policy gained defaults: %v", got)
			}
			continue
		}
		if !reflect.DeepEqual(got, policy) {
			t.Fatalf("policy changed: %v != %v", got, policy)
		}
	}
}

func TestValidateForecastPolicyRejectsInvalidAndDoesNotAlias(t *testing.T) {
	invalid := []url.Values{
		{"forecast_threshold": {"0.9"}}, {"assumed_cost_tokens_lower": {"0"}, "assumed_cost_tokens_upper": {"1"}},
		{"forecast_horizon_tasks": {"0"}}, {"forecast_horizon_tasks": {"100001"}},
		{"forecast_horizon_tasks": {"-1"}}, {"forecast_horizon_tasks": {"1e3"}},
		{"forecast_horizon_tasks": {""}}, {"forecast_horizon_tasks": {"1", "2"}},
		{"forecast_horizon_tasks": {}}, {"producer_evidence": {"on"}},
	}
	for _, threshold := range []string{"0", "0.000000", "1.000001", "0.1234567", "1e-2", ".9", "NaN", " 0.9"} {
		invalid = append(invalid, url.Values{"forecast_horizon_tasks": {"1"}, "forecast_threshold": {threshold}})
	}
	for _, bounds := range [][2]string{{"2", "1"}, {"-1", "1"}, {"0", "1000000000001"}, {"1.5", "2"}} {
		invalid = append(invalid, url.Values{"forecast_horizon_tasks": {"1"},
			"assumed_cost_tokens_lower": {bounds[0]}, "assumed_cost_tokens_upper": {bounds[1]}})
	}
	invalid = append(invalid, url.Values{"forecast_horizon_tasks": {"1"}, "assumed_cost_tokens_lower": {"0"}},
		url.Values{"forecast_horizon_tasks": {"1"}, "assumed_cost_tokens_upper": {"1"}})
	for _, policy := range invalid {
		if got, err := ValidateForecastPolicy(policy); err == nil {
			t.Errorf("accepted %v as %v", policy, got)
		}
	}
	input := url.Values{"forecast_horizon_tasks": {"1"}}
	checked, err := ValidateForecastPolicy(input)
	if err != nil {
		t.Fatal(err)
	}
	input["forecast_horizon_tasks"][0] = "0"
	if checked.Get("forecast_horizon_tasks") != "1" {
		t.Fatal("validated policy aliases unchecked caller input")
	}
}
