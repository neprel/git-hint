# Local verified-baseline collection

`CollectVerifiedBaseline(ctx, s, BaselineCollectRequest{RepoID, Limits})` performs a
new current collection of the exact configured repository. It returns an owned
`BaselineCollection` with copied `Ref`, `Identity`, `Snapshot` and a distinct
nonserializable `BaselineAcceptance`. The name "verified" means checked retained
input consistency, not origin/issuer authority. No public producer admission changes.

The sole `captureSourcePair` core is factored from `CaptureWorkDefinitionSource`.
It uses the real strict SourceView, two actual passes, the existing closed pair
codec, and complete bounded referenced objects. No fake reader/version, ambient
scan, new evaluator, or additional Git selection algorithm exists. Explicit limits
may only narrow the strict defaults (8 MiB/file, 64 MiB/pass, 10,000 selected paths).
The pair's referenced raw object closure stays under 128 MiB. Local receipt records
stay under 8 MiB; native snapshot/parent inputs stay under 8 MiB each. Reload uses
one 128 MiB aggregate returned-object read budget, including receipt/config and
snapshot dependencies, so extreme valid captures may refuse reload rather than
publish an incomplete result.

The operation materializes only verified present regular selected objects into an
owned temporary directory. The explicit nonempty list enters the normal `scan.Scan`
with the configured RepoID/key. Index file hashes must match every retained content
object. `snapshot.Build` receives the retained checked Git HEAD and an honest
`unknown` worktree state; it does not run ambient `ReadGitState`. A second use of
that same builder verifies the structural graph against the actual recorded parent
ID; a stored StateKey alone cannot select the unchanged fast path and evade the
check. Modes belong to the physical commitment, not the structural snapshot.

## Exact local bindings

`BaselineObservationRef` is `{RecordRef, RecordDigest}`. `BaselineBinding` contains
that ref, `RepoID`, native `SnapshotID`/`SnapshotDigest`, and `Physical`:

- `TreeDigest`: actual selected path/mode/size/content manifest object.
- `SelectionDigest`, `StageDigest`: exact selected occurrence and index-stage facts.
- `SourceDigest`, `GitStateObject`: exact observed Git/config/source relation.
- `Selection`: fixed `git_selected_regular_source_v1` scope.

The private observation also binds namespace, actual store/root, config object,
limits, scanner/reader and declared implementation/dependency identities, four
receipt objects, complete object inventory, native snapshot/Index object, previous
pointer and predecessor object, actual observation/derivation times, and the
prepublication monotonic offset. These offsets do not convert the separate inner
reader clock into the outer clock. Raw paths/content hashes never enter ordinary
HINT export. Source/dependency pins are declared source families, not a complete
transitive binary attestation.

## Publication and acceptance

All capture, scan, persistence, reload, lock release and final checks use the
original `min(parent deadline, entry+30s)` endpoint. One nonblocking state lock
serializes cooperating APIs. Normal CLI/MCP collection already owns that lock;
legacy `Collect` remains caller-serialized and takes no nested lock. This is not
OS compare-and-swap or protection against hostile same-owner mutation.

The shared snapshot writer creates native records once. Its private before-pointer
callback seals an observation with state `prepared_baseline_publication_unknown`.
It checks the exact original latest pointer and frozen config immediately before
publication. The stored observation cannot claim publication or successful return.
After pointer publication, the operation reloads the exact observation, checks the
pointed native triple/config, releases the lock, then checks the original endpoint.
Only then does it mint the baseline-purpose checkpoint, with no further blocking IO.
The acceptance binds the exact record digest, actual store and writer. Unknown,
foreign, zero, cross-kind, expired or deserialized checkpoints do not match.

The shared `WriteStateContext` preserves the ordinary atomic-replace algorithm and
successful bytes. It carries the original context through Git path guards; local
filesystem calls remain cooperative. A late write can leave a factual prefix or
published pointer while returning failure. No replay/reload recovers on-time process
acceptance or durable observer authority. Partial evidence remains retained.

The common destructive-retention barrier exists before any new objects. Empty,
orphan, corrupt and linked baseline roots block destructive retention. Immutable
snapshot collisions refuse. Unchanged native triples may be reused with a fresh
physical observation; that observation does not certify any earlier physical state.
A→B→A uses the accepted predecessor-bound snapshot identity, never overwrites A.

## Definition/source v2

`DefinitionRequest.Baseline != nil` explicitly selects
`project-work-definition-inputs-v2`. The writer resolves the normal actual stored
plan/set and exact qualified inputs, then binds the baseline ref, native triple,
physical tuple and complete native snapshot/Index data. The normal plan's exported
snapshot digest remains distinct from the native snapshot digest. Qualified input,
reference patch and prepared-reference TAR identities remain separate domains.

`CaptureWorkDefinitionSource` uses `project-capture-source-v2` only for definition
v2. It reloads the exact baseline and independently captures through the same core.
Selected presence/absence, path, mode, size/content, root, index stages, Git/config
source relation and structural graph must match. A mode-only change refuses even
when the structural triple stays unchanged. Definition/source v1 protocol paths
remain available without promotion for v1-only units. Definition kinds are mutually
exclusive per unit. Create/replay checks the other exact kind-specific reference
under the shared state lock. Any counterpart, including malformed or unreadable
data, refuses. Reload also refuses already-coexisting rejected records; no repair,
downgrade, upgrade, or alternate-version source acceptance occurs. Same-kind
unchanged replay remains available when the counterpart is absent. Writer source gates rotate with these changes;
this package does not claim historical writer-hash readability or migration.

## Scope and remaining boundary

Git-selected artifact scope excludes ignored files and `.git` content. An ignored
addition is not a census finding. A `.git` addition can change observed directory
metadata and cause conservative refusal, which still does not prove full `.git`
coverage. Generated setup files and captured `/app`-with-`.git` form a distinct
population. Their mapping needs the separate actual immutable prelaunch TAR and
prestart graph handle. This package does not add that callback, historical graph
publication, source/issuer authority, original start, whole-work coverage, server
admission, or calibration eligibility. Independent observer admission remains separate.
