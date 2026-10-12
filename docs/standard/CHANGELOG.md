# HINT data standard — change log

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
