# Local evals from your agent sessions

An eval is a repeatable task with an exact starting state of your repository, the
instruction you gave, an isolated environment and hidden checks. HINT builds evals from
your own coding-agent sessions. Use them to measure how well agents work on your project:
before and after a refactoring that HINT recommends, or when you change the agent, model,
context or instructions.

Everything stays on your machine. Session texts, file states, checks and reference
solutions are stored in `.hint/state/evals/` (owner-only files, ignored by Git). Nothing
is sent to a HINT server.

## Ordinary use

```sh
git hint eval on                 # once: collect from OpenCode sessions
git hint eval on --run docker    # optional: allow qualification runs in containers
git hint collect                 # as usual; new sessions become evals automatically
git hint eval                    # what was collected, and why some sessions were not used
git hint eval show ev-…          # one eval with its checks (--text shows the instruction)
```

After `eval on`, every `git hint collect` (also through the MCP tool `hint_collect`)
runs one finite pass: it imports new sessions, keeps the exact file states, finds the
finished tasks, and packages them as evals. With `--run docker`, collect then starts one
background worker that qualifies the new evals; `git hint eval` shows the results.

Other agents: `git hint eval on --agent opencode,claude-code,codex`. Claude Code and Codex
sessions are read from their session files only ([session-imports.md](session-imports.md)).

## What makes an eval

| Step | Rule |
|---|---|
| Task | A request you typed, plus your corrections before the first passing check. A request after a passing check starts a new task. |
| Starting state | The exact state before the first edit, from OpenCode's own step snapshots. Without it, no eval is built (HINT never uses the current state instead). |
| Checks | Tests that the session added or changed become hidden checks; your existing tests must keep passing. HINT never runs a command copied from a session. |
| Reference | The changed source files, kept hidden from the agent. |
| Ownership | Work that another session changed at the same time on the same files is not used. |

Sessions that do not meet these rules are listed with their reasons, for example
`baseline_missing`, `verifier_missing` or `attribution_ambiguous`.

## Qualification

An eval is `validated` (policy q2) only when, in disposable containers without network:

- the starting state fails the hidden checks;
- the reference passes them and your existing tests in three clean runs;
- partial references fail;
- every requirement is linked to tests, the linked tests pass with the reference, and for
  requested behavior at least one linked test fails on the starting state;
- a probe that tries the network, writes fake results and adds test code that ends the
  test run early (a `TestMain` or `conftest.py` that exits 0) gets no network and no
  credit, and the agent's environment holds no Git history, hidden test or reference
  solution.

A check counts only when each required test actually ran: the verifier reads Go's
`go test -json` events and pytest's per-test lines, and it removes test files that the
agent added. `validated` means qualified under q2; it is not a proof that the task is
complete or that the reference is the only correct solution.

**Requirements.** Tests written with a change do not show which requested behavior they
check. So your request and each correction start without a link to a check. With the local
model allowed in the eval policy, HINT asks it once per eval to split the text into
requirements and to link each one to hidden tests. HINT accepts the answer only when every
quote is your exact text, every test exists, and every part of your text is either a
requirement or marked as background. The model cannot remove text: a sentence that it
marks as background, or a link that joins several sentences, stays a requirement without
a check. Each requirement keeps your original request or correction as its source. A
requirement that no test checks stays without a check and is shown as missing evidence. Without the model, these evals stay
`needs_review` with the reason `requirement_coverage_missing`. A model link is labeled
`model`; it is not independent evidence. `git hint eval show <id>` lists each requirement,
its link and its coverage.

**Your own mapping.** `git hint eval map ID` prints the mapping file of the latest
version: your original texts, the test IDs, and the current links (a model's links are
marked as an estimate). Edit the links, move background sentences to `context`, and run
`git hint eval map ID --apply FILE`. HINT checks that the file belongs to that exact
version (its digest), that every quote is your text and every test exists. It stores a
new `packaged` version and never sets `validated` itself. Qualification then runs as
after `collect`, when the eval policy allows container runs.

**Alternative solutions.** If you already have another solution of the same task in a
Git work tree, name it in the file under `alternatives` (`"worktree": "../other"`). Add
`"claimed_by": "user"` only when it was made independently of the reference; HINT
records the claim and does not check it. Qualification runs each alternative; one that
fails sends the eval to `needs_review`. Without alternatives the check stays `missing`;
HINT never generates one.

**Variants: context and refactoring.** To see whether added context or a refactoring
helps agents on the same tasks, run a variant of validated evals:

- `git hint eval run ID... --arm context_only --with AGENTS.md` adds the named files to
  each task's repository;
- `git hint eval run ID... --arm refactor --with ../refactored` uses the current state of
  that Git work tree as the starting code.

A variant keeps the task: the same request, requirements, hidden tests and environment.
For a refactoring, HINT ports the original change onto the refactored code with a strict
patch; when it does not apply, the variant has no reference solution and stays
`needs_review` (`reference_missing`). Each variant is qualified first; until it is
`validated`, the command lists it and does not run it. `git hint eval compare` shows the
arms of one task objective side by side. A label alone (`--arm refactor` without
`--with`) is refused. A HINT context policy as the treatment (`--context-policy`) is refused
for now: the task container cannot run `hint_context` yet, so the policy would only be a label.

Qualification and eval runs need a Docker engine (Docker Desktop, OrbStack, or Docker on
Linux). git-hint has a built-in container runner: it builds the task images, runs every
attempt in fresh containers with resource limits, freezes the agent's final tree, and checks
it in a separate container without network. Nothing else is installed: no Harbor, no Python.
Without a Docker engine, evals stay `packaged`; there is no other runner to fall back to.

A live OpenCode run gets network only to your local model, through HINT's model gate. The
runner installs a pinned OpenCode build into each attempt: it downloads the pinned archive
from the npm registry once (checked against its pin, then cached in `.hint/cache`), or uses a
local copy that you name with `git hint eval run --opencode-tgz FILE`.

Declared behavior checks: `git hint eval declare-io --base REV --reference REV --entry
'["python","tool.py"]' --case ID=FILE... --request FILE` packages an eval whose check runs
your command on your own inputs (stdin) and compares its output and exit status with the
stored reference's. The verdict is equivalence to the reference on those cases, not general
correctness. The inputs stay on your machine and never reach the agent.

## Measuring agents

```sh
git hint eval run --agent oracle          # check the setup without a model
git hint eval run --attempts 3            # OpenCode with the local model of the policy
git hint eval run --arm configuration     # label a run, for example after a setup change
git hint eval compare                     # raw results per arm and agent
```

`eval run` freezes a suite of validated evals and runs bounded trials in containers. The
results of each trial (verifier output, agent log, the files the agent left) are stored in
the local library before the work directory is removed; when they do not fit the quota,
the directory stays. A
live agent uses only the local model named in the eval policy (`model.use: lan`, a
private-network `base_url`, the key in the environment variable `key_env`). HINT runs a
small proxy that holds the key, counts the model requests of each trial and stops them at
the trial limit; inside the container the agent can reach only that proxy. `eval compare`
shows raw counts: every failed or timed-out attempt and its resources count, unknown
usage stays unknown. Statistical comparisons are not computed locally.

## Result reports

```sh
git hint eval report                 # write a minimized report of new trial results
git hint eval report --send          # send it (needs "export": "results" in the eval policy)
```

A report holds outcomes, check results, counters, budgets and opaque references that only
your project key can produce. It holds no task text, code, file name, command, log,
content digest or local ID; a model name becomes an opaque reference. `--send` asks for
approval (or `--yes` in scripts), like `git hint recommend`. A local `validated` state is
reported as your client's claim, with its policy name. The server uses reports for
comparisons; no server is needed to create, run or compare evals.

## Moving evals to another machine

```sh
git hint eval pack ev-… -o evals.bundle   # private file: instruction, states, solution
git hint eval unpack evals.bundle         # every object is checked; nothing is sent
```

A bundle holds private task content. Keep it on your own machines; HINT never uploads it.

## Limits and deletion

Defaults: 90 days retention for unpinned evals, 2 GiB, 20 episodes per pass, 20 trials
per qualification run. Change them in the `evals` object of `.hint/state/project.json`.

```sh
git hint eval off              # stop collecting; stored evals stay
git hint eval forget ev-…      # delete evals (pinned ones stay)
git hint eval forget --all     # delete the whole eval library of this project
```

Deletion lists anything that remains outside the library, such as runner job folders.
