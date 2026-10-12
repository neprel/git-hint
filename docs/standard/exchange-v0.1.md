# HINT exchange v0.1: files and API

**Version:** 0.1.0 (draft) | **Status:** draft; not final | **Date:** 2026-10-11

One document format serves both modes. The API wraps the same documents; it defines no other fields.

## 1. Document

- A document is one envelope ([hint-data](hint-data-v0.1.md) §3), UTF-8 JSON.
- Media type: `application/vnd.hint+json; version=0.1`.
- File name: `<document_type>-<first 12 hex of content_digest>.hint.json`. An export package is a directory of such files. File order has no meaning.
- A receiver validates a document by its content, not by its file name or transport.

## 2. File mode

1. The client builds the documents, shows them to the user ([privacy](privacy-v0.1.md) §6), and writes the approved files.
2. The user moves the files to the server by any means.
3. The server validates each document (schema, then semantic rules) and stores accepted documents unchanged.
4. The server writes `recommendation_set` documents; the user moves them back.
5. The client runs the import validation (§4).

## 3. API mode

**Server address.** The client takes the server base URL from its local configuration, for example `https://hint.example.org`. The paths below are relative to this base URL. There is no default server: without a configured URL the client works in file mode only. The user chooses the server; the client sends nothing before the user approves the export ([privacy](privacy-v0.1.md) §6).

| Method and path | Body | Response |
|---|---|---|
| `POST /hint/v0.1/documents` | one document | `201` with a `receipt` document when accepted; `422` with a `receipt` that lists the rejection code; `415` for another media type |
| `GET /hint/v0.1/documents/{document_id}` | — | `200` with the stored document, byte for byte; `404` if unknown |
| `GET /hint/v0.1/recommendations?project_namespace={id}` | — | `200` with a `recommendation_set` document |

Receipt rejection codes: `unsupported_version`, `schema_invalid`, `digest_mismatch`, `namespace_mismatch`, `policy_violation`, `content_forbidden`, `reference_unresolved`, `duplicate`, `too_large`. A semantic rule without a receipt code of its own, for example `plan_snapshot_set`, gives `reference_unresolved`.

Authentication, transport security and rate limits are deployment matters outside this contract. A server MUST use TLS outside a local test.

## 4. Client import validation

A client imports a `recommendation_set` in the same way in both modes. It runs these checks in order and stops at the first failing document-level check.

| Order | Level | Check | Result on failure |
|---|---|---|---|
| 1 | document | `hint_version` major.minor is in the client's supported list | `unsupported_version` |
| 2 | document | schema validation | `schema_invalid` |
| 3 | document | semantic rules ([hint-data](hint-data-v0.1.md) §9.2), including `content_digest` and the plan snapshot set | the rule code, for example `digest_mismatch` or `plan_snapshot_set` |
| 4 | document | envelope `project_namespace` equals the local namespace | `namespace_mismatch` |
| 5 | plan | now < `expires_at` | plan `reject`, code `expired` |
| 6 | plan | for each repository in `repo_ids`, the plan's one snapshot digest equals the current local snapshot digest of that repository | `reject`, `snapshot_mismatch` (or `repository_unknown`) |
| 7 | plan | every target, step target and destination resolves to exactly one local entity | `reject`, `unresolved_id` or `ambiguous_id` |
| 8 | plan | `decision` is `recommend` | `render_only` (shown, not applied) |
| 9 | plan | every step is `supported` | `partial`, code `unsupported_step` (only supported steps can be applied) |
| — | plan | all checks pass | `accept` |

`accept` means the plan may be shown as applicable. Applying any step still needs the plan's preconditions and approvals. The client never executes a plan as a script.

## 5. Conformance

The conformance suite sends every valid fixture through a file round trip and a mock API round trip and requires byte-identical documents; it sends invalid fixtures to the mock API and requires a `422` receipt; and it runs every client import case through both modes: a document that the client rejects at document level must also get a `422` receipt from the API, and a stored document must give the same result in both modes (`TestFileAndAPIRoundTrip`, `TestImportResolution`).
