package cli

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// The forecast request policy of `git hint recommend` (server design §9): the same strict
// forms and bounds as the server, checked before any network or write action.
const (
	maxForecastHorizon = 100000
	maxAssumedCost     = 1_000_000_000_000
)

var (
	intForm  = regexp.MustCompile(`^[0-9]{1,13}$`)
	probForm = regexp.MustCompile(`^(0(\.[0-9]{1,6})?|1(\.0{1,6})?)$`)
	hexID    = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// forecastPolicy maps the set flags to the query parameters; nil without forecast flags.
func forecastPolicy(fs *flag.FlagSet, horizon, threshold, cost string) (url.Values, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["forecast-horizon"] && !set["forecast-threshold"] && !set["assumed-cost"] {
		return nil, nil
	}
	if !set["forecast-horizon"] {
		return nil, errors.New("--forecast-threshold and --assumed-cost need an explicit --forecast-horizon")
	}
	if !intForm.MatchString(horizon) {
		return nil, errors.New("--forecast-horizon must be an integer 1-100000")
	}
	if h, _ := strconv.Atoi(horizon); h < 1 || h > maxForecastHorizon {
		return nil, errors.New("--forecast-horizon must be an integer 1-100000")
	}
	q := url.Values{"forecast_horizon_tasks": {horizon}}
	if set["forecast-threshold"] {
		if !probForm.MatchString(threshold) || strings.Trim(threshold, "0.") == "" {
			return nil, errors.New("--forecast-threshold must be a decimal in (0, 1] with at most 6 fraction digits")
		}
		q.Set("forecast_threshold", threshold)
	}
	if set["assumed-cost"] {
		lo, hi, ok := strings.Cut(cost, ":")
		if !ok || !intForm.MatchString(lo) || !intForm.MatchString(hi) {
			return nil, errors.New("--assumed-cost must be LO:HI in tokens (integers)")
		}
		l, _ := strconv.ParseInt(lo, 10, 64)
		u, _ := strconv.ParseInt(hi, 10, 64)
		if u > maxAssumedCost || l > u {
			return nil, errors.New("--assumed-cost must satisfy 0 <= LO <= HI <= 1000000000000")
		}
		q.Set("assumed_cost_tokens_lower", lo)
		q.Set("assumed_cost_tokens_upper", hi)
	}
	return q, nil
}

// declarePlan records a declared plan application (git hint changes --declare-plan). It reads
// local state and writes one declaration file below .hint/state/declarations/; no network.
func declarePlan(e *Env, s *store.Store, planID, from string) int {
	if !hexID.MatchString(planID) {
		fmt.Fprintln(e.Stderr, "git hint changes: --declare-plan needs a plan ID (32 hexadecimal characters)")
		return 2
	}
	src := from
	var err error
	if src == "" {
		if src, err = project.LatestRecommendation(s); err != nil {
			return fail(e, err)
		}
	} else if !filepath.IsAbs(src) {
		src = filepath.Join(e.Dir, src)
	}
	raw, err := readLimited(src, project.MaxDocumentBytes)
	if err != nil {
		return fail(e, err)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	d, created, err := project.Declare(s, raw, planID, e.Now())
	if errors.Is(err, project.ErrDeclarationConflict) {
		return fail(e, fmt.Errorf("%w: plan %s of set %s is already declared for this change", err, d.PlanID, d.SetID))
	}
	if err != nil {
		return fail(e, err)
	}
	state := "Recorded"
	if !created {
		state = "Already recorded"
	}
	fmt.Fprintf(e.Stdout, "%s: the change %s -> %s is declared as the application of plan %s.\n", state, d.BeforeSnapshotID[:12], d.AfterSnapshotID[:12], planID[:12])
	fmt.Fprintln(e.Stdout, "This is an association only; the server checks it. It is sent with the next export or recommend.")
	return 0
}
