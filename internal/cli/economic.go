package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/neprel/git-hint/internal/api"
)

func economicReceived(e *Env, file, namespace string, p api.EconomicPolicy) int {
	if !filepath.IsAbs(file) {
		file = filepath.Join(e.Dir, file)
	}
	raw, err := readLimited(file, api.MaxEconomicResponseBytes)
	if err != nil {
		return fail(e, err)
	}
	result, err := api.ParseEconomicAssessment(raw, namespace, p)
	if err != nil {
		return fail(e, err)
	}
	renderEconomic(e, result)
	return 0
}
func fetchEconomic(e *Env, ctx context.Context, c *api.Client, namespace string, policy api.EconomicPolicy) int {
	assessment, err := c.EconomicRecommendations(ctx, namespace, policy)
	if err != nil {
		return fail(e, fmt.Errorf("fetch economic assessment: %w", err))
	}
	renderEconomic(e, assessment)
	return 0
}

func renderEconomic(e *Env, a *api.EconomicAssessment) {
	fmt.Fprintln(e.Stdout, "Economic assessment: COLLECT_MORE_EVIDENCE (product_ready=false)")
	fmt.Fprintf(e.Stdout, "Horizon: %d accepted completions; threshold: %s\n", a.Policy.Horizon, a.Policy.Threshold)
	fmt.Fprintln(e.Stdout, "Risk: conservative_cost_bounds; correctness guard: paired_accepted_fraction_lower_bound_nonnegative")
	tokens := "unknown"
	if a.CanonicalTokens != nil {
		tokens = fmt.Sprint(*a.CanonicalTokens)
	}
	attempts := "unknown"
	if a.Attempts != nil {
		attempts = fmt.Sprint(*a.Attempts)
	}
	fmt.Fprintf(e.Stdout, "Observed received-task baseline: canonical full-task tokens=%s; coverage=%d/%d; attempts=%s; counter_semantics=%s\n", tokens, a.Coverage[0], a.Coverage[1], attempts, a.CounterSemantics)
	fmt.Fprintln(e.Stdout, "Scope: received confirmed tasks, all attempts. Whole-project cost, effect, savings, and a winner remain unknown.")
	for _, reason := range a.Reasons {
		fmt.Fprintln(e.Stdout, "Missing or refused:", reason)
	}
	fmt.Fprintln(e.Stdout, "This JSON sidecar is not a HINT plan. HINT does not save or apply it as a recommendation.")
}
