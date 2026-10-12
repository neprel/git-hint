# HINT privacy contract v0.1

**Version:** 0.1.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-11

**The standard does not guarantee anonymity.** It removes content and names. Structure, timing and sizes still carry information (§4). The user decides what leaves the machine (§6).

## 1. Default policy: deny free-form content

A HINT document has no free-form text field. This is a property of the schemas, not a filter:

- every string is an opaque hex identifier, a digest, a timestamp, a version or a closed enumeration value;
- every object is closed (`additionalProperties: false`); there is no extension or comment field;
- a version pre-release label is limited to `alpha`, `beta`, `rc` or `dev` and a number.

The conformance suite checks this property on the schemas themselves (`TestNoFreeFormStringSlot`) and by mutation: every string of every valid fixture is replaced with a sample of each forbidden class below, and each sample is added as an extra field to every object; the schema must reject every variant (`TestForbiddenContentInEverySlot`).

**Limit: the schema checks the form, not the origin.** A validator sees that an identifier has 32 hex characters. It cannot see whether the producer derived it with the HMAC rule (§3). A secret, a key or a raw value that has the form of an identifier, a digest, a timestamp or a number passes the schema and the semantic rules. Examples: an API key of 32 hex characters in an `entity_id`; a commit hash in a `commit_ref`; data encoded into exact sizes or timestamps. The absence of free-text fields therefore does not prove the absence of leaks. It removes one large class of leaks: text that a producer copies without transformation.

**Producer obligations.** The privacy of a document depends on the producer. Before export, a conforming producer MUST:

- derive every identifier with the HMAC rule of §3 from the project key, also trace, span and parent span IDs (hint-data §4.1); it never copies a native ID, a trace or span ID, a hash, a key or any raw value into an identifier field;
- compute every digest from the canonical HINT value that the standard defines (hint-data §10), never from source content;
- build each record from an allowlist of fields, never by stripping a copy of a raw record;
- apply the export policy and profile (§5) and show the package to the user (§6).

A consumer cannot verify these obligations from the document alone.

## 2. Forbidden content classes

| Class | Examples | How the format blocks it | Negative fixtures |
|---|---|---|---|
| Source code, comments, string literals, raw AST token text | function bodies, `// …`, `"…"` | no content field; strings limited to tokens | `content-source-code` |
| Identifiers and names | function, class, file names | names exist only as opaque IDs | `content-identifier-name` |
| Paths | `src/…/main.go`, `/home/…`, `C:\…` | IDs are 32 hex characters | `content-path-as-id` |
| Prompts, messages, assistant answers, chain-of-thought | user prompt, model reply | no message field | `content-prompt`, `content-assistant-answer` |
| Shell command strings and terminal output | `cat … \| grep …`, test output | `EXECUTE` carries only `command_class` | `content-shell-command`, `content-terminal-output`, `content-step-command` |
| Environment variables and credentials | `NAME=value`, keys, tokens | no field | `content-env-var` |
| URLs, hostnames, repository remote names | `https://…`, `build01.…`, `origin` | no field; repositories are opaque IDs | `content-remote-url`, `content-hostname` |
| User identity and author emails | `jdoe`, `dev@…` | no field | `content-author-email` |
| Branch names and commit messages | `feature/…`, message text | commits are opaque `commit_ref` IDs | `content-branch-name`, `content-commit-message` |
| Raw third-party package names | `@private-org/billing-core` | dependencies appear only as contract IDs with evidence | `content-package-name` |
| Free-text reasons or labels | "split because …" | reasons and versions are enumerations | `content-free-reason`, `content-version-label`, `content-policy-field-name` |

A server MUST reject raw SCIP indexes, source text, file paths and prompts in normal ingestion. The schema rejects them when they appear as text. It cannot detect them when a producer encodes them into a value of a valid form (§1, limit).

## 3. Opaque identifiers and keys

- Each project has a secret key `K` (32 random bytes). It stays in the local protected store with the name mapping (file mode `0600` or the OS key store). It is never exported.
- Identifiers are `HMAC-SHA256(K, domain-separated input)` truncated to 128 bits ([hint-data](hint-data-v0.1.md) §4). Trace IDs are truncated to 128 bits and span IDs to 64 bits, so they keep the W3C formats (hint-data §4.1). Without `K`, an identifier does not reveal its input, and a dictionary attack needs `K`.
- Source trace and span IDs stay local. A source trace-id can also exist in other systems (agent telemetry, MCP servers, logs); a copied ID would link a HINT document to those records.
- A document MUST NOT contain a dictionary that maps an identifier back to a name. The resolution of identifiers to local names happens only on the client ([exchange](exchange-v0.1.md) §4).
- HMAC identifiers are pseudonyms: the same key links all documents of a project. Rotating the key gives new, unlinkable identifiers and breaks stable IDs.
- Equal digests in two repositories of one namespace show equal input under one key. They do not prove a runtime contract; only contract evidence does ([hint-data](hint-data-v0.1.md) §6.3).

## 4. Residual risks

| Risk | How it discloses information | Mitigation in v0.1 |
|---|---|---|
| Structural fingerprint | The graph shape (files per directory, call fan-in/out, nesting) of a known codebase can be matched without names. Vendored or open-source code is easy to recognize. | flag `vendored` and `generated`, exclude them in the policy; `aggregate` profile |
| Membership inference | A party that holds a candidate repository can scan it and compare the structure with an exported snapshot. | profiles; the user approves each export; no guarantee |
| Rare subgraphs | A small, unusual subgraph (a rare call pattern, an unusual nesting) can identify a project or component. | `aggregate` profile; fewer record types |
| Exact magnitudes | Exact file sizes, line counts and token counts can identify known files and releases. | omit `features` fields via the policy allowlist; `aggregate` profile |
| High-resolution timestamps | Times show working hours, time zone, team size and the order of actions. | `coarse` profile with a time resolution of 60 s, 1 h or 1 day |
| Agent and version data | `agent`, `agent_version` and capability lists show the tool chain. | required for interpretation; the user can see them before export |
| Small repositories | Few entities make every other risk stronger. | the user decides; `aggregate` profile |
| Misused value slots | A faulty or hostile producer puts a secret or raw value of a valid form into an identifier, digest, timestamp or number. | producer obligations (§1); the user inspects the package (§6); no validator can detect it |

## 5. Export profiles

| Profile | Contents | Time resolution |
|---|---|---|
| `detailed` | all record types the policy allows | any (`0` = as recorded) |
| `coarse` | as `detailed`, timestamps truncated | 60, 3600 or 86400 s; enforced by the semantic rule `time_resolution` |
| `aggregate` | no `Observation` records; metrics without input references | as declared |

The export policy (`export_policy` document) is an allowlist: record types and, for each record type, the top-level fields that may be exported. A record type that the policy does not name is not exported. The envelope declares the policy ID, profile, record types and time resolution; a consumer checks them (semantic rules `record_type_not_declared`, `policy_violation`, `time_resolution`).

## 6. Inspection and approval

- A producer MUST let the user inspect an export package before it leaves the machine, in file mode and in API mode.
- The package is the same document in both modes, so what the user inspects is what is sent.
- The user can choose a stricter profile or policy, or decline the export.

## 7. What the standard does not claim

- It does not claim anonymity or resistance to a party with background knowledge of the code.
- It does not claim that a schema-valid document contains no secret. Validation proves the form of each value, not its origin.
- It does not claim that structural data is harmless.
- It does not protect data on the local machine; the local store and key are the user's responsibility.
