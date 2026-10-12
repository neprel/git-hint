# HINT recommendation contract v0.1

**Version:** 0.1.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-11
**Schema:** `recommendation.schema.json` (`Plan`, `Prediction`, `Simulation`, `Step`, `Effect`) | **Example:** [`recommendation.json`](../../examples/v0.1/recommendation.json), [`effect-report.json`](../../examples/v0.1/effect-report.json)

This contract defines what a client receives, validates and shows. It does not define how a server finds or ranks a recommendation.

## 1. Plan

| Field | Required | Meaning |
|---|---|---|
| `plan_id`, `plan_schema` | yes | opaque ID; specification version |
| `project_namespace`, `repo_ids` | yes | target project and repositories |
| `snapshots` | yes | exactly one entry for each repository in `repo_ids`: snapshot ID and snapshot digest the plan was made for (semantic rule `plan_snapshot_set`) |
| `intervention_class` | yes | `NO_CHANGE`, `CONTEXT_ONLY`, `REFACTOR_CODE`, `REPARTITION`, `REASSIGN_OWNER`, `COLLECT_MORE_EVIDENCE` |
| `transformation` | yes | for `REFACTOR_CODE` and `REPARTITION`: `SPLIT`, `MERGE`, `MOVE`, `EXTRACT`, `INTRODUCE_INTERFACE`, `ISOLATE_STATE`, `REMOVE_CANDIDATE`; `null` for other classes |
| `decision` | yes | `recommend`, `refuse_uncertain`, `refuse_negative`, `collect_more_evidence` |
| `targets` | yes | opaque entity IDs that the plan acts on |
| `preconditions` | yes | checks before any step: `snapshot_matches`, `entities_resolve`, `worktree_clean`, `tests_pass`, `owner_approval`, `constraint_holds`, with target IDs |
| `steps` | yes | ordered declarative steps (§2); empty unless `decision` is `recommend` |
| `reasons` | yes | reason codes with supporting observation and metric IDs |
| `predictions` | no | forecasts (§3) |
| `simulation` | no | how the forecast was produced, as labels (§3) |
| `refactoring_cost_estimate` | no | interval in tokens or seconds; never negative |
| `break_even_horizon` | no | interval in tasks, never negative, or `unbounded: true` |
| `risk_constraints` | yes | behavior, API, test, security and performance requirements; each says whether its check is deterministic |
| `required_approvals` | yes | `owner`, `code_owner`, `security_reviewer` |
| `authorization_basis` | for destructive plans | `deterministic` or `probabilistic` |
| `created_at`, `expires_at` | yes | validity window |

Reason codes: `co_change`, `boundary_crossings`, `context_size`, `navigation_repeats`, `change_spread`, `rework`, `coordination_wait`, `ownership_split`, `weak_evidence`, `low_adoption`.

Optional fields stay absent until a server can support them with evidence. A server MUST NOT emit a fabricated value.

## 2. Steps

A step has `step_id` (integer), `action` (`SPLIT`, `MERGE`, `MOVE`, `EXTRACT`, `INTRODUCE_INTERFACE`, `ISOLATE_STATE`, `REMOVE_CANDIDATE`, `REASSIGN_OWNER`, `ADD_CONTEXT`, `RUN_CHECKS`), `targets`, an optional `destination`, `depends_on` (earlier steps only), `requires_approval` and `support` (`supported`, `unsupported`, `ambiguous`).

- A step names entities and an action. It never carries a command, a script, a patch or code. A client MUST NOT execute a plan as a shell script.
- A client applies only `supported` steps whose IDs resolve. An `unsupported` or `ambiguous` step is shown, not applied.

## 3. Forecasts

A `Prediction` has `class: "forecast"`, a `metric` with its fixed unit ([hint-data](hint-data-v0.1.md) §8.2), `per` (`task` or `total_over_horizon`), an `interval`, an optional `point` and an optional `probability_improvement`. The interval and the point are signed: they give the expected change of the metric, so a decrease is negative.

`simulation` labels the forecast:

| Field | Values |
|---|---|
| `level` | `NONE` (no simulation), `L1` (structural what-if on the graph), `L2` (projection of observed traces onto the changed structure), `L3` (calibrated model of agent behavior) |
| `calibration_status` | `uncalibrated`, `backtested`, `holdout_validated` |
| `out_of_distribution` | `true` when the task class or agent configuration has no calibration data |
| `task_distribution_id`, `agent_configuration_id`, `model_version` | opaque references and a version, so that two forecasts can be compared |

`L4` is not a simulation level. Empirical results are `Effect` records (§5). A client SHOULD show the level, the calibration status and the interval with every forecast, and SHOULD NOT show a point value alone.

## 4. Destructive transformations

A plan whose transformation or any step is `REMOVE_CANDIDATE` MUST have `authorization_basis: deterministic` and `owner` in `required_approvals`. A low-confidence or probabilistic judgment never authorizes a destructive transformation. A `PolicyConstraint` with `blocks_destructive: true` blocks such a plan on its scope.

## 5. Effects (L4)

An `Effect` has `class: "confirmed_effect"`, `evidence_level: "L4"`, the `plan_id`, a metric and unit, the `control` and `treatment` task sets (snapshot ID and task IDs), the number of `pairs`, an `estimate` interval, the `method` (`paired_difference_bootstrap`, `paired_t`, `sign_test`), the checked constraints and the agent configuration. A forecast is never an effect.

## 6. Client handling

The client import validation is defined in [exchange](exchange-v0.1.md) §4. It checks version, schema, digests, namespace, expiry, snapshot digests and identifier resolution, in that order, and gives each plan one status: `accept`, `partial`, `render_only` or `reject`.
