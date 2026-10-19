# HINT data standard — change log

## 0.2.0 (draft, 2026-10-18)

- Optional `Observation.context`: a measurement of delivered context (action, stage, artifact, reference, snapshot, policy state and reference, freshness, result, UTF-8 bytes, omitted details, a labeled token estimate, `repeat_of`). It is allowed only on `DISCOVER`/`READ` observations with a tool call ID. See [hint-data v0.2](hint-data-v0.2.md).
- Optional `RecommendationPayload.findings` and `abstentions`: typed effort findings (problem, evidence state, observed quantities with coverage, scope, references, cause hypotheses, candidate interventions, next step, heuristic priority). See [hint-data v0.2](hint-data-v0.2.md).
- Optional `Plan.forecast_basis` (objective, horizon, model-conditional probability and net interval, threshold provenance, cost basis, evidence, conditions) and the semantic rule `forecast_inconsistent`.
- Semantic rule `duplicate_measurement`.
- Optional `ChangeSetPayload.plan_application`: a client-declared association of a change set with one plan of a stored recommendation set (set ID and digest, plan ID, optional cross-repository group). An association only, never a confirmed realization or effect. Semantic rule `plan_application_inconsistent`.
- Optional `sources[].configuration` (canonical effective configuration with known/not_applicable/unknown states and the user's declared settings), `correlation.task_family_id` (declared task family) and `plan_application.work` (the realized attempt of a declared work unit). Semantic rule `task_family_conflict`.
- Draft model calls (C7B-E): `op: MODEL_CALL` with `request_kind` and `correlation.call_ref`, `request_kind` on the terminal `USAGE`, the typed `collection` closure on a closing `SESSION` record, allowlisted `sources[].usage_normalization`. Semantic rules `call_link_conflict` and `collection_inconsistent`. Field names `correlation.call_ref`, `request_kind`, `collection`.
- The export policy lists `context`, `collection` and `request_kind` as observation fields.
- Everything else is unchanged; v0.1 documents stay valid v0.1 documents.
- Conformance: the "unsupported minor version" fixture now uses `0.3.0`, because `0.2` is supported.

## 0.1.0 (draft, 2026-10-12)

First draft. Not final; fields can change before the first release. The version stays 0.1.0 until v0.1 is released.

- Envelope with seven document types; one format for files and the API.
- Records: `Snapshot`, `Entity`, `Relation`, `PolicyConstraint`, `Change`, `Observation`, `Metric`, `Plan`, `Effect`.
- `Quantity` with the states `known`, `unknown` and `partial` (known part and coverage). Quantities are never negative; `count`, `lines` and `tasks` are integers; `ratio` lies in [0, 1].
- Evidence classes: observation, derived metric, estimate, forecast, confirmed effect. Forecast intervals, forecast points and effect estimates are signed; cost and break-even intervals use `NonNegativeInterval`.
- Closed schemas without free-form strings. The privacy contract states that the schema checks the form of a value, not its origin, and lists the producer obligations.
- HMAC-based opaque identifiers; contract identifiers shared across repositories only with contract evidence. Trace, span and parent span IDs are HMAC pseudonyms in the W3C formats; source IDs are never exported.
- RFC 8785 canonical form; content and snapshot digests.
- The `observation_id` of a span observation includes the source trace-id and span-id.
- A plan has exactly one snapshot for each of its repositories (semantic rule `plan_snapshot_set`).
- Client import validation with fixed order and result codes.
