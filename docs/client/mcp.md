# git-hint MCP server

`git hint mcp` runs an MCP server on stdio for a coding agent. It calls the same services as the CLI commands: the same collector, export validation, API client, recommendation import and plan renderer. It is local: there is no remote MCP server.

## 1. Protocol

| Item | Value |
|---|---|
| Transport | stdio (newline-delimited JSON-RPC). stdout carries MCP messages only; diagnostics go to stderr. |
| SDK | official MCP Go SDK `github.com/modelcontextprotocol/go-sdk` v1.8.0 |
| Protocol versions | 2025-03-26, 2025-06-18, 2025-11-25, 2026-07-28; the server uses the client's version when it is one of these |
| Tested clients | OpenCode 2.0.26 (negotiates 2025-11-25), macOS and Linux; MCP Inspector 2.8.0 (CLI mode) |

The server writes each client name, client version and protocol version to `.hint/state/mcp.log`.

## 2. Tools

Every tool description starts with its side effect. Every result carries `side_effect` (`none`, `local_write` or `network_upload`) and, where it applies, `schema_version` and snapshot digests.

| Tool | Input | Result | Side effect |
|---|---|---|---|
| `hint_status` | — | repositories with snapshot ID and digest, activity sources and their gaps, server configured, MCP upload rule, privacy profile, queued packages | none |
| `hint_collect` | `full`, optional `task_id` | per repository: snapshot ID and digest, changed, files, entities, relations, file changes; missing capabilities | local write (`.hint/` only) |
| `hint_export_preview` | — | validity, documents and sizes, record counts, fields removed by the policy, profile | none |
| `hint_recommend` | — | `approval_required` (nothing sent) or `sent` with plan summaries: status, affected repositories, forecasts marked as forecasts | network upload, only with the user's rule |
| `hint_plan` | optional `recommendation_id`, `output_path` | validation result, plan summaries, Markdown path and text | local write (one plan file) |
| `hint_import_recommendation` | `path`, optional `output_path` | as `hint_plan` (file mode) | local write |
| `hint_changes` | optional `limit` | changes between the last two snapshots, with local names | none |
| `hint_evidence` | optional `recommendation_id` | reason codes and evidence counts, simulation level, calibration, forecasts, warnings, local collection coverage | none |
| `hint_compare` | optional `recommendation_id` | alternatives by intervention class; `available: false` when the recommendation offers one class only | none |
| `hint_find_definition` | `name` (for example `Rate`, `tax.Rate`, `Cart.Total`), optional `repository` | file, line, kind and qualified name of each matching declaration | none |
| `hint_importers` | `package` (directory relative to the repository root), optional `repository` | files that import the package | none |
| `hint_file_outline` | `path`, optional `repository` | declarations of the file with kinds and lines | none |

The three navigation tools answer from the local index of the last collected snapshot. They read no file content and write nothing. Declarations exist for Go only; other languages are at file level. Lines can be out of date after edits; `hint_collect` refreshes the index.

`git hint mcp --tools <names>` serves only the listed tools (comma-separated), for example a read-only navigation set: `--tools hint_find_definition,hint_importers,hint_file_outline`.

No tool runs a shell command, changes source files, or executes a plan step. A successful tool call is not an accepted change: review the plan and run the checks before any code change.

## 3. Upload rule

`hint_recommend` sends data only when **the user** allowed it in the local configuration:

```sh
git hint init --mcp-upload allow     # or deny (default)
```

The rule is in `.hint/state/project.json` (`mcp.upload`). No tool can change it. The tool has no arguments: the agent cannot choose the server, the record types or the approval. It uses the configured `server_url` only (not `HINT_SERVER_URL`) and the local export policy. With `deny` the result is `approval_required` and nothing leaves the machine. The user can then inspect and send with the CLI (`git hint export --dry-run --privacy-report`, `git hint recommend`).

## 4. Paths

- `output_path` must be a `.md` file inside a project repository, not below `.git/` or `.hint/`, with no symbolic link in the path (also a dangling one). An existing file is replaced only if it is an earlier HINT plan. The plan is written to a new temporary file and renamed into place; the write never follows a link. Without `output_path`, the plan goes to `.hint/state/recommendations/`.
- `path` of `hint_import_recommendation` must be inside a project repository after symbolic links are resolved. The file limit is 16 MiB.

## 5. Limits and errors

- Collection and plan validation stop after 10 minutes, network operations after 5 minutes. A cancelled or stopped operation leaves the local state as it was, or complete.
- Writing tools take a project lock. A second writer gets `busy`; CLI commands wait for the lock.
- The Markdown text in a result is limited to 64 KiB (the file is complete); change lists to 500 entries per repository.
- Tool errors carry a code and a fixed sentence, never a file name, a path or source text: `not_initialized`, `not_excluded`, `key_location`, `busy`, `cancelled`, `timeout`, `no_snapshot`, `policy_invalid`, `document_invalid`, `server_unconfigured`, `network_queued`, `server_rejected`, `receipt_invalid`, `recommendation_missing`, `invalid_argument`, `path_not_allowed`, `too_large`, `internal`.
- `task_id` is stored only as an opaque ID on file-change observations. Do not invent one.

## 6. Client configuration

Generic stdio configuration: command `git-hint`, arguments `mcp`, working directory the project root (or `mcp --project <dir>`).

**OpenCode 2.x** (tested with 2.0.26), project `opencode.json`:

```json
{
  "mcp": {
    "servers": {
      "githint": { "type": "local", "codemode": false, "command": ["git-hint", "mcp"] }
    }
  }
}
```

OpenCode names the tools `githint_hint_status`, `githint_hint_collect` and so on. Notes from the tests:

- Use the `mcp.servers` form. OpenCode 2.0.26 migrates the older form `"mcp": {"<name>": {...}}` through its version 1 schema, which drops `codemode`; the tools then appear only through Code Mode.
- OpenCode connects MCP servers asynchronously. The first model request of a session can lack the HINT tools; they are available from the next request.

Claude Code and Codex configurations are not published before they are tested.
