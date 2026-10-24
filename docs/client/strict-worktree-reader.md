# Strict Git-selected worktree reader

This is an offline reader prerequisite. It is not an admitted capture policy, catalogue
entry, original project verification intent, runtime reference preparation, completion
verdict or economic cost proof. It adds no CLI or default strict-mode selection.

```go
trees.FromWorktreeWithCapturePolicy(ctx, library, repoRoot, limits,
    trees.StrictCapturePolicyV1)
```

The exact policy name is `strict-worktree-reader-v1`. It uses the SAME Git selection and
content reader as legacy `trees.FromWorktree`: tracked paths and untracked nonignored paths
from `git ls-files -z --cached --others --exclude-standard --deduplicate`.

**The scope is Git-selected artifact paths only.** Git omits some untracked special files,
including a FIFO. A regression demonstrates that an untracked FIFO can exist while the
selected artifact digest stays unchanged. An indexed path replaced by a FIFO refuses.
No result establishes whole-worktree special-file absence or whole-work completeness.
The earlier proposed capture policy's global `special_files=refuse` interpretation remains
inadmissible until this scope receives explicit source/outcome review.

## Exact effective limits

| Bound | Strict default and maximum |
|---|---|
| Regular file bytes | 8 MiB |
| Aggregate captured content bytes | 64 MiB |
| Selected inventory paths, including absent paths | 10,000 |
| Git stdout, per command | 4 MiB |
| Git stderr, per command | 64 KiB |
| Capture context | 30 seconds or the earlier caller deadline |

For each `Limits` field, zero selects its strict default; a positive value selects
`min(requested, strict maximum)`; a negative value refuses. Strict defaults do not depend
on mutable legacy `trees.DefaultLimits`. Unknown/empty/boolean-like policy names refuse.
Each read bounds its bytes by both per-file and remaining aggregate budgets. Cancellation,
command failures, malformed/incomplete stage inventory, overflow and an expired context
refuse. An expired context cannot return a successful tree after persistence. Process
cleanup uses `exec.Cmd.WaitDelay`; this is not a workflow-wide runtime or kernel-I/O
quiescence guarantee. No automatic retry exists.

## Shared descriptor reader

On Unix, strict roots and source ancestors open descriptor-relatively with `O_NOFOLLOW`.
The same bound file descriptor supplies size/type/mode checks, bounded content reads and
post-read identity checks. The path must still name that file through the same parent
chain. Replacement, a changed ancestor with the SAME leaf inode, growth/shrink, mode change,
symlink/FIFO substitution and read failures refuse. FIFO opens are nonblocking before type
checks. Root identity is checked before/after Git commands and after content capture.

Git still receives a canonical pathname: macOS Git cannot use a directory `/dev/fd/N`
argument. The pathname checks detect observed changes; Git's census is not atomic with the
subsequent file reads. Full ignore-source resolution and repeated-capture orchestration are
separate missing prerequisites. The reader does not claim that a transient mutation outside
its observed checks cannot occur.

Strict capture refuses symlinks/special files AT SELECTED PATHS, selected protected root
`.hint` components and any `.git` component under conservative case-fold comparison, invalid/duplicate paths, submodules, LFS pointers and Git stage-command failures.
Modes use the existing normalized `100644`/`100755` tree representation. A missing tracked
file or subtree remains absent; stable-absence/repeat proof belongs to the later orchestration.

Legacy `FromWorktree` uses the same bounded regular-file core while preserving ordinary
supported final-symlink target bytes, modes, Git selection and canonical tree bytes. Its
existing defaults remain 8 MiB/file, 256 MiB/tree and 50,000 paths; Git stdout now has a 32 MiB bound. Read-only Git callback suppression applies in both modes,
including store exclusion checks made by object persistence. Unsafe observed ancestor/file replacement refuses. Legacy root aliases are
resolved once; strict root/ancestor aliases refuse. Non-Unix platforms keep bounded legacy
reads and explicitly refuse the new strict contract rather than claim unavailable no-follow
semantics. No approved implementation/source digest is self-pinned here.

## Local verification

Deterministic tests exercise before-open and after-open replacement, root/ancestor symlinks,
FIFO substitution, same-inode ancestor changes, growth/shrink, executable-mode changes,
closed-descriptor errors, read/total/path/Git-output limits, cancellation, malformed/failed
Git stage output, strict limit clamping and the unlisted-FIFO limitation.

A fixed legacy fixture with dirty, renamed, deleted, untracked, ignored and symlink files
keeps tree digest `sha256:fdcebb051070364be786fc5c29bad5bed82de61c99aae7cdee710a0702d1f1d0`.
The equivalent regular-only fixture gives the same tree in legacy and strict modes.
All fixtures use local synthetic repositories; no engine, model or external service is needed.

## CR-1 correction history

The original reader package `08e32b8` failed independent review: its strict protected-store
check was case-sensitive. On case-insensitive APFS, an indexed `.HINT/private` aliases
`.hint/private` and bypassed that check. The follow-up applies case-fold protection only
in strict mode: root `.hint` and every `.git` component refuse across case variants.
Near spellings such as `.hinted`, `.HINT-copy` and `.gitignore` remain ordinary names;
a nested `.hint` remains outside the root-store spelling rule. Legacy spelling semantics
and the fixed golden digest remain unchanged. This is not a claim of whole-filesystem
absence or a new capture-policy approval.

The actual APFS alias regression runs without a skip on the review host. A temporary
overlay restoring the original guard causes that regression to fail. Pure spelling
controls run on every filesystem; the actual-alias test explicitly skips when the
filesystem does not exhibit that alias. Targeted race tests include the legacy golden.

## CR-2 correction: callback-safe Git and persistence deadlines

The original `08e32b8` and `4320ac6` did not disable configured Git fsmonitor callbacks.
Independent review demonstrated a fake configured hook executing on the host during strict
capture. Those original packages are not accepted as callback-safe reader evidence.

The shared `internal/readgit` command builder now uses this fixed prefix for inventory and
store safety inspections (including their rev-parse calls), in BOTH reader modes:

```text
git --no-pager -c core.fsmonitor=false -c core.hooksPath=/dev/null -C <root> <fixed read command>
```

Each command receives its own copied environment. Ambient `GIT_CONFIG_PARAMETERS`,
`GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_*`, `GIT_CONFIG_VALUE_*` command injections are removed;
`GIT_OPTIONAL_LOCKS=0` replaces any ambient setting. No global environment mutation occurs.
Ordinary global/system configuration sources remain selected; fixed argv overrides their
callback settings. Intentional ambient command-config overrides no longer influence these
inspections. Default ordinary fixture object/tree bytes remain unchanged. This does not
attest an arbitrary executable selected from PATH or resolve the full ignore-source policy.

Source inventory keeps its existing output/root/cancellation checks. Store safety commands
use bounded stdout (4 MiB), stderr (64 KiB), process cancellation and bounded stream cleanup.
Both stream readers must independently observe EOF: a process exit 1 must not hide a
WaitDelay-truncated stream. Only clean complete exit 0 or deliberate check-ignore exit 1
can return stdout. Exit 1 still
undergoes the existing exclusion parser and never means an ignored path by itself. Bounds,
cancellation, stderr failures, other exits and incomplete streams discard output; a matching
partial prefix cannot clear the guard. No command retry is introduced.

`Library.PutObjectContext` and `Store.CheckPathContext` wrap the SAME existing persistence/
exclusion cores. Strict capture passes its original caller context through every file object,
final tree object, writer-lock wait, quota walk and transitive Git guard. A pre-existing
deadline is preserved exactly; no per-file deadline renewal occurs. Legacy no-context guards
receive a disclosed 10-second fallback. Metadata/content formatting and immutable object
digests do not change. Cancelled checks return failure rather than a successful object/tree;
this is not a kernel-I/O interruption or whole-workflow quiescence guarantee.

Actual fake-hook sentinels first prove configured fsmonitor execution through a raw local
Git control. The marker then stays absent during strict capture INCLUDING same-store object
persistence, and during legacy capture. Local config, ambient count/keys and ambient parameter
injections all receive controls. Root's original separate-store probe passes unchanged.
Further tests cover exact argv/environment, unchanged deadline identity, check-ignore exit 1,
failed matching output, cancellation during a started persistence safety command, and legacy
golden bytes. No model, engine, CI or capture-policy authority follows from these tests.

During the combined local gate, the cancellation fixture's original one-second pre-cancel
startup wait failed under parallel load. It now waits for the observed fake-check entry
for at most eight seconds within a ten-second test context. The post-cancel join assertion
remains one second. No production capture, cancellation or resource threshold changed.
