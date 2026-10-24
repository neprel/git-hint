package cli

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// forecastPolicy maps the set flags to the query parameters; nil without forecast flags.
func forecastPolicy(fs *flag.FlagSet, horizon, threshold, cost string) (url.Values, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	q := url.Values{}
	if set["forecast-horizon"] {
		q.Set("forecast_horizon_tasks", horizon)
	}
	if set["forecast-threshold"] {
		q.Set("forecast_threshold", threshold)
	}
	if set["assumed-cost"] {
		// Keep the existing dependency error before parsing the CLI's combined bounds.
		if !set["forecast-horizon"] {
			return nil, errors.New("--forecast-threshold and --assumed-cost need an explicit --forecast-horizon")
		}
		lo, hi, ok := strings.Cut(cost, ":")
		if !ok {
			return nil, errors.New("--assumed-cost must be LO:HI in tokens (integers)")
		}
		q.Set("assumed_cost_tokens_lower", lo)
		q.Set("assumed_cost_tokens_upper", hi)
	}
	return api.ValidateForecastPolicy(q)
}

// declarePlan records a declared plan application (git hint changes --declare-plan). It reads
// local state and writes one declaration file below .hint/state/declarations/; no network.
func declarePlan(e *Env, s *store.Store, planID, from, work, after string) int {
	if !hexID.MatchString(planID) {
		fmt.Fprintln(e.Stderr, "git hint changes: --declare-plan needs a plan ID (32 hexadecimal characters)")
		return 2
	}
	raw, err := readSet(e, s, from)
	if err != nil {
		return fail(e, err)
	}
	lock, err := s.LockState(true)
	if err != nil {
		return fail(e, err)
	}
	defer lock.Unlock()
	var d project.Declaration
	var created bool
	if work != "" {
		d, created, err = project.DeclareWork(s, raw, planID, work, after, e.Now())
	} else {
		d, created, err = project.Declare(s, raw, planID, e.Now())
	}
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
