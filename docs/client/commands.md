# git-hint command reference

Every command says what it reads, what it writes and whether it uses the network. Run `git hint <command> -h` for its options.

| Command | Effects | Purpose |
|---|---|---|
| `git hint init [--repo PATH[=NAME]]... [--server URL] [--mcp-upload allow\|deny] [--mcp-eval-run allow\|deny]` | writes local state; no network | Create the store, the key and the repository list. Running it again adds repositories and keeps the key. `--mcp-upload` sets whether the MCP tool `hint_recommend` may send data (default deny); `--mcp-eval-run` whether `hint_eval_run` may run eval trials (default deny). |
| `git hint status` | reads local state; no network | Repositories, last snapshots, adapter counters, queued packages, privacy profile. |
| `git hint collect [--full] [--json]` | local repository scanning by default; configured eval processing can use the network and containers | Scan the repositories; store a new snapshot when they changed; record changed files. Without `--json`, enabled evals also run their configured processing pass. `--full` ignores the scan cache. |
| `git hint changes [--format text\|canonical]` | reads local state; no network | Changes between the last two snapshots. `text` shows local names; `canonical` prints the HINT change set without names. |
| `git hint observe` | reads local state; no network | How activity is collected; adapter state. |
| `git hint export [--dry-run] [--privacy-report] [--out DIR] [--all]` | reads local state; writes files; no network | Build and validate the export package. `--dry-run` writes nothing. Default output: `.hint/state/exports/<time>/`. `--all` includes observations that were already exported. |
| `git hint recommend [--yes] [--retry] [--server URL] [--all] [--forecast-horizon N] [--forecast-threshold P] [--assumed-cost LO:HI]` | reads local state; **sends data** to the server | Send the package and fetch the recommendations. Without `--yes` it asks in a terminal and refuses in a script. `--retry` sends queued packages. |
| `git hint plan [--from FILE] [--out FILE] [--json]` | reads local state and work trees; writes a Markdown file; no network | Validate a recommendation and write the local plan. Exit code 1 when the document is rejected. |
| `git hint cache clean` | removes `.hint/cache/` only; no network | Delete rebuildable data. Keys, mapping and state stay. |
| `git hint doctor` | reads local state and tools; no network | Check Git, the store, the key, the schemas, the adapters and optional tools. Starts no agent. |
| `git hint adapter install opencode [--force]` | writes `.opencode/plugins/hint-adapter.ts`; no network | Install the OpenCode adapter plugin. |
| `git hint adapter opencode --project DIR` | reads stdin; writes the activity spool; no network | Started by the OpenCode plugin. Not for direct use. |
| `git hint mcp [--project DIR] [--tools NAMES]` | stdio MCP server; each tool states its effect; `hint_recommend` sends only with the user's rule | Serve the HINT tools to a coding agent. See [mcp.md](mcp.md). |
| `git hint version [--json] [--capabilities]`, `git hint --version` | none | Client and data format version, commit, build; `--capabilities`: languages, MCP, adapters, optional tools, platform support. |

## Collection and eval effects

Eval collection is off by default. With no enabled eval policy, `collect` scans repositories and writes local state. When eval collection is on, ordinary `collect` also processes sessions under that policy. `collect --json` currently returns repository scan results without that eval pass. MCP `hint_collect` runs the configured eval pass.

An allowed LAN-model policy can send request text or code to the configured LAN model for segmentation, mapping, or test drafting. Allowed container jobs can build images, download dependencies, and run code. The permission to collect sessions does not itself allow model calls or container runs. HINT server exports have separate consent: `recommend` needs approval, MCP upload needs its local allow rule, and `eval report --send` needs result-export permission plus approval.

## Conditional forecast options

| Flag | Meaning and accepted values |
|---|---|
| `--forecast-horizon N` | Explicit future-task horizon, integer 1–100,000. |
| `--forecast-threshold P` | Decimal in (0, 1], with at most six fractional digits. Requires an explicit horizon. The default 0.9 is heuristic. |
| `--assumed-cost LO:HI` | Ordered nonnegative integer token bounds, at most 1,000,000,000,000. Requires an explicit horizon. The result is an assumption scenario, never a recommendation. |

Invalid values stop before any upload. The server can return a conditional forecast only after its calibration gate passes; distribution, effect, cost, and correctness gates still apply. An option does not establish calibrated support for arbitrary data. Unknown cost stays unknown. The ordinary intervention comparison still has no cost model, net value, or break-even.

MCP `hint_recommend` accepts the corresponding optional `forecast_horizon_tasks` integer, `forecast_threshold` decimal string, and paired `assumed_cost_tokens_lower` / `assumed_cost_tokens_upper` integers. Omit unused fields; explicit `null` is refused. The same validator checks CLI and MCP policy values before HTTP requests. Forecast arguments grant no upload consent or server selection. See [MCP policy arguments](mcp.md). Returned documents use the existing plan validation and rendering rules.

## Eval collection and review

| Command | Effects and purpose |
|---|---|
| `git hint eval [--json]` | Read the local eval list and pipeline state. |
| `git hint eval on [--agent opencode,claude-code,codex] [--run docker] [--runner docker]` | Enable session collection. `--run docker` separately permits container qualification. Other model permissions remain in the eval policy. |
| `git hint eval off` | Disable collection; keep stored evals. |
| `git hint eval show ID [--text] [--json]` | Read one local eval. `--text` exposes its instruction locally. |
| `git hint eval map ID [--apply FILE]` | Print a mapping, or validate the edited mapping and store a new eval version. Qualification follows the execution policy. |

## Eval runs, reports, and declared work

| Command | Effects and purpose |
|---|---|
| `git hint eval run [ID...] [--agent opencode\|oracle\|nop] [--attempts N] [--arm ARM] [--json]` | Run bounded trials of validated evals under the execution policy. OpenCode uses the configured LAN model through the model gate. Oracle and nop need no model. Image builds and dependency downloads can use the network. |
| `git hint eval run ID... --arm context_only --with FILE...` or `--arm refactor --with WORKTREE [--plan PLAN --work-unit LABEL]` | Qualify and run variants of the same task. `--with` is repeatable for context files. `--opencode-tgz FILE` supplies the pinned agent archive offline. `--context-policy` is reserved and currently refused. |
| `git hint eval compare [SUITE] [--json]` | Read raw local results by arm and agent. Unknown usage stays unknown. |
| `git hint eval report [-o FILE] [--send] [--yes] [--server URL]` | Write a minimized report. `--send` uploads only with result-export permission and approval; `--yes` supplies that approval in scripts. |
| `git hint eval work start LABEL [--plan PLAN [--from SETFILE]] [--family LABEL]` and `git hint eval work close LABEL --outcome completed\|failed\|abandoned` | Store immutable work-unit declarations and revisions. A declaration does not establish canonical usage or measured intervention effects. |

## Eval library and behavior cases

| Command | Effects and purpose |
|---|---|
| `git hint eval pin ID [--off]` | Set or remove a local retention pin. |
| `git hint eval forget ID...` or `git hint eval forget --all` | Delete unpinned local evals and list leftovers. |
| `git hint eval pack ID... -o FILE` / `git hint eval unpack FILE` | Write or read a private bundle containing instructions, states, checks, and solutions. These commands do not upload it. |
| `git hint eval declare-io --base REV --reference REV --entry JSON_ARGV --case ID=FILE... --request FILE [--case-seconds N] [--family FAMILY]` | Package declared behavior cases. Use 1–64 cases with 1–30 seconds per case. Qualification compares output and exit status against the stored reference; it does not prove general correctness. |

Container eval execution needs a Docker engine. The ordinary client needs only `git-hint` and Git. Harbor and Python are not client runtime dependencies. See [evals](evals.md) for policy, limits, qualification, and reporting details.

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
