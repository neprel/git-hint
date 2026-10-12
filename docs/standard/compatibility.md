# HINT compatibility and mappings v0.1

**Version:** 0.1.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-11

## 1. Versioning

- The specification uses semantic versioning. `hint_version` in each document names the version it follows.
- Before 1.0, a minor version can change fields. A reader accepts a document if it supports the document's major.minor version (`0.1`). A patch version (`0.1.x`) changes only text, examples or tests, never the schemas' accepted set.
- A reader rejects a document of an unsupported minor version (`unsupported_version`). It does not try to read it partly.
- Unknown fields are invalid at every level. A producer emits only the fields of the version it declares. Closed objects are a privacy property: an unknown field could carry content.
- The CLI and the server each publish the list of `major.minor` versions they accept. For v0.1 the list is `["0.1"]`.
- Downgrade: a producer that supports a later version writes a v0.1 document by leaving out the fields that v0.1 does not define. It never maps a later field into another v0.1 field.
- The change log is [`CHANGELOG.md`](CHANGELOG.md).

## 2. Chosen design

The activity part is **OpenTelemetry-compatible**: the observation model follows OTLP spans (trace, span, parent span, start and end time, status) and W3C Trace Context identifiers, so a span maps to an observation without loss of correlation (§3). The **repository structure, changes, metrics and recommendations are HINT extensions**, because OTLP has no snapshot, entity, relation or recommendation model. HINT documents are JSON validated by JSON Schema, not OTLP payloads, because OTLP attributes are open key-value maps and cannot enforce the no-free-text rule ([privacy](privacy-v0.1.md) §1).

## 3. OTLP span → HINT observation

| OTLP / GenAI convention | HINT field | Rule |
|---|---|---|
| `traceId`, `spanId`, `parentSpanId` | `correlation.trace_id`, `span_id`, `parent_span_id` | HMAC with the project key ([hint-data](hint-data-v0.1.md) §4.1); W3C formats kept; a parent uses the trace-id of its span; source IDs are never exported; an invalid source ID gives a `missing` entry |
| span identity (`traceId`, `spanId`) | `observation_id` | `DeriveID(K, "event", source_id, T, S)` with lowercase `T` and `S` ([hint-data](hint-data-v0.1.md) §4.1); spans of two traces with the same span-id give two observations |
| `startTimeUnixNano`, `endTimeUnixNano` | `time.start`, `time.end` | RFC 3339 UTC; `clock: agent` |
| `status.code` 0 / 1 / 2 | `outcome.status` `unknown` / `success` / `failure` | `status.message` is dropped (it can hold paths) |
| `gen_ai.operation.name = execute_tool` with `gen_ai.tool.name` | `op` from the tool table: `read` → `READ`; `glob`, `grep` → `SEARCH`; `list` → `DISCOVER`; `edit`, `write`, `patch` → `EDIT_INTENT`; `bash` → `EXECUTE`; `task` → `DELEGATE`; other → `TOOL` | the tool name itself is not exported |
| `gen_ai.operation.name = invoke_agent` | `op: TURN` | |
| `gen_ai.operation.name = chat` | `op: USAGE` | |
| other spans | `op: UNKNOWN` | |
| `gen_ai.conversation.id` | `correlation.session_id` | opaque ID derived with the project key |
| `gen_ai.tool.call.id` | `correlation.tool_call_id` | opaque ID derived with the project key |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | `usage.input_tokens`, `usage.output_tokens` | a reported 0 stays a known 0; an absent attribute is `unknown` (`not_supported`) |
| `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result`, `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, all other attributes, events, links, resource attributes | — | dropped; the observation is built from this allowlist, not by stripping a copy |
| OpenCode native span `Tool.execute` with `tool.name`, `session.id`, `tool.call_id` | as the GenAI rows | observed in OpenCode 1.18.15 captures |

`EXECUTE` from OTLP has `command_class: UNKNOWN`: the command string is content; only a local shell parser can classify it. `READ` from OTLP has `content_delivery: unknown` and an unknown size.

The test `TestOTLPSpanMapping` applies this table to a synthetic GenAI fixture with canary values and to a recorded, scrubbed OpenCode 1.18.15 capture. The output validates, and no canary, path, host or native ID appears; no source trace-id or span-id (also from links) appears. The test also checks that the mapping is deterministic, that each `parent_span_id` equals the exported `span_id` of its parent, and that another key gives other IDs. The OpenCode capture shows the mapping only; it does not confirm OpenCode 2.0.26.

**GenAI convention version.** OpenTelemetry GenAI semantic conventions, repository `open-telemetry/semantic-conventions-genai`, `schema_url` `https://opentelemetry.io/schemas/gen-ai-dev/1.42.0-dev`. Every `gen_ai.*` attribute used here has status `development`; the mapping can change with the conventions.

## 4. Other standards

| Standard | Use in v0.1 |
|---|---|
| W3C Trace Context | trace and span identifier formats; source IDs are pseudonymized before export (hint-data §4.1); propagated through MCP `params._meta` when an agent supplies it |
| OpenInference | input only: span kinds `TOOL` and `AGENT` map like `execute_tool` and `invoke_agent`; HINT does not emit OpenInference. OpenInference captures content by default, so its content attributes are dropped |
| SCIP | local input for symbol resolution (`evidence: scip`); raw SCIP indexes are never exported |
| MCP, A2A | protocol request IDs become opaque `coordination.request_id`; message content is never copied |
| JSON Schema 2020-12 | validation of all documents |
| RFC 8785 (JCS) | canonical form for digests |
| Protocol Buffers | not used in v0.1; a later transport option once the JSON form is stable |

## 5. Platforms

The format is platform-neutral. The conformance suite runs on macOS and Linux. Windows is not tested.
