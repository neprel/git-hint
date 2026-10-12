# HINT data standard

**Current draft:** v0.1.0 | **Status:** draft; not final | **License:** Apache-2.0 (repository license)

The HINT data standard is an open format for **repository structure**, **agent activity**, **changes across time** and **recommendations**. It does not depend on an AI agent, a programming language or HINT code. Its schemas have no field for source code, names, paths, prompts or raw outputs; the privacy contract states what this does and does not prove.

| Document | Content |
|---|---|
| [`hint-data-v0.1.md`](hint-data-v0.1.md) | data model, identifiers, values (unknown, real zero, known part, coverage), evidence classes, validation rules, digests |
| [`privacy-v0.1.md`](privacy-v0.1.md) | forbidden content, opaque identifiers and keys, residual risks, export profiles, approval |
| [`recommendation-v0.1.md`](recommendation-v0.1.md) | plans, steps, forecasts, destructive-change rule, effects |
| [`exchange-v0.1.md`](exchange-v0.1.md) | file mode, API mode, receipts, client import validation |
| [`compatibility.md`](compatibility.md) | versioning, unknown fields, OTLP mapping, other standards |
| [`CHANGELOG.md`](CHANGELOG.md) | changes per version |

| Artifact | Location |
|---|---|
| JSON Schemas (2020-12) | [`schemas/v0.1/`](../../schemas/v0.1/); entry point `urn:hint:schema:v0.1:envelope` in `envelope.schema.json` |
| Examples | [`examples/v0.1/`](../../examples/v0.1/) (synthetic) |
| Conformance suite | [`tests/conformance/`](../../tests/conformance/README.md) |

## Validate a document with any tool

1. Load all files of `schemas/v0.1/` into a JSON Schema 2020-12 validator. Each file declares its `$id` (`urn:hint:schema:v0.1:<name>`).
2. Validate the document against `urn:hint:schema:v0.1:envelope`.
3. Apply the semantic rules of `hint-data-v0.1.md` §9.2. The digest rules need an RFC 8785 implementation and SHA-256.

The conformance suite does this with two independent implementations: a Go reference checker and a Python check that uses other libraries.
