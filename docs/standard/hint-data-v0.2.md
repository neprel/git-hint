# HINT data standard v0.2 — changes from v0.1

**Version:** 0.2.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-18

v0.2 is v0.1 plus one optional field: the **context measurement** of an observation. All other rules of [hint-data v0.1](hint-data-v0.1.md), [privacy](privacy-v0.1.md), [recommendation](recommendation-v0.1.md) and [exchange](exchange-v0.1.md) stay the same:

- identifiers and their derivation;
- digests and the canonical form;
- envelope and document types;
- every other field.

Schemas: [`schemas/v0.2/`](../../schemas/v0.2/), entry point `urn:hint:schema:v0.2:envelope`. Example: [`examples/v0.2/`](../../examples/v0.2/).

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

## Versions

- The CLI and the server accept `0.1` and `0.2`. A v0.1 document cannot carry a context measurement.
- A producer writes v0.2 only when a document holds a context measurement; otherwise it writes v0.1.
- `0.3` is unsupported (`unsupported_version`).
