# git-hint

HINT is a local client and an open data format. The client collects repository structure and AI coding agent activity, removes content and names, and exchanges documents with a HINT server. It validates the recommendations that the server returns, resolves their identifiers on the local machine and shows them to the user.

The client is in development (version 0.1.0, not released). Platforms: macOS and Linux; Windows is not tested.

## Client

```sh
go install github.com/neprel/git-hint/cmd/git-hint@latest
git hint init && git hint collect --full
git hint export --dry-run --privacy-report
```

| Document | Content |
|---|---|
| [User guide](docs/client/README.md) | install, first run, several repositories, OpenCode adapter, file and API mode, plans |
| [Command reference](docs/client/commands.md) | commands, their effects (read, write, network), environment, local store |
| [MCP server](docs/client/mcp.md) | `git hint mcp` for coding agents: tools, upload rule, limits, OpenCode configuration |
| [Privacy and data audit](docs/client/privacy.md) | what stays local, what leaves, how it is enforced and tested |
| [Troubleshooting](docs/client/troubleshooting.md) | messages and actions |
| [Third-party notices](THIRD_PARTY_NOTICES.md) | licenses of included components |

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
- **API mode.** The client sends documents to `POST /hint/v0.1/documents` and fetches recommendations from the server. Set the server base URL with `git hint init --server <url>` or `HINT_SERVER_URL`. There is no default server.

The user inspects and approves every export before it leaves the machine ([privacy](docs/standard/privacy-v0.1.md) §6).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
