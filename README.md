# git-hint

HINT is a local client and an open data format. The client collects repository structure and AI coding agent activity, removes content and names, and exchanges documents with a HINT server. It validates the recommendations that the server returns, resolves their identifiers on the local machine and shows them to the user.

The client is not released yet. This repository currently publishes the data contract.

## HINT data standard (draft v0.1)

| Part | Location |
|---|---|
| Specification: data model, privacy, recommendations, exchange, compatibility | [`docs/standard/`](docs/standard/README.md) |
| JSON Schemas 2020-12 | [`schemas/v0.1/`](schemas/v0.1/) |
| Examples (synthetic) | [`examples/v0.1/`](examples/v0.1/) |
| Conformance suite | [`tests/conformance/`](tests/conformance/README.md) |

Status: draft; not final.

## Exchange with a server

The client and the server exchange the same documents in two modes ([exchange](docs/standard/exchange-v0.1.md)):

- **File mode.** The client writes documents to files. The user moves them to the server and moves the returned recommendation files back.
- **API mode.** The client sends documents to `POST /hint/v0.1/documents` and fetches recommendations from the server. Set the server base URL in the client configuration. There is no default server.

The user inspects and approves every export before it leaves the machine ([privacy](docs/standard/privacy-v0.1.md) §6).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
