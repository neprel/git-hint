package api

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	forecastInteger     = regexp.MustCompile(`^[0-9]{1,13}$`)
	forecastProbability = regexp.MustCompile(`^(0(\.[0-9]{1,6})?|1(\.0{1,6})?)$`)
)

// ValidateForecastPolicy checks the four existing forecast query keys before any I/O.
// No keys means no forecast policy: no horizon or threshold is supplied implicitly.
// Decimal strings retain their precision; callers must not round floating-point input.
// It returns a private copy so a later caller mutation cannot change the checked policy.
func ValidateForecastPolicy(values url.Values) (url.Values, error) {
	if len(values) == 0 {
		return nil, nil
	}
	for key, value := range values {
		switch key {
		case "forecast_horizon_tasks", "forecast_threshold", "assumed_cost_tokens_lower", "assumed_cost_tokens_upper":
		default:
			return nil, errors.New("unknown forecast policy key")
		}
		if len(value) != 1 {
			return nil, errors.New("forecast policy keys require exactly one value")
		}
	}
	horizon, present := values["forecast_horizon_tasks"]
	if !present {
		return nil, errors.New("--forecast-threshold and --assumed-cost need an explicit --forecast-horizon")
	}
	if !forecastInteger.MatchString(horizon[0]) {
		return nil, errors.New("--forecast-horizon must be an integer 1-100000")
	}
	h, _ := strconv.ParseInt(horizon[0], 10, 64)
	if h < 1 || h > 100000 {
		return nil, errors.New("--forecast-horizon must be an integer 1-100000")
	}
	if threshold, present := values["forecast_threshold"]; present {
		if !forecastProbability.MatchString(threshold[0]) || strings.Trim(threshold[0], "0.") == "" {
			return nil, errors.New("--forecast-threshold must be a decimal in (0, 1] with at most 6 fraction digits")
		}
	}
	lo, hasLo := values["assumed_cost_tokens_lower"]
	hi, hasHi := values["assumed_cost_tokens_upper"]
	if hasLo != hasHi {
		return nil, errors.New("assumed cost requires both lower and upper bounds")
	}
	if hasLo {
		if !forecastInteger.MatchString(lo[0]) || !forecastInteger.MatchString(hi[0]) {
			return nil, errors.New("--assumed-cost must be LO:HI in tokens (integers)")
		}
		l, _ := strconv.ParseInt(lo[0], 10, 64)
		u, _ := strconv.ParseInt(hi[0], 10, 64)
		if u > 1_000_000_000_000 || l > u {
			return nil, errors.New("--assumed-cost must satisfy 0 <= LO <= HI <= 1000000000000")
		}
	}
	checked := url.Values{}
	for key, value := range values {
		checked[key] = []string{value[0]}
	}
	return checked, nil
}
