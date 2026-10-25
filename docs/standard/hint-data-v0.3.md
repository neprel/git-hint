# HINT activity privacy v0.3

Version 0.3.0 is a draft minor schema family. It corrects the normal export of private model-ledger and membership identities. It does not change usage arithmetic.

## Compatibility

Readers advertise supported major.minor families. A v0.3 reader accepts 0.3 patch versions under the same schemas. Older 0.1/0.2 schema bytes and patch behavior stay unchanged. Unsupported minor versions refuse. Evalreport versions use a separate protocol and schema namespace.

The normal exporter selects0.3 for model-ledger or membership evidence. It never falls back to exporting their private identity fields under 0.2 after a projection error. Existing local spools and runner records keep their exact original values.

## Private identity projection

K is the existing project key. DeriveID and EvalRef retain the standard framing and 128-bit output. Each new reference denotes minimized equality evidence, not a new physical record or a loading capability.

| New field | Exact existing primitive and private input |
|---|---|
| execution_provenance_ref | DeriveID(K,"execution-provenance",raw execution digest B) |
| image_ref | DeriveID(K,"image",raw engine image ID) |
| launch_configuration_ref | EvalRef(K,"observed-config","setting_override",raw launch digest A) |
| membership_policy_ref | DeriveID(K,"membership-policy",raw membership policy digest) |
| gate_policy_ref | DeriveID(K,"gate-policy",raw gate policy digest) |

Ledger agent_run and membership launch use the SAME execution-provenance role. Backend start/end snapshots use the SAME image role. launch_configuration_ref reuses the actual existing configuration reference. The separate execution_ref remains an execution INSTANCE identity.

Raw source-content digests, names and their reverse mappings never enter these fields. Do not prefix a keyed reference with sha256 or reinterpret a legacy digest slot. The v0.3 schemas use closed new fields. Canonical configuration digests still seal actual allowlisted HINT values.

A v0.3 ledger source uses hint-model-ledger@0.3.0. A v0.3 administrative membership source uses hint-project-orchestrator@0.2.0. All inherited CLI-root, correlation, closure, ledger-entry and lifecycle checks remain active. Source shape alone proves no trusted origin.

## Normal export and limits

One normal allowlist projection constructs new wire values. It preserves private evidence and original observation identities. Membership event links and heads seal projected events. Each predecessor_epoch_digest seals the COMPLETE projected previous membership body. Canonical no-private-intent scope/revision identities stay unchanged after existing validation. A private future verification intent cannot pass through this path without its corresponding reviewed projection.

Incremental export processes selected observations. It retains required predecessor epochs only for membership units in that selection. Unrelated old project journals do not become dependencies of a new controlled ledger.

Lossless ledger instance and anchor timestamps require time_resolution_seconds0. A policy that exports a model-ledger body with a truncating time resolution receives model_ledger_profile_unsupported. The exporter does not silently preserve finer timestamps. Aggregate or explicitly withheld ledger bodies keep their policy behavior. General coarse-ledger support remains unavailable.

Counts, monotonic offsets, outcomes and available coverage retain their original meanings. Provider components remain descriptive until a reviewed consumer maps them. New references grant no profile admission, runtime enforcement, source authority, work completion, calibration or economic claim. Consumers require separately reviewed namespace-bound profile/configuration evidence and preserve UNKNOWN on missing or conflicting evidence.
