# git-hint privacy and data audit

This document states what the client keeps on the machine, what can leave it, how the client enforces this, and what an export still discloses. The format rules are in the [privacy contract](../standard/privacy-v0.1.md).

## 1. What stays on the machine

| Data | Where |
|---|---|
| Project key (32 random bytes) | user configuration directory or `HINT_KEY_DIR`, mode 0600; never in the work tree, never exported |
| Names: paths, symbol names, repository names | `.hint/state/snapshots/` (local index) and `project.json` |
| Source code, file content, comments | never stored; only content hashes for change detection, in the local index |
| Agent tool output, edit text, search patterns, prompts | never leave the OpenCode process; the plugin keeps an allowlist and replaces output by its length. For `hint_context` it also passes the validated metadata line of the answer (enumerations, numbers, opaque IDs), never its source or names |
| Shell command strings | reach the local child process through a pipe; stored only as a command class and file IDs |
| Commit hashes, branch names, remotes, author names | never exported; a commit is an opaque ID |

## 2. What an export contains

Only canonical HINT documents: `export_policy`, `repository_snapshot`, `change_set` and `activity_stream`. Every string in them is an opaque ID, a digest, a timestamp, a version or an enumeration value. Identifiers are HMAC-SHA256 pseudonyms under the project key.

## 3. How the client enforces it

| Rule | Mechanism | Test |
|---|---|---|
| Records are built from an allowlist | every record type has a fixed set of fields; the adapter reads only allowlisted event fields | `TestExportHasNoContentOrNames`, plugin test "the pipe carries no output, edit text, search pattern or prompt" |
| Deny by default before sending | every document passes the schema, the semantic rules (including the content scan) and the export policy before it is written or sent; a failure stops the export | `TestPolicyAllowlistKeepsRecordTypesOut` |
| Record types and fields outside the policy never leave | the policy filter runs before validation; the validation checks the policy again | `TestPolicyAllowlistKeepsRecordTypesOut` (file and API) |
| The user inspects before sending | `export --dry-run --privacy-report`; `recommend` needs approval | `TestRecommendNeedsApproval` |
| `.hint/` is never committed | `init` adds `/.hint/` to `.git/info/exclude`; every write checks its own path and every store area with `git check-ignore`, and that Git tracks no file below `.hint/`; a partial rule or a tracked file stops every write | `TestInitExcludesStoreAndKeepsKeyOutside`, `TestWriteFailsClosedWhenNotExcluded`, `TestPartialIgnoreAndTrackedFilesFailClosed` |
| A running adapter stops when the exclusion breaks | the spool writer checks the exclusion before every record and stops for good after a failed check; the plugin writes no file and counts later events in memory | `TestExclusionChangedDuringAdapterSession`; Bun tests "exclusion changed during the session" and "the plugin source writes no file" |
| The key stays outside the repositories | a key directory or key file inside any project repository (also by relative path or symbolic link) is refused at `init` and at every start | `TestKeyInsideAProjectRepositoryIsRejected` |
| A changed work tree is never mistaken for an old one | every file is hashed on every scan; the cache only saves parsing | `TestSameSizeAndMtimeChangeIsDetected` |
| Transport | `https` only (except loopback), no redirects, response size limit | `internal/api` tests |
| The MCP server never sends on the agent's word | `hint_recommend` has no arguments, uses only the configured server and the local policy, and sends only with the user's local rule `mcp.upload: allow`; tool errors are redacted; paths are checked against the project repositories | `internal/mcpserver` tests: `TestRecommendFailsClosedWithoutLocalApproval`, `TestErrorsAreRedacted`, `TestImportRecommendationPathsAndLimits` |
| A send counts only when the server confirms it | the receipt must pass the schema and the semantic rules, and its accepted entry must name the sent document ID and content digest; otherwise the export is not marked as done | `TestWrongReceiptDoesNotMarkExport` |
| An independent check agrees | the conformance suite builds the client and validates its exports with the reference checker | `tests/conformance` `TestClientExportsPassReferenceChecker` |

The canary tests place fake secrets in file names, directory names, file content, comments, string literals, symbol names, the commit message, the branch name, the remote URL, the author email, agent session and call IDs, shell commands and tool output. None of them appears in an export or an API request.

## 4. Recommendations are untrusted data

A recommendation holds no text and no command. The client validates it before it shows it, resolves identifiers only with the local index, and renders only enumerations, numbers and local names. It never executes a step. A plan that names an unknown or ambiguous identifier, or that was made for another state of the work tree, is rejected.

## 5. What an export still discloses

The format removes content and names. It does not make a project anonymous:

- **Structure**: the shape of the directory tree and the declaration graph can identify a known code base.
- **Sizes**: exact file sizes, line counts and parameter counts can identify known files. Remove `features` from the `Entity` field allowlist to omit them.
- **Times**: timestamps show working hours and the order of actions. Use the `coarse` profile.
- **Activity**: the operation sequence of an agent session. Use the `aggregate` profile or remove `Observation` from the record types.
- **Linkability**: all exports of one project use one key, so their IDs link to each other. A new project key gives unlinkable IDs (and new IDs for everything).

A schema-valid document can still carry a secret if a faulty producer puts it into a field of a valid form. The client derives every ID with the HMAC rule; the tests above check that no input value reaches the output.
