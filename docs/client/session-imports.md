# Session imports for local evals

HINT can turn your own coding-agent sessions into local evals. The session importers read
files only: they never start an agent, never run a command found in a session, and never
read agent credential stores (for example OpenCode's credential table or Codex's
`auth.json`). Imports happen only when the local eval policy is on.

## Supported formats

| Agent | Input | Tested version | Accepted versions |
|---|---|---|---|
| OpenCode | `opencode session export <session>` JSON | 2.0.26 | 2.0.x |
| Claude Code | session JSONL file | 2.1.302 | 2.1.x |
| Codex CLI | rollout JSONL file | 0.161.0 | 0.161.x |

Other versions are refused with an "unsupported session format version" message; HINT
does not guess. Other agents are not supported.

## What each format cannot provide

| Field | OpenCode 2.0 export | Claude Code 2.1 | Codex 0.161 |
|---|---|---|---|
| Task identity | not present | not present | not present |
| Parent session | not in the export; HINT takes it from the session list | not present | `parent_thread_id` |
| Exact file states | per-step snapshot trees (OpenCode snapshot repository) | edited-file backups only | not present |
| Compaction markers | not present | `compact_boundary` | `compacted` |
| Token usage | per response | per response (counted once per response) | per response |

A child-agent session is kept for attribution and cost; its prompt comes from the parent
agent and is never treated as a user request.

## Damaged and repeated input

- Importing the same file again changes nothing.
- A later export of the same session replaces the earlier form; a shorter or truncated
  copy never replaces a longer one.
- A torn last line (a crash or rotation) marks the session as truncated; other unreadable
  records are counted and skipped.
- Inputs larger than 64 MiB and single text fields larger than 1 MiB are not stored.
- Error messages name record positions and codes, never session text.
