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

An eval is `validated` (policy q1) only when, in disposable containers without network:
the starting state fails the hidden checks; the reference passes them and your existing
tests in three clean runs; partial references fail; a probe that tries the network and
writes fake results gets no network and no credit; and the agent's environment holds no
Git history, hidden test or reference solution. `validated` means qualified under q1; it
is not a proof that the task is complete or that the reference is the only correct
solution.

Qualification needs Docker and Harbor 0.23 (`uv tool install harbor==0.23.0`); HINT
turns Harbor's telemetry off for its runs. Without them, evals stay `packaged`.

## Measuring agents

```sh
git hint eval run --agent oracle          # check the setup without a model
git hint eval run --attempts 3            # OpenCode with the local model of the policy
git hint eval run --arm configuration     # label a run, for example after a setup change
git hint eval compare                     # raw results per arm and agent
```

`eval run` freezes a suite of validated evals and runs bounded trials in containers. A
live agent uses only the local model named in the eval policy (`model.use: lan`, a
private-network `base_url`, the key in the environment variable `key_env`). HINT runs a
small proxy that holds the key, counts the model requests of each trial and stops them at
the trial limit; inside the container the agent can reach only that proxy. `eval compare`
shows raw counts: every failed or timed-out attempt and its resources count, unknown
usage stays unknown. Statistical comparisons are not computed locally.

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
