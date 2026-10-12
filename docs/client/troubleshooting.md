# git-hint troubleshooting

Run `git hint doctor` first. It checks Git, the store, the key, the schemas and the adapters, and starts no agent.

| Message or symptom | Cause | Action |
|---|---|---|
| `no HINT project here` | No `.hint/state/project.json` in this directory or above. | Run `git hint init` in the root of the repository. |
| `is not inside a Git work tree` | `init` runs only in a Git repository. | Run `git init` first, or change to the repository. |
| `.hint/ is not ignored by Git` | The entry in `.git/info/exclude` was removed, or a rule ignores only a part of `.hint/` (for example one file, or a negated subdirectory). Writes stop to protect the store. | Run `git hint init` again, or add `/.hint/` to `.gitignore`. |
| `adapter opencode: stopped writing` (exit 4) | The exclusion of `.hint/` broke during an OpenCode session. Nothing was written after that; the plugin counted the later events as dropped. | Restore `/.hint/` in `.git/info/exclude` or remove `.hint/` from the index; the next OpenCode session writes again. |
| `Git tracks .hint/...` | A file below `.hint/` is in the Git index; ignore rules do not apply to it. | `git rm --cached -r .hint`, then commit. |
| `the project key must not be inside a repository` | `HINT_KEY_DIR` (or a symbolic link in it) points into one of the project repositories. | Use a directory outside every repository, for example the default. |
| `the receipt ...` | The server answer did not confirm the sent document (wrong digest or ID, or an invalid receipt). The export is not marked as done. | Send again later; report the server problem. |
| `project key ... not found` | The key file was moved or deleted, or `HINT_KEY_DIR` points elsewhere. | Restore the key file. Without it, the old IDs cannot be reproduced; a new project (`init` in a fresh store) gets new, unlinkable IDs. |
| `repository ... has no snapshot` | `export` or `recommend` before the first scan. | Run `git hint collect --full`. |
| `the export policy removes the required field` | A field allowlist in `project.json` drops a field that the record needs. | Add the field, or remove the record type from `record_types`. |
| `... (the document is not exported)` with a rule code | A document failed validation. The client never sends it. | Report the rule code. `content_forbidden` means that a value of a wrong form reached a record. |
| `needs approval` | `recommend` in a script without `--yes`. | Inspect with `git hint export --dry-run --privacy-report`, then use `--yes`. |
| `server address must use https` | A plain `http` URL to a remote host. | Use `https`. Plain `http` works only for `localhost`, `127.0.0.1` or `::1`. |
| `the package is kept in .../queue/...` | The server was unreachable or answered 5xx/429. Nothing was lost. | `git hint recommend --retry --yes` later. |
| `plan`: `snapshot_mismatch` | The recommendation was made for another state of the work tree. | Run `git hint collect`, `git hint recommend` again, then `git hint plan`. |
| `plan`: `unresolved_id`, `ambiguous_id` | The plan names an entity that does not exist locally, or two local declarations share one name (for example several `init` functions). | The plan is rejected; do not apply it. |
| OpenCode: no observations in `status` | The plugin is not installed, `git-hint` is not on the `PATH` of OpenCode, or Git is missing for the child process. | `git hint adapter install opencode`; set `HINT_BIN`; check `adapter-stats.json` in `.hint/state/activity/` for `dropped`. |
| OpenCode: a second HINT plugin | OpenCode loads every `.ts` file in `.opencode/plugins/`. | Keep only `hint-adapter.ts`. |
| Slow export on a large repository | Canonical digests (RFC 8785) of large snapshots. About 1 s per 1,000 files on a laptop. | Use `export` without `--dry-run` once; `collect` stays incremental. `HINT_CPUPROFILE=<file>` records a profile. |
| Windows | Not tested. | Use macOS or Linux. |
