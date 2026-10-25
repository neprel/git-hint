# Project root selection and retention

`store.Open` discovers a project from the requested directory upwards. It resolves
the selected project root once to its physical path. This supports ordinary root
aliases, including macOS `/var` aliases, while preserving one state-lock domain.

The logical and physical roots must identify the same directory. Their
`.hint/state/project.json` paths must identify the same regular file. The reader
uses the existing descriptor-rooted bounded state reader, then reads the file
again before loading the key. Both reads must return identical bytes. A replaced
root, replaced config file, changed bytes, unsafe state path, or expired context
causes refusal.

Project configuration has a 1 MiB byte limit, including whitespace. Both reads
share one 10-second context. Root discovery and filesystem metadata operations
remain synchronous filesystem operations; the context does not impose an OS-level
timeout on those calls. Configuration files above the limit now refuse instead
of receiving an unbounded read.

This change preserves stored config bytes, namespace, key references, and
repository references. It does not rewrite configuration or change `RepoID`.
The supported Unix platforms retain the existing state-lock implementation.

Strict state APIs still require a canonical root. A manually constructed store
with a symlink root still refuses. Symlinks below the root, including `.hint`,
`state`, `project.json`, and state-lock links, receive no new permission. The
destructive retention lock and definition-reference barrier remain unchanged.

The regression tests cover root aliases, physical identity, exact bytes, root
and config replacement, cancellation, unsafe paths, config bounds, and shared
lock ownership. CLI tests use synthetic eval data and disabled agent commands.
They verify ordinary pruning and forgetting, busy refusal, and retention refusal
when definition references exist. No source-capture or prospective-work authority
follows from opening a project.
