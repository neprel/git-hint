# HINT conformance suite v0.1

Fixtures and checks for the [HINT data standard](../../docs/standard/README.md). All data is synthetic, except one recorded and scrubbed OpenCode 1.18.15 OTLP capture in `otlp/` (scripted mock model; prompts, outputs and tool arguments replaced by length markers). No AI agent or model runs in these tests.

## Layout

| Path | Content |
|---|---|
| `../../examples/v0.1/` | valid example documents (also fixtures) |
| `../../examples/v0.2/` | v0.2 examples: context measurements and the v0.2 export policy; `invalid/context-*.json` are their negative cases |
| `valid/` | more valid documents: coarse and aggregate profiles, a later patch version, edge cases (unseen tool, errors, compaction, coordination with a blocking wait, concurrent edits, time correction) |
| `invalid/` | invalid documents; `invalid/expected.json` gives the level (`schema` or `semantic`) and the semantic code of each |
| `resolve/` | client import validation: local states, recommendation cases and `expected.json` |
| `vectors/` | identifier test vectors (published test keys) and RFC 8785 digest vectors |
| `otlp/` | synthetic GenAI OTLP spans with canary values, and the scrubbed OpenCode capture, for the mapping test |
| `hintconf/` | Go reference checker: schema loading, semantic rules, digests, identifiers, import validation |
| `cmd/hintcheck/` | command-line validator |
| `cmd/genexamples/` | generator of the examples and fixtures (deterministic) |
| `py/check.py` | independent Python check (different validator and RFC 8785 library) |

## Run

```sh
cd tests/conformance
go test ./...                                   # Go reference checks
go run ./cmd/hintcheck ../../examples/v0.1/*.json
python3 -m pip install jsonschema==4.23.0 rfc8785==0.1.4   # in a virtual environment
python3 -I py/check.py ../..                    # independent check
```

After a schema or generator change: `go run ./cmd/genexamples -root ../..`. The test `TestGeneratedFilesAreCurrent` fails while committed fixtures are stale.

## Tests

| Test | What it proves |
|---|---|
| `TestNoFreeFormStringSlot` | every string slot of every schema is an enum, a const or a pattern; every object definition is closed |
| `TestValidDocuments` | all examples and valid fixtures pass the schema and the semantic rules (with the default export policy for detailed documents) |
| `TestInvalidDocuments` | each invalid fixture fails at its stated level; semantic fixtures are schema-valid and fail only their rule |
| `TestSignedDeltasStayValid` | negative forecast intervals, forecast points and effect estimates stay valid; only absolute quantities must be non-negative |
| `TestForbiddenContentInEverySlot` | 20 samples of forbidden content in every string slot and as an extra field of every object are rejected |
| `TestIDVectors`, `TestDigestVectors` | identifier derivation and canonical digests match the vectors; unrelated namespaces and similar names give different IDs |
| `TestStableIDsAcrossRescanAndRename` | symbol IDs stay stable across a file rename and a function move; file IDs change; the change set links them |
| `TestCrossRepoContract` | two repositories share a contract ID only through contract entities with evidence |
| `TestGeneratedFilesAreCurrent` | a rescan (regeneration) gives byte-identical files |
| `TestFileAndAPIRoundTrip` | file mode and mock API mode give byte-identical documents; invalid documents get a `422` receipt |
| `TestImportResolution` | client import validation gives the expected status and codes in file and API mode; a document that the client rejects also gets a `422` receipt |
| `TestOTLPSpanMapping` | the OTLP mapping table gives valid observations without content and without source trace or span IDs; the ID mapping is deterministic, keeps parent links and changes with the key |
| `TestSpanObservationIDIncludesTrace` | two traces with the same source span-id give two observations with different IDs; the document passes the duplicate rule |
| `TestTraceAndSpanIDDerivation` | trace and span IDs keep the W3C formats, normalize case, differ by key and by trace, and reject invalid or all-zero source IDs |

## Libraries

| Library | Version | License | Use |
|---|---|---|---|
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | Apache-2.0 | JSON Schema validation (Go) |
| `github.com/cyberphone/json-canonicalization` | `19d51d7fe467` (2024-12-13) | Apache-2.0 | RFC 8785 (Go) |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause | dependency of the validator |
| `jsonschema` (Python) | 4.23.0 | MIT | independent check |
| `rfc8785` (Python) | 0.1.4 | Apache-2.0 | independent check |
