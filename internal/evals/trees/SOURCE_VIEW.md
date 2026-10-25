# Local selected-source observations

`OpenSelectedSourceView` is an opt-in internal seam. It uses the existing strict
selected-file capture loop and descriptor reader. It does not grant source,
project, definition, start, catalogue, export, or retention authority.

The supported source profile is exactly Git 2.56.0, a born SHA-1 repository with
an ordinary root `.git` directory, and canonical Unix paths. Git itself discovers
the system candidate with `var GIT_CONFIG_SYSTEM`. The implementation requires the
source-proved `var GIT_CONFIG_GLOBAL` order: XDG candidate, then HOME candidate.
It does not guess an installation prefix or manually merge Git configuration.
Unknown versions, layouts, source origins, includes, worktree configuration,
extensions, relative excludes, unresolved source paths, and unsafe files refuse.

The one explicit invocation freezes the canonical executable, copied selectors,
fixed safe argv, and copied child environment. Selected source inspection and all
transitive object-write exclusion guards use that same invocation. An outer
context invocation cannot replace the source view's own profile. Existing callers
without a scope retain their ordinary invocation. Executable observations use a
separate narrowly scoped descriptor helper because readgit cannot import trees or
store. Configuration and source files use only the existing trees descriptor core.
Executable hashes/identities are repeated observations, not OS attestation or
proof that the bytes read necessarily execute unchanged after the final check.

## Original budget and counter scopes

The selected session starts first. Its private monotonic origin precedes discovery.
Its deadline is exactly origin + 30 seconds, tightened by the parent's deadline.
Both receipt types use this origin. Each caller can only tighten this bound.
There are at most two attempted captures; failure cancels the whole view. No
parallel capture, renewed allowance, third capture, or partial receipt is admitted.

The 38 source-query bound includes six discovery commands, four initial snapshot
commands, and fourteen commands for each capture (three four-command snapshots
plus the existing two inventories). Source-query stdout and stderr have a combined
16 MiB bound. Every command also retains its existing stdout bound and 64 KiB
stderr bound. These caps do not bound Git's internal configuration reads.

Persistence guards have a separate dispatch/output scope. One `PutObjectContext`
uses at most four Git guard dispatches: tracked/check-ignore validation before the
cache check and again before a new write. A cache hit uses at most two. Two
captures write at most `2 * (10,000 selected files + 1 tree) = 20,002` selected
objects, plus `2 * (1,024 final metadata objects + 2 envelopes) = 2,052` source
objects. Thus the conservative common dispatch cap is
`38 + 4 * (20,002 + 2,052) = 88,254`. It is not a promise that this much work fits
within 30 seconds. All dispatches, executable rechecks, and persistence retain the
same original context. The receipt separately records actual source queries,
persistence guards, executable read count/bytes, and both output scopes. It does
not claim 38 total processes, 39 total executable reads, or a whole-operation
16 MiB I/O bound. Counts describe dispatch attempts, not independent process-start
observations. Successful receipts require complete joined command output.

Source metadata has at most 1,024 reads and 4 MiB returned bytes per pass, including
repeated checks; a regular file has a 1 MiB cap. Initial discovery reads charge the
first pass. Strict selected-content limits remain 10,000 paths, 8 MiB/file, and
64 MiB/pass by default. Positive caller limits can tighten these limits.
The executable helper accepts at most a 64 MiB regular executable; each dispatch
rechecks its canonical no-follow path, bytes, leaf identity and ancestor node chain.
Failed changing-file reads can observe one extra 32 KiB chunk before refusal.

## Facts and repeat semantics

Config list framing is exactly `scope NUL origin NUL key[LFvalue] NUL`. Bare and
explicit empty values differ. Typed config-get framing is `scope NUL origin NUL
expanded-value NUL`; only Git expands `~/` in the selected excludes path. A clean,
complete exit-1 empty nonmatch is admitted only by the config-path or detached-HEAD
query. A nonempty stderr, truncated stream, other error, or other query refuses.

Metadata facts include candidate config, repository config, global/local excludes,
HEAD/index/packed-refs, an active loose ref, absent worktree config, required absent
`objects/info/alternates` and `objects/info/http-alternates`, and ignore
files on the actual selected path chains. Present facts include bytes/object/node
identities; absent facts include the checked nearest-parent identity. They remain
local. The config query bytes and state envelope use the existing local immutable
object store. Returned receipts and selected receipts copy their private slices.
The source digest uses the `git-selected-source-observations-v1` domain. It does not
replace the separately bound invocation, selected inventory, stage inventory, or
tree identities.

The first ignore chain is discovered after selection, so its receipt states
`discovered_after_selection`. The second pass checks that same chain before
selection and states `observed_before_selection`. Repeated passes compare metadata
bytes and node/absence identities, config bytes, HEAD/branch/index observations,
selected path node/absence facts, stage tuples, and selected content/tree digests.
They do not make a cross-file atomic snapshot or prove the absence of ignored,
unselected, special, external, or outside-HINT work. Git may omit untracked FIFOs.
Strict ancestor identities can conservatively refuse after unrelated directory
metadata changes. Failed captures can leave unreferenced immutable objects; this
slice supplies no retention implementation.

## Pure receipt consumers

`ResolveStrictCaptureLimits` delegates to the existing strict normalizer.
`ValidateSelectedSourcePair(limits, selectedA, selectedB, sourceA, sourceB)` checks
bounded structural/semantic consistency without IO. Explicit normalized limits
also apply to tighter policies. Index modes need not equal unstaged worktree modes.
It supports genuine empty and indexed-absent results. The helper bounds raw strings
at8 MiB per receipt and its pure ignore-chain projection at8 MiB; this consumer
bound does not add a source-capture projection cap. The shared projection now
checks its existing1,024-entry cap during insertion as well as at return.

The helper checks declared paths/stages/facts, counts, byte totals, semantic/tree
projections, version/profile, counter relations and ordering within the private
reader clock scope. It cannot prove a clock origin, capture origin, authority,
actual environment selection or referenced raw object bytes. A future producer
still needs a reviewed closed wire codec with required/non-null fields, plus
bounded raw config/state/tree/content object verification through existing reader
and parser recipes. No transaction, late-commit witness, start, retention or
catalogue protocol is implemented by these helpers.

The accompanying private empty-chain copy normalization preserves non-nil empty
slices for the existing repeat comparison. It permits the current reader's born-
empty two-pass case while still refusing an empty-to-nonempty ignore-chain change.
It changes no selected/source facts, encoded bytes, limits or absence authority.

The missing codec seam is a future pure `DecodeSelectedSourcePair` for four closed,
required/non-null receipt envelopes before this typed validator. Referenced-object
validation must separately reuse the existing config parser/tree encoder plus a
closed StateObject (Head/Branch/Facts) decoder against the bounded actual bytes.
Those codecs are not implemented here. Their names do not grant producer or
transaction authority; their exact APIs still need review.

## LOCAL v1 wire and raw-object consistency

`EncodeSelectedSourcePair` emits four canonical envelopes in `SelectedSourceWire`.
`DecodeSelectedSourcePair` returns a private copied `DecodedSelectedSourcePair`.
The exact envelopes are `{kind,receipt}` with kinds `git-selected-receipt-wire-v1`
and `git-source-receipt-wire-v1`. Existing Go JSON member names are LOCAL v1 wire
names. Explicit reviewed field lists cover every receipt/fact/stage/limits member;
a shape-drift guard refuses changes until a wire-version review. Every member is
required and non-null, including `Indexed=false`, empty strings and empty arrays.
Duplicate/case-variant/unknown members, trailing values, noninteger counters, and
invalid types refuse without partial output or raw error values.

Consumer projection bounds are 8 MiB per envelope (32 MiB total), JSON depth16 and
250,000 value nodes/envelope. These are not new reader-capture limits: an oversized
projection can refuse without implying corrupted source. `Receipts()` returns deep
copies. There is no capture-origin, clock-origin, authority, start or handoff token.

`ObjectRefs()` returns the copied sorted exact raw-reference plan. `ValidateObjects`
requires exactly that byte map, at most22,054 objects/128 MiB distinct bytes.
Per-object caps are at most8 MiB; raw config query bytes are at most4 MiB; known
source-file sizes retain the existing1 MiB source limit. Deduplicated role aliases
intersect caps and must agree on exact known sizes. A known-empty object has a read
cap of at least1 but exact expected size0; a one-byte replacement refuses. No
method opens a file, Git process, store, URL, scanner or callback.

Every referenced raw byte object gets the existing SHA-256 and size checks. Tree
bytes equal the existing tree encoder projection, including empty `files:null`.
State bytes retain the producer's exact `json.Marshal{Head,Branch,Facts}` encoding,
with the same fact array and supported branch/ref shape. Head is40hex for exact
SourceView v1: born-empty means an empty selected tree at a real `--allow-empty`
commit, not unborn HEAD. Detached Branch is empty and has no optional loose-ref
fact. The shared state encoder preserves nil/empty byte distinctions even where
the supported source profile does not emit a nil fact array.

Config query bytes use the SAME existing NUL parser/profile as the producer;
file-origin scope must match an observed-present source fact. The validator does
not implement Git config evaluation or binary index/packed-ref/ref parsing. It
cannot prove query values derive from raw config files, or CLI Head/Branch values
from raw Git files. Those file bytes get hash/size consistency only. Private raw
paths, inode/absence claims, actual environment/executable selection and source
origin remain unproved. Supplied consistent maps never mint source authority.

These codecs add no source transaction, policy registry, retention edge, original
start or durable acceptance/handoff treatment. Those producers remain held.
