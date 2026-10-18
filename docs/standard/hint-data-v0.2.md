# HINT data standard v0.2 — changes from v0.1

**Version:** 0.2.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-18

v0.2 is v0.1 plus two optional additions: the **context measurement** of an observation (and `context` as a missing field) and the **effort findings** (with decisions and context policy feedback) of a recommendation set. All other rules of [hint-data v0.1](hint-data-v0.1.md), [privacy](privacy-v0.1.md), [recommendation](recommendation-v0.1.md) and [exchange](exchange-v0.1.md) stay the same:

- identifiers and their derivation;
- digests and the canonical form;
- envelope and document types;
- every other field.

Schemas: [`schemas/v0.2/`](../../schemas/v0.2/), entry point `urn:hint:schema:v0.2:envelope`. Examples: [`examples/v0.2/`](../../examples/v0.2/).

## Context measurement (`Observation.context`)

A context measurement records context that HINT delivered to an agent: a bounded answer, an expansion of a detail, or a retrieval of the original. It is allowed only on a `DISCOVER` or `READ` observation with a `correlation.tool_call_id`.

| Field | Meaning |
|---|---|
| `action` | `deliver` (a bounded answer), `expand` (a detail of an earlier answer), `retrieve_original` (the full original) |
| `stage` | `raw_tool_output` or `delivered_context`. Two stages of one call are two measurements of the same call, not two calls |
| `artifact_id` | opaque ID of the immutable context artifact; it binds the snapshot, the index and the actual input. Required when `result` is `delivered` |
| `ref_id` | opaque ID of the expandable reference that was used or returned |
| `snapshot_id` | opaque ID of the snapshot that the answer comes from. Required when `result` is `delivered` |
| `policy_state` | `enabled`, `disabled`, `not_applicable` or `unknown`. These four values never stand for each other |
| `policy_ref` | the project reference of the full context policy identity (id, version, digest). Present exactly when `policy_state` is `enabled` |
| `freshness` | `current`, `stale` or `unknown` |
| `result` | `delivered`, `refused_stale` (needs `freshness: stale`), `missing` or `error`. An old reference reads the same snapshot or is refused as stale; it never returns current source |
| `bytes` | UTF-8 bytes of the payload actually returned, before the transport envelope; `null` when unknown. An expansion is its own payload |
| `omitted` | details left out by the budget; `null` when unknown |
| `tokens` | an **estimate** only: `value`, `estimator` (a listed HINT estimator), `estimator_version`, `label: "estimate"`. Never observed model usage or cache |
| `repeat_of` | the artifact of an earlier retrieval of exactly the same target and snapshot. It is never set by time |

**Semantic rule `duplicate_measurement`:** two context measurements of one tool call with the same stage, action and reference are invalid. Counters (deliveries, expansions, repeated retrievals, bytes) are derived from these records; there is no second counter system. An expansion does not mean that the earlier answer failed, nor that the model used the content.

## Effort findings (`RecommendationPayload.findings`, `abstentions`)

A recommendation set can carry up to 20 findings. A finding states what costs effort, with its evidence. It holds only enumerations, numbers and opaque IDs; the client composes all text with local names.

| Field | Meaning |
|---|---|
| `problem` | `repeated_navigation`, `repeated_delivery`, `large_low_selectivity_read`, `recovery_after_lost_context`, `repeated_validation`, `coordination_wait` |
| `evidence_state` | `supported`, or `limited` when coverage or the number of tasks is too low to separate the causes |
| `quantities` | up to 10 observed quantities: `metric`, `unit`, `value` (null when `state` is `unknown`), `coverage` `[known, total]`. They are counts of what was observed, never avoidable work or a saving. Example: `repeats_without_observed_edit` counts reads of a target with no observed edit since the last read; edit coverage can be incomplete (an edit outside the observed tools is not seen) |
| `scope` | the confirmed task IDs (at most 50), their count and the snapshot IDs |
| `targets`, `observation_ids` | opaque entity and observation IDs that support the finding |
| `causes` | hypotheses: `cause`, `state` (`supported`, `not_excluded`, `not_supported`) and the evidence codes (`basis`) that decide the state. Evidence counts only for the same task and target (for example a context measurement); unrelated evidence never excludes a cause |
| `interventions` | candidate intervention classes (the plan classes) |
| `next_step` | a checkable step: `refresh_index_and_recheck`, `compare_context_variant`, `inspect_coupling`, `review_validation_loop`, `review_ownership`, `collect_more_tasks`, with targets |
| `priority` | always `heuristic`: the array order is a heuristic priority, not a calibrated ranking |
| `decision` | optional: the minimum sufficient intervention. Each alternative (`CONTEXT_ONLY`, `REFACTOR_CODE`) uses only eval variant pairs that are bound to the finding's objectives: `effort_delta` (tokens per attempt, every attempt counted) and `success_delta` (correctness evidence), each with interval and coverage, and `scope` with the `pair_id` of the comparison. `state`: `chosen`, `known`, `unknown`, `unbound` (no pair bound to the finding), `partial_coverage` (resources known for only part of the objectives; never used for a choice), `excluded_correctness`, `cost_unknown`, `not_applicable`. `evidence`: `bound_eval_pairs`, `unbound` or `none`. Without bound evidence the choice is `COLLECT_MORE_EVIDENCE`. There is no net value, horizon or break-even: `cost_model` is `unavailable` (no HINT cost model is available yet). `basis` is always `heuristic` |

`project_comparisons` (up to 20) lists project-level eval variant comparisons that no finding is bound to: the observed differences with coverage and the objective, intervention (variant) and baseline eval references. They decide no intervention by themselves.

`context_feedback` (up to 20) reports on each effective context policy identity (`policy_ref`): counters derived from the context measurements (deliveries, expansions, repeated originals, stale refusals, delivered bytes) and a comparison of whole task windows (all navigation, context calls included) and test pass rates against a matched cohort: tasks of the same agent source that changed the same components, where the source observed `hint_context` and no measurement is missing, and that had no delivery. Each quantity has its coverage. `proposal`: `keep`, `propose_revision`, `propose_disable` or `insufficient_evidence`, with the `basis` codes. An expansion is normal progressive disclosure, never a failure by itself. Proposals only; `basis_label` is always `heuristic`.

`abstentions` lists the problem classes that the server did not diagnose and why (`no_confirmed_tasks`, `insufficient_coverage`). A plan in a v0.2 set names `plan_schema` 0.2.x. The client shows at most five findings.

## Forecast basis of a plan (`Plan.forecast_basis`)

Optional. It tells how to read a plan's forecast; a forecast is never a measurement, and it never enters the observed `decision` of a finding.

| Field | Meaning |
|---|---|
| `objective` | the one scalar resource objective: `metric` and `unit` (never tokens and seconds together) |
| `horizon_tasks` | the stated number of future comparable tasks behind any net value (integer ≥ 1) |
| `threshold`, `threshold_basis` | the decision threshold on the model-conditional probability; `heuristic_default` (the unchanged default, 0.9) or `configured_policy` |
| `p_net_gain_positive` | model-conditional P(net value over the horizon > 0); null when unknown |
| `net_gain_interval` | the net value over the horizon (a total, not per task) in the objective unit, as an interval with its level; negative values are valid; null when unknown |
| `cost_basis` | `observed_comparable_work` (an estimate from observed comparable refactoring work, not a measured cost of this change), `assumption_scenario`, or null (unknown) |
| `evidence` | `synthetic_fixture` or `project_history` (baseline evidence; it does not validate an effect) |
| `conditional_on` | every stated qualification: `behavior_transport`, `unseen_transformation`, `feature_extrapolation`, `unseen_task_mix`, `unseen_configuration`, `coordination_unmodeled` |

**Semantic rule `forecast_inconsistent`:** the plan has `simulation`; a per-task prediction exists for the objective metric and unit, and the one-time cost uses the same unit; a probability, net interval or break-even needs a known one-time cost (`cost_basis` and `refactoring_cost_estimate`); `heuristic_default` means the default threshold; a scenario cost or synthetic evidence never gives `recommend`; an unseen transformation, configuration or task mix, an extrapolation or unmodeled coordination needs `out_of_distribution: true` and no `recommend`; an L2 or L3 forecast states `behavior_transport`. The interval rule (`interval_order`) also applies to `net_gain_interval`. The break-even keeps its `unbounded` flag; no infinite number is sent.

## Declared plan application (`ChangeSetPayload.plan_application`)

A change set can declare that the client applied one plan of a stored recommendation set. The declaration names the set (`recommendation_set.document_id` and `content_digest`) and the `plan_id`; `declaration` is always `client_declared_association`. Example: [`change-set-declared.json`](../../examples/v0.2/change-set-declared.json).

- **Meaning.** The client declares an association only. The declaration does not confirm that the plan was realized correctly, that it changed behavior or that it had an effect. A receiver checks the association against the stored set, the plan's snapshot and the change mappings. It keeps the declaration, the structural consistency, the validation outcome and any measured effect as separate states. A measured effect is an `Effect` in an `effect_report`, never a declaration.
- **Snapshots and identity.** The change set's existing `before_snapshot`, `after_snapshot` and `changes` carry the evidence. File IDs depend on the path, so a moved file gets a new ID; the `Change` records map the old IDs to the new ones.
- **Several repositories.** A plan over several repositories needs one declared change set per repository, each with the same `group` (`group_id` and the complete `repo_ids` of the plan). A missing member leaves the application partial; a receiver never groups change sets by time.
- **One association per change set.** A later declaration is a new change-set document with its own ID; it never replaces an earlier document.

**Semantic rule `plan_application_inconsistent`:** a declared group names the declaring repository (`repo_id`) among its `repo_ids`.

## Configuration identity and declared task family

- **`sources[].configuration`** (optional): the canonical effective configuration of a source's runs — agent name and version, the effective model (a reference with its provenance: `launch_parameter` when the selection that constructed the launch is enforced and checked, `agent_reported` when the agent reports it; otherwise unknown — a declaration alone never makes it known), harness (`harbor`, `direct` or `other`) and its version, context policy (state, reference, version), observed configuration references and the budget (request and time limits, each `known` with a value, `unbounded` or `unknown`; an absent limit is never unbounded), plus the user's explicit `declared` settings as provenance. Versions and references are `known`, `not_applicable` (versions only) or `unknown`; unknown is never not applicable. `enabled` context policies carry a reference and version; `disabled` and `not_applicable` carry neither. A known empty list of configuration references is known. References are project-scoped opaque values with the same derivations as eval reports; no raw model name, configuration text or key is sent. A source whose effective configuration changes gets a new source ID.
- **`correlation.task_family_id`** (optional, needs `task_id`): the task family that the run declared explicitly (`HINT_TASK_FAMILY`), with the same derivation as the eval report `task_family_ref`. A declaration, not a classification by HINT. Capability name `correlation.task_family_id`.
- **`plan_application.work`** (optional): the exact closed successful attempt (`unit_ref`, `revision`, `attempt_ref`) of a declared work unit of an eval report that this change realizes.

**Semantic rule `task_family_conflict`:** one task declares one family in a document.

## Versions

- The CLI and the server accept `0.1` and `0.2`. A v0.1 document cannot carry a context measurement or findings.
- A producer writes v0.2 only when a document holds a context measurement or findings; otherwise it writes v0.1.
- `0.3` is unsupported (`unsupported_version`).
