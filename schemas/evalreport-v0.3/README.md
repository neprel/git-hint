# Evalreport 0.3.0 receipt contract

This version adds project `scope.verification_intent`, optional top-level
`project_verifications`, and `closure.project_verification_ref`. Default report writers
remain 0.2.1. No runner, CLI, spool, completion authority or economic result is enabled.

The start intent freezes definition, qualification, specification, input, reference and
mandatory-set identities in the original scope before implementation. Verification records
separately bind that start scope/intent and the latest actual open revision. They carry
plan/set and result snapshot/artifact identities, separate verification execution,
mandatory check verdicts and output digests. No task text, source, paths, input bytes,
prompts or copied usage are allowed.

Verification-bearing receipts are self-contained: include revision 1, all intervening
revisions, the latest open revision, and optionally its immediately subsequent closing
revision. Revisions use RFC 8785 scope/revision digests. Scope stays frozen. Terminal
attempts stay immutable. The frozen attempt contains actual member task identities. A
closing revision cannot change actual member/usage fields or other attempts. Future
incremental writers must re-emit the referenced immutable history; this contract does
not implement a writer.

The mandatory list contains sorted unique opaque check references. Its RFC 8785 SHA-256
must match the digest frozen in the intent. Verdict checks match that exact ordered set.
Aggregate trust equals the minimum of all checks and the qualification cap; unvalidated
qualification caps trust at unknown. A passed verdict needs every mandatory check to
pass, validated qualification, and process-separated aggregate trust. A failed check gives
a failed verdict; every other case stays unknown. The currently supported verifier tuple
is `hint-docker` / `cli-file-v1`. Identical verification replay is allowed; conflicting
content under one reference is invalid. Servers also check identity conflicts across
receipts within the namespace.

These are consistency checks over receipt assertions. The checker cannot establish real
pre-work order, actual artifact bytes, qualification provenance, runner separation, latest
external history, plan/set/snapshot resolution, or later application from JSON alone.
These require the separately reviewed future runner/writer/projection. The private server
therefore reads project 0.3 trust as unknown and refuses new project-verification cost
authority. Retention refuses deletion in namespaces carrying the new strong references
until their full mapping receives review. A valid synthetic fixture proves only software
behavior. Legacy 0.1/0.2 schemas stay unchanged and reject 0.3. Existing legacy reads do not
gain project authority. Downgrading a new receipt leaves unsupported fields and fails.
