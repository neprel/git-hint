# git-hint — unreleased changes

These notes describe changes that are not released yet. They name no version, and no tag or package exists for them.

## Local evals from your own agent sessions

`git hint eval` turns your agent sessions into local evals: repeatable tasks with hidden checks that measure how well agents work on your project, before and after a change of agent setup, added context or a refactoring. Everything stays on your machine unless you send a minimized report yourself. Guide: [evals](../client/evals.md).

- **Enable once.** `git hint eval on` (collection), `--run docker` (container runs). After that, `git hint collect` imports sessions, proposes episodes, packages evals and starts a bounded qualification worker.
- **Session import.** OpenCode 2.0 exports and snapshots; Claude Code 2.1 and Codex 0.161 session files (offline import only). Untested versions are refused.
- **Episodes.** Deterministic rules split sessions into tasks at user requests. An optional local model can label tasks (budgeted, recorded as a proposal, replayed in later passes).
- **Evals.**
  - Each eval holds the exact starting state (from agent snapshots, never the current work tree), hidden tests, a reference solution and the instruction.
  - Bug fixes, features, compatibility changes and refactorings are supported.
  - Large binaries, Git LFS and submodules are refused with a reason.
- **Requirements.**
  - The request and each correction must be linked to hidden tests.
  - The local model can propose links; they are labeled as an estimate.
  - `git hint eval map ID` prints an editable mapping file; `--apply FILE` checks it against that exact eval version and stores a new version. Only you can confirm that a sentence states no requirement.
- **Qualification** (needs only a Docker engine; git-hint's built-in container runner, no Harbor or Python).
  - Disposable containers: the agent's container has no network (a live agent reaches only your local model through HINT's gate); the agent's final tree is frozen and checked by a separate verifier without network.
  - The starting state must fail and the reference must pass, three times.
  - Partial references must fail, and each requirement's tests must discriminate.
  - Probes check result forgery, early test exits, network access and leaked solutions.
  - States: `validated`, `reproducible`, `needs_review`, `quarantined`, each with reasons.
- **Alternatives.** Existing alternative solutions in a Git work tree can be named in the mapping file. HINT runs them; a claim of independence is recorded as your claim.
- **Runs and comparisons.**
  - `git hint eval run` runs bounded trials with `nop`, `oracle` or OpenCode on your configured local model, through a counting proxy with a request limit. OpenCode is a pinned, hash-checked build (downloaded once, or `--opencode-tgz FILE` offline).
  - `git hint eval declare-io` packages a declared behavior check: your command's output on your own inputs is compared with the stored reference's (equivalence on those cases, not general correctness).
  - `--arm context_only --with FILE...` and `--arm refactor --with WORKTREE` run variants of the same tasks with added context files or on your refactored code. A refactoring variant is used only when the original change ports onto it; otherwise it stays `needs_review`.
  - `git hint eval compare` shows every attempt and its resources per task objective; unknown usage stays unknown.
- **Library.** Immutable versions, retention with pins (`git hint eval pin`), `forget`, and private bundles for your other machines (`eval pack`, `eval unpack`).
- **Reports.** `git hint eval report [--send]` writes a minimized result report (evalreport v0.2): outcomes, checks, counters, the context policy state and project-scoped references only, no task text, code, paths or commands. Sending needs the policy and your approval.

## Bounded context for agents (MCP)

- New read-only MCP tool `hint_context`: a small context around one declaration, file or package from the local index (definition with source, file declarations, imports, importers, tests), with refs to expand one detail.
- Hard limits on depth, items, bytes and time. An old ref reads its own snapshot or is refused as stale. Nothing is stored; source is read only when the file is unchanged since the snapshot.
- The OpenCode adapter records each `hint_context` call with its context measurement (metadata only). A local context policy (`git hint init --context-limits`) sets the default limits, with every version kept for restore.

## Declared tasks

- `HINT_TASK_ID` in an agent run's environment declares the task of that run for the OpenCode adapter (same opaque ID as `hint_collect` `task_id`). Eval reports carry the declared task of an eval when the binding is unambiguous, so comparisons can bind to the findings of the same tasks.

## HINT data standard v0.2 (draft)

- The client reads v0.1 and v0.2. It writes v0.1 by default and the **draft** v0.2 only for a package that holds a draft field: the context measurement of an observation, a declared task family, a source configuration, model calls (`MODEL_CALL`, request kind, call identity), a collector closure, a usage normalization, or a declared plan application. `git hint version --json` lists them (`standard_draft_writes`, `standard_draft_fields`); the release manifest names both versions. The draft is not stable and can change before a release.
- v0.2 also adds parts that the client reads from a server: effort findings and abstentions, context-policy feedback, project eval comparisons with counter provenance (`counter_semantics`, `scope`) and their decision rule, a forecast basis (`forecast_basis`) and its draft effect basis (`effect_basis`). See [v0.2 changes](../standard/hint-data-v0.2.md).
- The local plan (`git hint plan`, MCP `hint_plan`) starts with a summary and shows at most five findings with local names: the problem, observed quantities with coverage, cause hypotheses and their state, candidate interventions and a next step. The order is a heuristic priority. It also shows the project's eval comparisons (observed) and proposals on the context policy. No net value or break-even is given.

## Known limits

- Linux was checked in containers (Linux engine on a macOS host), not on a physical Linux machine. Windows is deferred.
- OpenCode runs in containers use a pinned npm release by default; OpenCode 2.x runs through a checked adapter with a pinned, hash-checked binary.
- A requirement link proposed by the local model is an estimate. Qualification cannot detect a semantic mislink within one sentence.
- The accuracy of the optional local-model task labeling is not measured.
- **Work units and configuration (draft v0.2).** `git hint eval work start|close` declares a piece of work, its member agent runs (`HINT_WORK_UNIT`) and its attempts as immutable revisions that `eval report` sends; `changes --declare-plan --work-unit --after` binds a completed unit's change to its plan. `HINT_TASK_FAMILY`, `HINT_MODEL` and `HINT_CONFIG_REFS` add a declared family and the canonical run configuration to the activity. All optional; without them nothing changes.
- **Controlled work units (draft).** A live `eval run` writes its run manifest and an open work unit for every planned refactor-family eval before the first attempt; each terminal trial adds an immutable revision (outcome, checks against the eval's mandatory set, harness-reported tokens or null, the proxy's model check); `eval report` sends the journal. The proxy counts requests; token counts come from the harness result.
