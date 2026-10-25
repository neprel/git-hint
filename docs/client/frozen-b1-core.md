# Internal frozen-artifact B1 machinery

The runner uses one `compareFrozenIO` implementation for ordinary `ioPhase` and
the internal frozen-artifact primitive. Both use the existing parsed declaration,
reference repetition, per-case runner, exact stdout/status comparison, transcript
framing, TEST/CHECK output recipe and network provenance.

Ordinary `ioPhase` retains its original verifier deadline, reference application,
classification and output bytes. A candidate timeout, truncated output or failed
input remains a failed ordinary comparison. The internal strict primitive instead
returns no verdict for incomplete input/output, timeout, missing network isolation,
uncertain cleanup or a late return. An observed finite output/status difference
remains a failed comparison, not an unknown observation.

The strict primitive accepts only private runner artifact descriptors and a private
purpose-specific policy checkpoint. No production code issues that checkpoint.
Nil/unissued policy refuses before file/engine IO. There is no public raw-path API,
constructor, registration hook, catalogue entry, engine selector or completion
authority. Reference-preparation policy cannot authorize candidate verification.
Both artifacts must match the same image bound in the private policy. This is
internal testable execution machinery, not project admission or origin attestation.

One original endpoint is the earliest of the caller endpoint, entry plus 1,800
seconds and the existing declared verifier limit. A complete worst-case schedule
must fit before the first engine dispatch. It reserves 30 seconds for initial work
and 30 seconds for final work. Each of the exact `3 * N` executions receives the
declared per-case limit plus at most 10 seconds overhead, followed by 30 seconds
cleanup: 10 seconds stop, 10 seconds remove, then 10 seconds join. The case runner
still enforces the declared per-case limit. Earlier completion tightens all cleanup
endpoints; cancellation-independent cleanup never receives a renewed endpoint.

The extracted `runBoundedPhase` helper is the existing reference-preparation
lifecycle. Preparation retains the same schedule, bookkeeping, audit and cleanup
semantics. The strict primitive reuses its create/inspect/start/wait/stop/remove/
join path. It performs no image build, agent execution, reference application or
reference regeneration. Each copied artifact uses existing `referenceRetree`
expected-hash checks and its same-descriptor 96 MiB bound; stdout uses the existing
1 MiB per-run bound. These per-run limits are not a whole-operation IO or memory
guarantee. Output directories and parsed inputs remain owned runner inputs, with
the existing cooperative filesystem assumptions.

Synthetic SDK tests exercise the real shared comparison and lifecycle. Distinct
reference/actual TAR contents establish that the correct artifact enters each
run. An unchanged finite output passes; one changed candidate output fails.
Network, partial candidate stream, candidate timeout, byte drift, cleanup failure
and cancellation remain unknown. Additional unit controls cover incomplete input
and output-cap flags. Existing preparation and ordinary B1 golden tests retain
their output/provenance recipes. Source guards bind both new helper files and
reject omitted or changed bytes. No real engine or model participates.
