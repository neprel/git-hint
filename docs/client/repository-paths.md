# Repository paths through project root aliases

Ordinary event and context-file paths can use a logical filesystem alias while
`store.Open` retains the canonical project root. Both consumers use the same
`store.RepositoryPath(root, path)` resolver. The resolver shares the existing
nearest-existing-prefix algorithm, with explicit error handling. Existing
`resolve` callers retain their previous fallback behavior.

The result contains the resolved absolute path, repository-relative path,
presence, and mode. A present final leaf must not be a symlink. Its original and
resolved paths must identify the same physical file. Outside paths, traversal
components, and protected `.hint` or `.git` components refuse. Case variants of
protected components also refuse. Repository containment uses path components;
a sibling whose name starts with the repository name remains outside.

Activity targets use the relative path to derive the existing file ID. Directory
targets remain supported. Missing targets remain declarations, including paths
whose suffix does not exist yet. They do not establish a read, successful write,
capture, or actual work. A missing or inaccessible repository root refuses.

Variant context files use the same resolver but require a present regular file.
The ordinary content read remains separate from resolution. Resolution does not
guarantee an atomic content snapshot or protect a subsequent read against every
concurrent filesystem change. Strict source/state readers and their policies do
not change.

Synthetic tests cover explicit root aliases, identical physical/logical file
IDs, actual mutation inference, context preparation, missing declarations,
directory observations, legacy fallback, private paths, final links, and parent
links that point outside the repository. Qualification execution uses the
existing fake spawn recorder; no agent or engine starts.
