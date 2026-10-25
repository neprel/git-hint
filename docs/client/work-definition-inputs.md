# Local work-definition inputs

This internal Go API creates an immutable local input record. It does not start a work unit. Its only state is `resolved_inputs`, with basis `stored_input_consistency_only`. There is no CLI, report emitter, launch integration, completion authority, or source-policy admission.

## API

```go
project.WriteWorkDefinitionInputs(ctx, s, project.DefinitionRequest{
    Label: label,
    ExactSetBytes: exactRecommendationSet,
    PlanID: planID,
    Family: family,
    DeclaredConfiguration: configuration,
    QualifiedIO: qualify.ResolveIORequest{
        EvalName: name,
        ManifestDigest: manifestDigest,
        PreviousManifestDigest: previousDigest,
    },
}, now) (project.WorkDefinitionRef, error)

project.LoadWorkDefinitionInputs(ctx, s, exactRef) (*project.WorkDefinitionInputs, error)
handle.Identity() project.WorkDefinitionIdentity
```

`WorkDefinitionRef` contains `RecordRef` and `RecordDigest`. The digest covers the exact canonical stored record. The binding digest inside that record covers its body without the binding-digest field. Neither digest establishes trusted origin. Returned identities and qualified-object bytes are copies.

The writer shares the existing plan validator through `resolvePlanForBase`. It reads bounded, exact stored export-state, snapshot, and latest-reference bytes. That basis is not a current-worktree measurement. The legacy `resolvePlan` still obtains `CurrentState` first and retains its existing validation order. The writer never substitutes a new scan for missing stored facts.

## Exact inputs and local identity

The record binds the namespace, pseudonymous intended unit and family, exact set/plan/base snapshot, declared configuration, exact qualification request, and resolved qualification identities. Declared configuration does not establish effective launch configuration. Original stored bytes become content-addressed objects. The record names their digests and sizes; it does not duplicate prompts or source text.

`qualify.ResolveQualifiedIO` resolves the exact stored qualified and prior manifests. It uses the existing library digest and evaluation validators. It checks q3 archive control inventory, mandatory checks, spec, inputs, reference patch, image, setup, and raw runner provenance. It rejects every encountered trial prefix without exactly one supported result. Reported runner provenance is not attestation. Qualification control counts remain separate from future verification oracle runs.

Reference patch archive, HINT reference-result tree, and raw archived final TAR retain separate domains. This API does not select or prepare a final reference artifact. The local IO identity retains its declared container workdir, including `/app`; that value is not a host source path.

Store binding includes the observed canonical root node, root location, key identity, and namespace. Copying the record to a foreign store does not produce a valid handle. Compiled reader, writer, and resolver identities describe source bytes; they do not admit a source catalogue.

## Create, replay, and exact reload

Creation inspects current received local work journals, membership records, sources, spool, activity batches, and controlled-run index under one nonblocking state lock. The neutral `runjournal.Parse` reader checks exact controlled-run bytes against their filename, explicit manifest/member/unit fields, planned unit inventory, derived ownership, configuration, and complete revision chain. It uses the existing work-unit revision schema. Missing eligibility fields never default to known absence. A flag alone never establishes eligibility or authority. Failed terminal attempts remain in received history; missing task ownership refuses. Valid unrelated controlled journals can pass. Distinct runs can retain the same validated objective task; the parser does not declare those runs independent. Existing work for the requested unit, orphaned or conflicting ownership, corrupt present history, and unsupported history shapes refuse creation. Default-path absence can pass only as explicitly inspected absence. It does not establish absence of deleted, unreceived, or outside work.

An identical create replay returns the original reference only after the same current-history checks. Changed inputs refuse replay. A later work start therefore blocks create replay. Exact reload remains readable after that start: it checks frozen input consistency, not current eligibility or prospective order. Reload never recertifies a start. Missing original qualification objects refuse reload; copied record objects do not replace that source.

Records use exclusive, durable creation under `.hint/state/work-definitions`. No operation rewrites an existing record or retrofits a work unit. A failed creation can leave an empty protected directory or unreferenced input objects. No automatic cleanup removes that boundary.

## Bounds and descriptor checks

The shared store reader opens canonical root ancestors and targets without following symlinks. Regular-file reads use one nonblocking descriptor. The reader checks descriptor and named-path identity before return. It rejects FIFO, special-file, replacement, growth, mode-change, and traversal cases. Directory reads bound entries and check stable directory identity. These APIs fail closed on unsupported platforms; existing project locking already requires a supported platform.

Each operation uses one caller deadline capped at 30 seconds. Nested qualification and persistence retain that deadline. Qualification caps metadata at 1 MiB, each q3 archive at 32 MiB, and its aggregate reads at 128 MiB. The writer separately caps received-history bytes at 64 MiB, individual history metadata at 8 MiB, directory entries at 1,024, configured repositories at 16, and records at 1 MiB. Exact reload caps its frozen set/export/snapshot objects at 80 MiB in addition to qualification reads. Recommendation sets retain the existing 16 MiB document bound. Exceeding a bound refuses; it never supplies empty data.

## Retention and lock ownership

Strong-reference deletion planning for these new records is not implemented. `Prune`, `Forget`, `Purge`, `DeleteIndex`, and `CollectGarbage` refuse destructive work whenever the definition-record path exists or cannot be inspected safely. An empty directory, corrupt file, or symlink also refuses. The guard executes before deletion.

Each destructive public entry takes the same nonblocking state lock as the writer. Private index deletion and garbage collection do not reacquire it. The existing ordinary callers enter without ownership: CLI collect releases its lock before `AfterCollect`; MCP collect does the same; pipeline `Run` calls pruning without a state lock; CLI evaluation forgetting also enters unlocked. Standalone library operations follow that boundary. Concurrent ownership returns `store.ErrBusy`; no caller infers ownership from that error. Ordinary pipeline pruning still succeeds when no definition path exists.

The finite production caller map is:

| Entry | Destructive call | Ownership at call |
| --- | --- | --- |
| `internal/cli/cli.go`, collect | deferred `evalPass` → `AfterCollect` | explicit unlock before scheduling |
| `internal/mcpserver/tools.go`, collect | `AfterCollect` | explicit unlock before call |
| `internal/evals/pipeline/pipeline.go`, `Run` | `Library.Prune` | no state lock |
| `internal/evals/pipeline/pipeline.go`, session pruning | `Library.DeleteIndex`, then `Library.CollectGarbage` | no state lock |
| `internal/cli/eval.go`, evaluation forgetting | `Library.Forget` or `Library.Purge` | no state lock |

Standalone destructive APIs use a fixed ten-second safety-inspection context, then attempt lock acquisition without waiting. Their private helpers reuse that ownership. The session pruning path cannot delete an index before the definition barrier. No destructive caller joins or bypasses another caller's lock.

## Unimplemented authority

This record does not establish an original start, prospective capture policy, selected-source capture, fresh prepared final reference, verification execution, accepted completion, or complete cost population. A future start writer must resolve those independent proofs before it creates original revision 1. Schema-valid report 0.3.1 assertions cannot supply them. Default legacy writers and existing report bytes remain unchanged.

The tests use synthetic locally stored q3 and recommendation fixtures. They execute the actual resolver and durable writer. They do not run an agent, model, container engine, backend, or qualification runtime.
