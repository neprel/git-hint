# Evalreport 0.3.1: prospective intent assertions only

This distinct encoding describes prospective project verification intent and receipt
assertions. It provides no actual prospective-start implementation, source catalogue,
reference preparation, artifact capture, export, CLI, runtime projection or project-cost
authority. Default `report.Builder.Build` remains 0.2.1 and emits no 0.3.1 reports.
Existing 0.1/0.2/0.3.0 schemas stay unchanged.

An optional project `scope.verification_intent`, when present, requires all original0.3.0
identity fields plus the following closed0.3.1 fields:

- `intent_version=project-verification-intent-v1`;
- `reference_input={domain: hint-library-reference-patch-archive-v1,digest}`;
- `reference_preparation_ref/digest`, `capture_policy_ref/digest`, `capture_source_ref/digest`;
- `start_binding={recommendation_set_digest,repo_id,base_snapshot_id,base_snapshot_digest,base_hint_tree_digest}`.

The intent version assigns `reference_artifact_digest` the **prepared final TAR** domain,
`hint-b1-frozen-final-tar-v1`. The patch archive is input only. HINT tree manifests, captured
actual TARs and historical q3 TARs remain separate identities. Digest equality across domains
is neither required nor proof of equivalence. The schema asserts identities; it resolves none
of the actual stored artifacts, source policies, preparation executions or prospective timing.

The Go checker reuses the0.3.0 history/receipt rules. For0.3.1, intent-only receipts also need
revision1/project/open/no members/no predecessor, complete contiguous immutable history,
unchanged scope and an explicit plan. The bound base snapshot ID must occur in scope's base
snapshots. A verification record echoes the complete intent and matches the frozen set
content digest as well as existing set/plan IDs. Original scope/intent digests stay distinct
from the latest frozen-open revision. Existing mandatory checks, trust minima, separate
verification execution, closure and conflict checks remain. Unknown fields/versions refuse.

All shared `report-v031-*` positives are synthetic assertions. They establish no actual
source, qualification, pre-work intent or successful project completion. The invalid corpus
includes schema-valid history/base/set/echo substitutions plus malformed/domain/downgrade
controls. Typed0.3.0 roundtrip and ordinary writer checks preserve legacy behavior. The old
schemas reject0.3.1; the default writer refuses new intent fields instead of dropping them.

Schema-valid receipts provide UNKNOWN project authority. Before using a receipt as proof,
a consumer must independently resolve the exact immutable definition, qualification, source
policy, source audit, prepared reference, original start, actual artifact and distinct
verification execution. It must resolve complete journal/application history and conflicts.
A matching digest or a passed verdict assertion alone does not establish those facts.
No source-policy or whole-cost capability follows from this contract. Git-selected artifacts
exclude unselected/ignored work; they do not prove whole-work completeness or untracked
special-file absence.
