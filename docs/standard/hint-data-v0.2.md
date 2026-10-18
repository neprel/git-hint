# HINT data standard v0.2 — changes from v0.1

**Version:** 0.2.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-18

v0.2 is v0.1 plus two optional additions: the **context measurement** of an observation and the **effort findings** of a recommendation set. All other rules of [hint-data v0.1](hint-data-v0.1.md), [privacy](privacy-v0.1.md), [recommendation](recommendation-v0.1.md) and [exchange](exchange-v0.1.md) stay the same:

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

`abstentions` lists the problem classes that the server did not diagnose and why (`no_confirmed_tasks`, `insufficient_coverage`). A plan in a v0.2 set names `plan_schema` 0.2.x. The client shows at most five findings.

## Versions

- The CLI and the server accept `0.1` and `0.2`. A v0.1 document cannot carry a context measurement or findings.
- A producer writes v0.2 only when a document holds a context measurement or findings; otherwise it writes v0.1.
- `0.3` is unsupported (`unsupported_version`).
