# HINT data standard v0.1

**Version:** 0.1.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-11
**Schemas:** [`schemas/v0.1/`](../../schemas/v0.1/) (JSON Schema 2020-12) | **Examples:** [`examples/v0.1/`](../../examples/v0.1/) | **Conformance:** [`tests/conformance/`](../../tests/conformance/)
**Related:** [privacy](privacy-v0.1.md), [recommendations](recommendation-v0.1.md), [exchange](exchange-v0.1.md), [compatibility](compatibility.md)

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## 1. Scope

The standard defines an open format for four kinds of data about software repositories that AI coding agents work on:

- **repository structure** at one point in time (snapshots, entities, relations);
- **changes** between snapshots;
- **agent activity** (observations) and values computed from it (metrics);
- **recommendations** and the **effects** measured after a change.

The format does not depend on an agent (OpenCode, Claude Code, Codex), on a programming language or on the HINT implementation. One document format serves both the file mode and the API mode ([exchange](exchange-v0.1.md)). The format never carries source code, names, paths, prompts or raw outputs ([privacy](privacy-v0.1.md)).

The standard defines fields, units and allowed values. It does not define how a producer computes a metric or how a server computes a forecast.

## 2. Principles

1. **Unknown is not zero.** A value that is not observed is `unknown`. A real zero is a known value 0. A partly observed value carries its known part and its coverage, never a total (§5).
2. **Evidence classes stay apart.** Observations, derived metrics, estimates, forecasts and confirmed effects are different record classes (§5.3).
3. **No free text.** Every string in a document is an opaque identifier, a digest, a timestamp, a version or a value of a closed enumeration. The schemas have no free-form string field; unknown fields are invalid.
4. **One document, two transports.** A document is the same byte sequence in a file and in an API body.
5. **Snapshots and activity are separate documents**, so each can update at its own rate.

## 3. Documents and the envelope

Every document is one JSON object, the **envelope** (`envelope.schema.json`). Its `payload` depends on `document_type`.

| `document_type` | Payload records | Schema definition |
|---|---|---|
| `repository_snapshot` | `Snapshot`, `Entity`, `Relation`, `PolicyConstraint` | `repository#/$defs/SnapshotPayload` |
| `change_set` | `Change` | `repository#/$defs/ChangeSetPayload` |
| `activity_stream` | `Observation`, `Metric` | `activity#/$defs/ActivityPayload` |
| `recommendation_set` | `Plan` | `recommendation#/$defs/RecommendationPayload` |
| `effect_report` | `Effect` | `recommendation#/$defs/EffectPayload` |
| `export_policy` | the policy itself | `policy#/$defs/PolicyPayload` |
| `receipt` | API acknowledgement | `exchange#/$defs/ReceiptPayload` |

Envelope fields:

| Field | Required | Meaning |
|---|---|---|
| `hint_version` | yes | Specification version, `0.1.<patch>`. |
| `document_type` | yes | One of the table above. |
| `document_id` | yes | Opaque ID of this document. |
| `project_namespace` | yes | Opaque ID of the project; derived from the project key (§4). |
| `repositories` | yes | The repository set: opaque repository IDs that the payload may reference. |
| `producer` | yes | Producing tool (`hint-cli`, `hint-server`, `other`) and its version. |
| `sources` | for `activity_stream` | One entry per collecting adapter: agent, agent version (or `null` if unknown), adapter name and version, `capabilities` (fields it can provide), `unavailable` (fields it cannot provide), and optional event `coverage` (`events_received`, `events_dropped` as quantities). |
| `created_at` | yes | Creation time. |
| `redaction` | yes | Export policy ID, profile (`detailed`, `coarse`, `aggregate`), exported record types, `free_text: "denied"` and the time resolution in seconds. |
| `content_digest` | yes | `sha256:` digest of the canonical payload (§10). |
| `payload` | yes | The records. |

A consumer MUST NOT fill a field of one source from another source (common event format rule). The `sources` entries make the gaps of each adapter visible.

## 4. Identifiers

**Format.** An opaque identifier is 32 lowercase hex characters (128 bits). Trace and span identifiers follow W3C Trace Context (32 and 16 hex characters, not all zeros).

**Derivation.** A producer derives stable identifiers with a per-project secret key `K` (32 random bytes, kept in the local protected store; [privacy](privacy-v0.1.md) §3):

`id = hex(HMAC-SHA256(K, "hint:v0.1:" + domain + ("\x00" + part)*))[0:32]`

| Domain | Parts | Stability |
|---|---|---|
| `namespace` | — | per project key |
| `repo` | repository name in the local store | per repository |
| `file` | repo ID, repository-relative path with `/` | changes when the path changes |
| `symbol` | repo ID, language, language-qualified name (module or package plus name) | stable while the qualified name is stable, also after a file move or rename inside the same package |
| `contract` | contract kind, proven contract identifier | the same in every repository of the namespace (§6.3) |
| `local` | snapshot ID, parent entity ID, ordinal | valid in one snapshot only (`id_stability: snapshot_local`) |
| `snapshot`, `commit`, `event`, `session`, `call`, … | the native identifier, prefixed by the repo or source ID | as stable as the native identifier |
| `trace`, `span` | source W3C trace-id; source trace-id and span-id (§4.1) | as stable as the source trace |

Rules:

- An identifier is pseudonymous, not anonymous. The same key gives the same identifiers; a different key gives unrelated identifiers. Two projects with different keys MUST NOT be linkable through identifiers.
- A producer MAY use other derivations for record identifiers (for example random IDs), but MUST NOT encode a readable value.
- A consumer MUST treat identifiers as opaque. Equal identifiers prove equal input to the same key; they do not prove a runtime relation.
- Test vectors: [`tests/conformance/vectors/ids-v0.1.json`](../../tests/conformance/vectors/ids-v0.1.json) (published test keys only).

### 4.1 Trace and span identifiers

A producer never exports a source trace-id or span-id. It pseudonymizes them with the project key and keeps the W3C formats:

| Field | Derivation | Format |
|---|---|---|
| `trace_id` | first 16 bytes of `HMAC-SHA256(K, "hint:v0.1:trace" + "\x00" + T)` | 32 lowercase hex |
| `span_id` | first 8 bytes of `HMAC-SHA256(K, "hint:v0.1:span" + "\x00" + T + "\x00" + S)` | 16 lowercase hex |
| `parent_span_id` | the `span_id` derivation with the parent span-id as `S` and the trace-id `T` of the span | 16 lowercase hex |

- `T` and `S` are the source trace-id and span-id in lowercase hex. A source value with a wrong length, a non-hex character or all zeros is not exported; the field is listed in `missing`.
- W3C forbids an all-zero ID. If a derived ID is all zeros, the producer repeats the derivation with the extra parts `"1"`, `"2"`, … until the ID is not all zeros.
- Correlation is kept: equal source traces give equal `trace_id`s, and a `parent_span_id` equals the `span_id` of the parent span. The span derivation includes the trace-id, so equal span-ids in different traces give different `span_id`s.
- The derivation is deterministic for one key. Another key gives unrelated IDs.
- The `observation_id` of an observation built from a span is `DeriveID(K, "event", source_id, T, S)`. It includes the trace-id, so equal span-ids in two traces give two observations.
- The source IDs stay in the local store. A trace-id that the agent propagates to other systems (for example through MCP `params._meta`) cannot be matched with the exported ID without `K`.

## 5. Values and evidence classes

### 5.1 Quantity

Every measured or computed amount is a `Quantity`:

| Field | Meaning |
|---|---|
| `unit` | `bytes`, `tokens`, `count`, `seconds`, `lines`, `ratio`, `tasks` |
| `state` | `known`, `unknown` or `partial` |
| `value` | the exact value; only when `state` is `known`; 0 is a real zero; never negative |
| `known_part` | the sum over the covered items; only when `state` is `partial`; never negative |
| `coverage` | `covered` items with a known value out of `total` items (`total` may be `null` if unknown) |
| `bounds` | optional `lower`/`upper` range that contains the exact value (`upper: null` = no finite bound); never negative |
| `precision` | `exact`, `derived`, `inferred`, `estimated`; required for `known` and `partial` |
| `reason` | why a value is missing: `not_supported`, `not_collected`, `dropped`, `redacted`, `source_error`, `not_applicable_yet`; required for `unknown` and `partial` |
| `method` | how the value was obtained (closed list, for example `plugin_output_length`, `model_server_usage`, `otlp_attribute`, `clock_difference`) |

| Situation | Encoding |
|---|---|
| A request reported 0 output tokens | `{"unit":"tokens","state":"known","value":0,"precision":"exact"}` |
| The agent does not report token usage | `{"unit":"tokens","state":"unknown","reason":"not_supported"}` |
| 1 of 2 requests reported usage (7,808 tokens) | `{"unit":"tokens","state":"partial","known_part":7808,"coverage":{"covered":1,"total":2},"precision":"exact","reason":"source_error"}` |
| Boundary crossings cannot be counted exactly, but lie in 0–2 | `{"unit":"count","state":"unknown","reason":"not_collected","bounds":{"lower":0,"upper":2}}` |

**A quantity is an absolute amount.** Sizes, token counts, durations, line counts, entity features, change magnitudes and metric values are never negative. The schema enforces these unit rules:

| Unit | Admitted `value`, `known_part` and `bounds` |
|---|---|
| `bytes`, `tokens`, `seconds` | numbers ≥ 0 |
| `count`, `lines`, `tasks` | integers ≥ 0 (`value`, `known_part`); bounds ≥ 0 |
| `ratio` | numbers in [0, 1] |

A signed difference is never a `Quantity`. It exists only as a forecast (`Prediction.interval`, `Prediction.point`) or as an effect estimate (`Effect.estimate`), see §5.2.

The schema enforces the field combinations. The semantic rules (§9.2) also require `covered < total` for `partial`, `covered = total` for `known` with coverage, and a known value inside its bounds.

### 5.2 Intervals and probabilities

An `Interval` has `lower`, `upper` and `level` (the probability of the central interval, for example 0.9). A probability is a number in [0, 1].

| Interval | Sign |
|---|---|
| `Prediction.interval`, `Prediction.point`, `Effect.estimate` | signed: a change relative to the current state; a negative value is a decrease |
| `refactoring_cost_estimate.interval`, `break_even_horizon.interval` | absolute: `NonNegativeInterval`, both ends ≥ 0 |

### 5.3 Evidence classes

| Class | Record | Meaning | Rule |
|---|---|---|---|
| observation | `Observation` (`class: "observation"`) | something a source recorded | carries its source, precision and missing fields |
| derived metric | `Metric` (`class: "derived"`) | computed by a stated rule from observations | value precision `exact` or `derived`; lists its input observations |
| estimate | `Metric` (`class: "estimate"`) | computed with an approximation (for example tokens from bytes) | value precision `estimated` |
| forecast | `Prediction` inside a `Plan` (`class: "forecast"`) | an expected change after an intervention | interval and simulation level `NONE`, `L1`–`L3`; never `L4` |
| confirmed effect | `Effect` (`class: "confirmed_effect"`) | a difference measured on comparable real tasks before and after a change | `evidence_level: "L4"`, control and treatment task sets, number of pairs, interval |

A forecast never becomes an effect by relabeling. An effect needs the measured task sets.

## 6. Repository records

### 6.1 Snapshot

One immutable state of one repository: `snapshot_id`, `repo_id`, `commit_ref` (opaque, `null` for an uncommitted tree), `parent_snapshot_ids`, `captured_at`, `worktree_state`, scanner name and version, `languages` and `digest` (§10). A `repository_snapshot` document holds one or more snapshots (one per repository); every entity, relation and constraint names its snapshot.

### 6.2 Entity

| Field | Values |
|---|---|
| `kind` | `repository`, `artifact`, `directory`, `file`, `module`, `package`, `class`, `interface`, `function`, `method`, `field`, `variable`, `block`, `statement`, `document_section`, `config_node`, `template`, `style_node`, `contract` |
| `parent_id` | containing entity in the same snapshot, or `null` for the repository |
| `id_stability` | `stable` (derived from a stable name) or `snapshot_local` (blocks, statements, positional items) |
| `language` | closed list (`go`, `python`, `typescript`, … `other`) or `null` |
| `flags` | `generated`, `vendored`, `test`, `config`, `entrypoint`, `public_api`, `deprecated` |
| `features` | quantities: `size_bytes`, `lines`, `children`, `fan_in`, `fan_out`, `cyclomatic`, `parameters` |
| `resolution` | `syntax`, `scope`, `type_checked`, `compiler`, `declared`, `unresolved` |

**Resolution is provenance.** Tree-sitter syntax (`syntax`) is not compiler-verified identity (`type_checked`, `compiler`). A consumer MUST NOT treat a syntax-level match as a proven semantic identity.

### 6.3 Relation

| Field | Values |
|---|---|
| `kind` | `contains`, `imports`, `references`, `calls`, `implements`, `inherits`, `control_flow`, `data_dependency`, `contract_ref`, `package_ref` |
| `resolution` | as for entities |
| `evidence` | `tree_sitter`, `scip`, `lsp`, `compiler`, `manifest`, `idl`, `api_spec`, `git` |
| `confidence` | optional probability |

Both endpoints of a relation are entities of the same snapshot.

**Cross-repository contracts.** Two repositories of one namespace share an opaque contract ID only when a local tool proved a common identifier: a package manifest name in a registry (`package_manifest`), an IDL definition such as a fully qualified service name (`idl_definition`), an API specification operation (`api_spec`) or a module path (`module_path`). Each repository then holds a `contract` entity with that ID, and its `contract_ref` or `package_ref` relation carries `contract_evidence` and a resolution of `declared`, `type_checked` or `compiler`. Similar names, equal file names or equal digests are not evidence. A raw dependency name MUST NOT be exported; only its contract ID is (privacy §2).

The multi-repository example ([`multi-repo.json`](../../examples/v0.1/multi-repo.json)) shows a contract shared by two repositories.

### 6.4 PolicyConstraint

A local rule (its text stays local) with `kind` (`build_compatibility`, `api_compatibility`, `test_requirement`, `style_rule`, `custom_rule`), `check` (`deterministic` or `probabilistic`), an optional confidence, the covered entities (`scope`) and `blocks_destructive`. A probabilistic check never authorizes a destructive transformation ([recommendations](recommendation-v0.1.md) §6).

## 7. Changes

A `change_set` document links a before and an after snapshot of one repository (IDs and digests). Each `Change` has:

| Field | Meaning |
|---|---|
| `operation` | `ADD`, `MODIFY`, `DELETE`, `MOVE`, `RENAME`, `SPLIT`, `MERGE`, `AMBIGUOUS` |
| `level` | `artifact` (files) or `symbol` (optional, when a matcher ran) |
| `before`, `after` | entity IDs; cardinality per operation below |
| `candidates` | for `AMBIGUOUS` only: at least two scored `before → after` pairs |
| `match_method`, `match_confidence` | `key`, `qualname`, `hash`, `containment`, `similarity`, `git_rename`, `none`; a confidence is required unless the method is `none` |
| `structural_similarity` | optional probability |
| `magnitude` | quantities: `lines_added`, `lines_deleted`, `entities_touched` |
| `deltas` | `signature` (`none`, `compatible`, `incompatible`, `unknown`), `dependency` (`added`/`removed` quantities), `control`, `data` |
| `provenance` | `git_diff`, `entity_matcher`, `scip` or `compiler`, with tool and version |

| Operation | `before` | `after` |
|---|---|---|
| `ADD` | 0 | 1 |
| `DELETE` | 1 | 0 |
| `MODIFY`, `MOVE`, `RENAME` | 1 | 1 |
| `SPLIT` | 1 | ≥ 2 |
| `MERGE` | ≥ 2 | 1 |
| `AMBIGUOUS` | any | any, with ≥ 2 candidates |

A `MOVE` MUST NOT be written as `DELETE` + `ADD` when the producer has matching evidence. When two matches are plausible, the producer MUST write `AMBIGUOUS` and MUST NOT pick one. Snapshots are immutable; a correction is a new snapshot.

## 8. Activity records

### 8.1 Observation

| Field | Meaning |
|---|---|
| `op` | `SESSION`, `TURN`, `TOOL`, `READ`, `SEARCH`, `DISCOVER`, `EDIT_INTENT`, `FILE_MUTATION`, `EXECUTE`, `TEST`, `BUILD`, `VALIDATION`, `COMPACTION`, `USAGE`, `DELEGATE`, `REQUEST`, `RESPONSE`, `WAIT`, `ERROR`, `UNKNOWN`; optional `ops_secondary` |
| `source` | `otlp`, `native_hook`, `plugin`, `log`, `shell_parser`, `watcher`, `orchestrator`, `transport`; `source_id` names the envelope source |
| `precision` | `exact`, `derived`, `inferred`, `estimated`, `unknown` |
| `time` | `start`, `end` (each may be `null`), `clock` (`agent`, `collector`, `server`, `unknown`), optional `clock_domain` |
| `correlation` | optional `task_id`, `session_id`, `turn_id`, `tool_call_id`, `trace_id`, `span_id`, `parent_span_id`, `parent_observation_id` |
| `targets` | entity IDs (optionally with a line range) or scope IDs |
| `outcome` | `success`, `failure`, `denied`, `cancelled`, `unknown`; optional `exit_code` |
| `size` | quantity in bytes or tokens, with its method |
| `usage` | only for `USAGE`: `input_tokens`, `output_tokens` (required), optional cache and reasoning tokens; each a quantity |
| `command_class` | required for `EXECUTE`: `READ`, `WRITE`, `SEARCH`, `LIST`, `TEST`, `BUILD`, `VCS`, `PACKAGE`, `NETWORK`, `MIXED`, `UNKNOWN` |
| `mutation_attribution` | required for `FILE_MUTATION`: `none`, `inferred`, `exact` |
| `content_delivery` | required for `READ`: `full`, `partial`, `unknown` |
| `coordination` | required for `DELEGATE`, `REQUEST`, `RESPONSE`, `WAIT`: sender, recipient, owning repository, request ID, wait evidence, conflict IDs, concurrent edit scope |
| `missing` | the fields that the source could not provide for this record |
| `provenance` | `captured_at`, optional `source_event_id`, `valid_from`, `valid_to`, and the transformation history (`normalize`, `redact`, `coarsen`, `aggregate`, `time_correction` with tool and version) |

Evidence rules that the schema enforces:

- A `FILE_MUTATION` from a `watcher` never has `mutation_attribution: exact`. A watcher sees a change; it does not prove which agent made it.
- An observation from the `shell_parser` has precision `inferred` or `unknown` and never `content_delivery: full`. A read inferred from `cat` does not prove that the model received the bytes.
- A `WAIT` needs `wait_evidence: blocking_wait_observed`. A reply alone is not a blocking wait.
- A missing session, an unknown command and unavailable token usage are written as missing fields, `UNKNOWN` and unknown quantities ([`activity-trace.json`](../../examples/v0.1/activity-trace.json)).

**Time.** Timestamps are RFC 3339 UTC. Timestamps from different clock domains are not globally ordered; a consumer MUST NOT order them without clock synchronization evidence. A corrected time is a new value with a `time_correction` step; the original record stays in the append-only stream.

### 8.2 Metric

A `Metric` is a value computed locally from observations: `metric`, `unit`, `scope` (`task`, `session`, `turn`, `repository` and its ID), `value` (a quantity), `inputs` (observation IDs) and `definition_version`. Each metric has one unit:

| Metric | Unit | Counts |
|---|---|---|
| `model_tokens`, `input_tokens`, `output_tokens` | tokens | model usage reported by the model server or harness |
| `context_bytes` | bytes | bytes returned to the agent by reads and searches |
| `context_tokens` | tokens | an estimate of the above in tokens (class `estimate`) |
| `navigation_ops`, `navigation_repeats` | count | read, search and discovery operations; repeated ones |
| `boundary_crossings` | count | consecutive reads in different components |
| `change_spread` | count | distinct components edited |
| `rework_edits` | count | edits of an already edited target after a failed validation |
| `validation_runs`, `validation_failures` | count | test and build runs; failed ones |
| `tool_work_seconds` | seconds | the sum of tool durations |
| `task_elapsed_seconds` | seconds | time between the task boundaries |
| `success_rate` | ratio | share of tasks with a verified outcome |
| `coordination_waits`, `handoffs` | count | observed blocking waits; handoffs between agents |

`tool_work_seconds` and `task_elapsed_seconds` are different metrics; a producer MUST NOT derive one from the other.

## 9. Validation

### 9.1 Schema level

A document is schema-valid if it validates against `urn:hint:schema:v0.1:envelope` with all schema files of `schemas/v0.1/` loaded. Any JSON Schema 2020-12 validator can do this. Unknown fields are invalid at every level (closed objects); there is no extension field in v0.1.

### 9.2 Semantic level

A schema-valid document is valid if it also passes these rules. The codes are used by the conformance suite and by receipts.

| Code | Rule |
|---|---|
| `digest_mismatch` | `content_digest` equals the digest of the canonical payload (§10) |
| `snapshot_digest_mismatch` | each snapshot `digest` equals its snapshot digest (§10) |
| `content_forbidden` | every string and key uses only `A–Z a–z 0–9 _ . : + -` (at most 80 characters) and contains no canary value of the conformance suite |
| `repository_not_declared` | every repository ID in the payload is in `repositories` |
| `record_type_not_declared` | every record type present is in `redaction.record_types` |
| `policy_violation` | `aggregate` profile has no observation records; with a given export policy, the policy ID, profile and time resolution match, and every present field is in the policy allowlist |
| `time_resolution` | with `time_resolution_seconds` ≥ 1, every timestamp is a multiple of it |
| `time_order` | `end` ≥ `start`; a plan's `expires_at` > `created_at` |
| `unsorted`, `duplicate_id` | entities, relations and constraints are sorted by (`snapshot_id`, ID) without duplicates; record IDs are unique |
| `dangling_reference` | references resolve inside the document: entity parents, relation endpoints and constraint scopes in the same snapshot; observation sources in `sources`; metric inputs in the observations; plan step and precondition targets in the plan targets |
| `plan_snapshot_set` | each plan has exactly one snapshot for each repository in `repo_ids`: no repository without a snapshot, no repository with two snapshots, no snapshot of a repository outside `repo_ids` |
| `namespace_mismatch` | each plan's `project_namespace` equals the envelope namespace |
| `step_graph` | step IDs are unique; a step depends only on steps listed before it |
| `interval_order` | `lower` ≤ `upper` in intervals and bounds; a known value lies inside its bounds |
| `coverage_inconsistent` | `covered` ≤ `total`; `partial` has `covered` < `total`; `known` with coverage has `covered` = `total` |

## 10. Canonical form and digests

- Canonical form: RFC 8785 JSON Canonicalization Scheme (JCS).
- Digest: `"sha256:" + hex(SHA-256(JCS(value)))`.
- `content_digest` = digest of `payload`.
- **Snapshot digest** = digest of `{"snapshot": <snapshot record without "digest">, "entities": [...], "relations": [...], "constraints": [...]}`, where each list holds the records of that snapshot in payload order. Because records are sorted (§9.2), a client that scans the same tree computes the same digest; a recommendation names this digest to detect a stale snapshot.
- Test vectors: [`tests/conformance/vectors/digests-v0.1.json`](../../tests/conformance/vectors/digests-v0.1.json).

## 11. Conformance

The conformance suite ([`tests/conformance/README.md`](../../tests/conformance/README.md)) holds valid and invalid fixtures, identifier and digest vectors, client import cases, and two independent checkers (Go reference checker; Python check with other libraries). A third-party implementation is conformant if it accepts every valid fixture and rejects every invalid fixture at the stated level.
