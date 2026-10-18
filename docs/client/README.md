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
| Declare that the last change applied a plan | `git hint changes --declare-plan PLAN [--from SETFILE]` |
| Check the state of the store and the adapters | `git hint status`, `git hint doctor` |
| Free disk space | `git hint cache clean` (keeps keys, mapping and state) |

`collect` reads and hashes every file, and parses only files whose content changed since the last scan. Size and modification time never decide alone, so an edit that keeps both is still found. A full scan (`--full`) gives the same snapshot. An unchanged tree keeps the same snapshot ID and digest, so a recommendation for it stays valid.

`collect` also records each file that changed since the last snapshot as a file change observation. A file change is linked to an agent tool call only when exactly one edit of that file by the agent happened in that time window. Otherwise the change stays unassigned.

## 5. Agent activity (OpenCode)

```sh
git hint adapter install opencode     # writes .opencode/plugins/hint-adapter.ts
```

OpenCode loads the plugin. The plugin starts `git-hint adapter opencode` and sends it reduced tool events through a pipe. Tool output, edit text, search patterns and prompts never leave the OpenCode process; the plugin replaces output by its byte length. The child process writes one observation per tool call to `.hint/state/activity/spool.jsonl`. If `git-hint` is not on `PATH`, set `HINT_BIN` to its path. Set `HINT_DISABLE=1` to turn the adapter off. The plugin never blocks or breaks OpenCode: without the child it drops and counts events. The plugin itself writes no file; its counters go through the child. The child checks the Git exclusion of `.hint/` before every record. If the exclusion breaks during a session (a rule is removed, or a file below `.hint/` enters the Git index), the child stops writing for the rest of the session and tells the plugin; later events are counted as dropped in memory, and OpenCode continues.

**Context measurements.** For the HINT tool `hint_context` (OpenCode name `githint_hint_context`; set `HINT_MCP_SERVER` when your MCP server entry has another name), the plugin also reads one metadata line of the answer: the separate text part that starts with `hint-context-measurement/1 `. It holds only enumerations, numbers and opaque IDs. The plugin and the child validate it strictly; a missing or damaged line is recorded as missing context, never as a zero. The call then gives two records with one tool call ID: the answer as the tool sent it (`raw_tool_output`) and the text the hook received (`delivered_context`). Delivery to the hook does not prove that the model used the text. A package with such records is exported as HINT v0.2. This needs OpenCode 2.0.26 or later with the MCP server in the `mcp.servers` form and `codemode: false` (OpenCode 2.0.22 offers MCP tools only through Code Mode, so the plugin never sees a `hint_context` call). OpenCode connects MCP servers asynchronously: the first model request of a session can lack the HINT tools.

**Declared tasks (orchestrators).** Findings work per task. An orchestrator that starts one agent run per task sets `HINT_TASK_ID=<its task id>` in the agent's environment. The adapter then stamps every record of that run with the task, under the same opaque ID that `hint_collect` with `task_id` gives for the same value; the raw value stays on the machine. An empty value is no declaration. An invalid value (control characters, more than 200 bytes) stops the adapter before it writes anything. One run is one declared task: to continue a long-running agent with another task, start a new run (or integrate explicitly); HINT never splits a run by time and never treats a session as a task. Without a declaration, records have no task, and the server reports "not diagnosed: no confirmed tasks".

**Declared family and configuration (optional).** With `HINT_TASK_ID`, an orchestrator can also set `HINT_TASK_FAMILY=<label>` (the family of that task; a declaration, not a classification), `HINT_MODEL=<model name>` (the model the user declared; kept as provenance, because the adapter cannot see the model that actually ran) and `HINT_CONFIG_REFS=<kind>:sha256:<hex>,...` (digests of the observed `setting_override` or `dependency_lock` files, or `none` for a known empty set). The values become project-scoped references with the same derivations as eval reports; the raw values stay on the machine. Any of them makes the source carry its configuration (another source identity) and makes the export HINT v0.2. An invalid value stops the adapter before it writes anything. Without these variables nothing changes.

**Eval objectives and tasks.** `git hint eval report` gives a trial the declared task of its eval only when the binding is unambiguous: the eval's episode names exactly one OpenCode session, that session has exactly one episode, and every adapter record of that session carries the same declared task. A variant takes the task of its exact baseline version. Mixed, missing or conflicting evidence gives no task. The server uses the task to bind eval comparisons to the findings of the same tasks.

**Context policy.** `git hint init --context-limits depth=1,items=20,bytes=16384,ms=1000` sets the local defaults of `hint_context` as a new policy version; `--context-limits off` turns them off; `--context-limits restore=N` brings back version N. Every version stays in `.hint/state/project.json` with its time and kind of change. Other settings, the key and the permissions do not change. The measurements name the identity of the limits that were in effect for each call (the call's own arguments included).

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

**Conditional forecast (optional).** `git hint recommend --forecast-horizon N` asks the server for a forecast of the model-token change per task over N future tasks (1–100,000). `--forecast-threshold P` sets the decision threshold in (0, 1] (default 0.9, a heuristic). `--assumed-cost LO:HI` adds an assumed one-time cost in tokens; the result is then a labeled scenario and never a recommendation. The threshold and the cost need an explicit horizon. HINT checks the values before it sends anything; an invalid value is a usage error (exit 2). The server answers with a forecast only when your project history passes its calibration check; otherwise the plans stay as without the option. Without a measured cost of comparable refactoring work there is no net value or break-even. The horizon, the threshold source and the scenario label appear in the plan.

**Declared plan application.** After you apply a plan and run `git hint collect`, `git hint changes --declare-plan PLAN` records that this change (the latest snapshot against its parent) applied plan PLAN of the last fetched set (or of `--from SETFILE`). HINT checks the set with the normal import, the plan's repository and that the plan was made for the parent snapshot. It writes one local file below `.hint/state/declarations/` and calls no server. The next export or `recommend` sends the change set with this declaration; the declaration keeps the set, plan and both snapshots even after later changes. A repeat is accepted unchanged; a different plan for the same change is refused. The declaration is an association only: the server checks it against the stored set and the change mappings, and it is never taken as proof that the refactoring is correct or had an effect. A plan over several repositories cannot be declared yet.

## 7. Plans

`git hint plan` checks the recommendation document in a fixed order: version, schema, semantic rules and digests, project namespace, expiry, snapshot digest per repository, identifier resolution, decision and step support. Each plan gets `accept`, `partial`, `render_only` or `reject`. A recommendation for an older state of the work tree gets `snapshot_mismatch`, also before the next `collect`.

The Markdown plan shows local file and symbol names, the steps, the preconditions and the constraints. Forecasts are marked as predictions with their simulation level and calibration status. HINT does not apply any step. Review a plan before you act on it.

When the server sends a HINT v0.2 set, the plan starts with a summary and adds:

- **Findings** (at most five): the problem, the observed quantities with their coverage, the cause hypotheses and their state, the candidate interventions and a next step. An intervention comparison shows only observed differences of eval runs that are bound to the finding; today no finding is bound, so it says to collect more evidence. There is no net value or break-even: no cost model is available yet.
- **Project eval comparisons**: the observed differences of your eval variants, which decide nothing alone.
- **Context policy feedback**: proposals only (keep, revise, disable, or not enough evidence). You change or restore the policy with `git hint init --context-limits`.

The report keeps the kinds of values apart: observed, estimated, predicted and experimentally supported. A set that you received as a file (`git hint plan --from FILE`) renders with the same checks as one fetched with `git hint recommend`.

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
