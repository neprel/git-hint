# git-hint command reference

Every command says what it reads, what it writes and whether it uses the network. Run `git hint <command> -h` for its options.

| Command | Effects | Purpose |
|---|---|---|
| `git hint init [--repo PATH[=NAME]]... [--server URL] [--mcp-upload allow\|deny]` | writes local state; no network | Create the store, the key and the repository list. Running it again adds repositories and keeps the key. `--mcp-upload` sets whether the MCP tool `hint_recommend` may send data (default deny). |
| `git hint status` | reads local state; no network | Repositories, last snapshots, adapter counters, queued packages, privacy profile. |
| `git hint collect [--full] [--json]` | reads the work trees; writes local state; no network | Scan the repositories; store a new snapshot when they changed; record changed files. `--full` ignores the scan cache. |
| `git hint changes [--format text\|canonical]` | reads local state; no network | Changes between the last two snapshots. `text` shows local names; `canonical` prints the HINT change set without names. |
| `git hint observe` | reads local state; no network | How activity is collected; adapter state. |
| `git hint export [--dry-run] [--privacy-report] [--out DIR] [--all]` | reads local state; writes files; no network | Build and validate the export package. `--dry-run` writes nothing. Default output: `.hint/state/exports/<time>/`. `--all` includes observations that were already exported. |
| `git hint recommend [--yes] [--retry] [--server URL] [--all]` | reads local state; **sends data** to the server | Send the package and fetch the recommendations. Without `--yes` it asks in a terminal and refuses in a script. `--retry` sends queued packages. |
| `git hint plan [--from FILE] [--out FILE] [--json]` | reads local state and work trees; writes a Markdown file; no network | Validate a recommendation and write the local plan. Exit code 1 when the document is rejected. |
| `git hint cache clean` | removes `.hint/cache/` only; no network | Delete rebuildable data. Keys, mapping and state stay. |
| `git hint doctor` | reads local state and tools; no network | Check Git, the store, the key, the schemas, the adapters and optional tools. Starts no agent. |
| `git hint adapter install opencode [--force]` | writes `.opencode/plugins/hint-adapter.ts`; no network | Install the OpenCode adapter plugin. |
| `git hint adapter opencode --project DIR` | reads stdin; writes the activity spool; no network | Started by the OpenCode plugin. Not for direct use. |
| `git hint mcp [--project DIR] [--tools NAMES]` | stdio MCP server; each tool states its effect; `hint_recommend` sends only with the user's rule | Serve the HINT tools to a coding agent. See [mcp.md](mcp.md). |
| `git hint version` | none | Client and data format version. |

## Environment

| Variable | Meaning |
|---|---|
| `HINT_KEY_DIR` | Directory of project keys (default: user configuration directory). It must be outside every repository of the project. |
| `HINT_SERVER_URL` | Server base URL; overrides the configuration. |
| `HINT_SERVER_TOKEN` | Optional bearer token for the server; never stored. |
| `HINT_BIN` | Path of `git-hint` for the OpenCode plugin. |
| `HINT_DISABLE` | `1` turns the OpenCode plugin off. |
| `HINT_AGENT_VERSION` | OpenCode version to record, when known. |
| `HINT_CPUPROFILE` | Write a Go CPU profile to this file (troubleshooting). |

## Local store

```text
.hint/
  cache/                     rebuildable: scan cache
  state/
    project.json             namespace, key ID, repositories, server URL, export policy
    snapshots/<repo>/        snapshots with the local name index
    activity/                observation spool, source list, adapter counters
    exports/                 export history and written packages
    queue/                   packages that wait for a retry
    recommendations/         fetched recommendation documents and Markdown plans
```

Files in `.hint/state/` are owner-only (mode 0600).
