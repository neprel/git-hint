# git-hint client: user guide

**Version:** 0.1.0 (development) | **Data format:** HINT data standard v0.1 ([`docs/standard/`](../standard/README.md))

`git-hint` collects the structure of your repositories and the activity of an AI coding agent on your machine. It removes content and names, and writes canonical HINT documents. It exchanges these documents with a HINT server in file mode or API mode. It validates the recommendations that come back, resolves their identifiers on your machine and writes a Markdown plan. It never changes your code.

Other documents: [command reference](commands.md), [MCP server](mcp.md), [privacy and data audit](privacy.md), [troubleshooting](troubleshooting.md), [sample configuration](project.example.json).

## 1. Requirements

- Git 2.30 or later on `PATH`.
- Go 1.27 or later to build from source.
- Optional: OpenCode 2.x for agent activity (the adapter is checked with OpenCode 2.0.26). Bun is part of OpenCode.
- Platforms: macOS and Linux (arm64, amd64). Windows is deferred and not supported.

## 2. Install

Release archives for macOS and Linux, checksum and provenance checks: [install](install.md). From source:

```sh
go install github.com/neprel/git-hint/cmd/git-hint@latest
```

Git runs `git-hint` as `git hint` when the executable is on `PATH`. The two forms are the same program.

## 3. First run

Run these commands in the root of a Git repository:

```sh
git hint init                    # create .hint/, the project key and the repository list
git hint collect --full          # scan the repository and store the first snapshot
git hint export --dry-run --privacy-report   # see what an export contains; nothing is written
```

`init` adds `/.hint/` to `.git/info/exclude`. Every later write checks its own path and every area of the store with Git, and checks that Git tracks no file below `.hint/`. If one check fails, the write stops.

The project key is not in the work tree. `init` writes it to the user configuration directory (`$XDG_CONFIG_HOME/git-hint/keys/` on Linux, `~/Library/Application Support/git-hint/keys/` on macOS), or to `HINT_KEY_DIR`. Coding agents read the work tree; the key can reverse exported identifiers, so it stays outside. The client refuses a key directory or key file inside any repository of the project, also through a relative path or a symbolic link.

### Several repositories

One project can hold several repositories. The store stays in the first repository:

```sh
git hint init --repo ../shop-web            # add a repository; its name is the directory name
git hint init --repo ../api=billing-api     # add a repository with an explicit local name
```

Repository names are local. Exported documents name repositories only by opaque IDs.

## 4. Daily use

| Situation | Command |
|---|---|
| After a task or a set of changes | `git hint collect` |
| See what changed between the last two snapshots | `git hint changes` |
| Check the state of the store and the adapters | `git hint status`, `git hint doctor` |
| Free disk space | `git hint cache clean` (keeps keys, mapping and state) |

`collect` reads and hashes every file, and parses only files whose content changed since the last scan. Size and modification time never decide alone, so an edit that keeps both is still found. A full scan (`--full`) gives the same snapshot. An unchanged tree keeps the same snapshot ID and digest, so a recommendation for it stays valid.

`collect` also records each file that changed since the last snapshot as a file change observation. A file change is linked to an agent tool call only when exactly one edit of that file by the agent happened in that time window. Otherwise the change stays unassigned.

## 5. Agent activity (OpenCode)

```sh
git hint adapter install opencode     # writes .opencode/plugins/hint-adapter.ts
```

OpenCode loads the plugin. The plugin starts `git-hint adapter opencode` and sends it reduced tool events through a pipe. Tool output, edit text, search patterns and prompts never leave the OpenCode process; the plugin replaces output by its byte length. The child process writes one observation per tool call to `.hint/state/activity/spool.jsonl`. If `git-hint` is not on `PATH`, set `HINT_BIN` to its path. Set `HINT_DISABLE=1` to turn the adapter off. The plugin never blocks or breaks OpenCode: without the child it drops and counts events. The plugin itself writes no file; its counters go through the child. The child checks the Git exclusion of `.hint/` before every record. If the exclusion breaks during a session (a rule is removed, or a file below `.hint/` enters the Git index), the child stops writing for the rest of the session and tells the plugin; later events are counted as dropped in memory, and OpenCode continues.

Claude Code and Codex adapters are not available in this version.

## 6. Exchange with a server

The same documents and the same checks serve both modes ([exchange](../standard/exchange-v0.1.md)).

**File mode** (no network):

```sh
git hint export --out ./hint-package           # write the approved package
# move the files to the server by any means; move the recommendation file back
git hint plan --from ./recommendation_set-....hint.json
```

**API mode:**

```sh
git hint init --server https://hint.example.org   # or set HINT_SERVER_URL
git hint export --dry-run --privacy-report         # inspect first
git hint recommend --yes                           # send, then fetch the recommendations
git hint plan                                      # validate and write the local Markdown plan
```

The server address must use `https`; `http` is allowed only for a loopback address. There is no default server. `recommend` asks for approval in a terminal and refuses to send without `--yes` in a script. An optional bearer token comes from `HINT_SERVER_TOKEN`; HINT never stores it. If the server is unreachable, the package stays in `.hint/state/queue/`; `git hint recommend --retry --yes` sends it later.

## 7. Plans

`git hint plan` checks the recommendation document in a fixed order: version, schema, semantic rules and digests, project namespace, expiry, snapshot digest per repository, identifier resolution, decision and step support. Each plan gets `accept`, `partial`, `render_only` or `reject`. A recommendation for an older state of the work tree gets `snapshot_mismatch`, also before the next `collect`.

The Markdown plan shows local file and symbol names, the steps, the preconditions and the constraints. Forecasts are marked as predictions with their simulation level and calibration status. HINT does not apply any step. Review a plan before you act on it.

## 8. Privacy profile

The export policy is in `.hint/state/project.json` (`policy`). It is an allowlist:

- `profile`: `detailed`, `coarse` (timestamps truncated to `time_resolution_seconds`: 60, 3600 or 86400) or `aggregate` (no activity records);
- `record_types`: the record types that may leave the machine;
- `fields`: optional per record type; the top-level fields that may leave the machine. A policy that removes a required field stops the export.

See [privacy.md](privacy.md) for what an export contains and what it still discloses.

## 9. What is collected

| Source | Content |
|---|---|
| Repository | directories and files from Git (tracked and untracked, not ignored); size and line count; language by file extension; flags (test, config, vendored, generated) |
| Go | declarations (functions, methods, types) with lines, parameters and cyclomatic complexity; imports of packages in the same repository. Parsed with the Go standard library parser (syntax level, no type checking) |
| Other languages | file level only |
| Changes | added, deleted, modified and renamed files; moved, added, deleted and modified Go declarations |
| OpenCode | tool calls: operation, time, outcome, exit code, output size, targets as file IDs, shell command class |

A file that cannot be parsed stays at file level. Optional indexers (SCIP, `sem`) are not used by this version; `doctor` reports whether they are installed.
