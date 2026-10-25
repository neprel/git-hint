# Local frozen verification API

`runner.VerifyFrozenIOForPolicy` provides a purpose-bound local Go entry to the
shared B1 machinery. Its installed procedure catalogue is empty. It cannot run
from an arbitrary caller profile, preparation selector, host filename, SDK client,
callback, environment endpoint, supplied count or claimed trust value.

`Task.FrozenIOInputs()` returns an immutable content snapshot only after a
successful `ExportBounded`. It copies the existing parsed B1 declaration and
effective generated task limits. Legacy `Export`, non-B1 tasks, zero tasks and
caller-written Task fields cannot mint it. Changing public Task fields, exported
files or copied identity maps does not rewrite the snapshot. The snapshot retains
source-object digests, exact spec/input identities, ordered cases, declared image,
task-config identity and verifier/CPU/memory/network settings. It proves no current
checkout, qualification, pre-work ordering or project source authority.

`ValidateFrozenIOArtifact` accepts only bounded bytes with exact domain, size and
digest. It reuses the existing frozen-tree validator without changing the byte
identity. No host path enters its API. Each TAR has at most96 MiB. Equal reference
and candidate bytes remain valid. Reference patch, HINT tree manifest and frozen
final TAR domains remain distinct. `ValidateFrozenIOReferenceTranscript` copies
the exact ordered zero-status reference cases and their original stdout, checked
through the existing `IOTranscriptDigest`. It binds the full input snapshot.

The operation uses the same catalogue, pinned Unix engine constructor, socket
checks, API/Info/image/platform binding and lifecycle as existing preparation.
Its purpose and digest are distinct. A preparation-only selector refuses before
file/engine IO. No production image or endpoint policy is installed. Descriptive
source identities cover the exact declared bundle/dependencies; they do not claim
a complete compiled-source closure or binary attestation.

The original endpoint includes resolution, workspace, engine open, cases, final
checks and cleanup. It is the earliest of entry+1800 seconds, caller endpoint,
installed policy cap and immutable task verifier limit. It admits the complete
`3*N` worst-case schedule before opening an engine. The initial30 seconds includes
the at-most10-second pinned constructor. Each case retains its declared limit,
at-most10-second overhead and10/10/10-second stop/remove/join slots. Final30 seconds
stays inside the same endpoint. Nothing renews the original anchor.

Two fresh reference runs per case must agree with each other and the retained
reference transcript before any candidate case. Candidate runs once per case.
Complete finite output/status differences produce `finite_difference_observed`.
Complete equality produces `finite_equivalence_observed`. Incomplete streams,
timeouts, input failure, network/resource/terminal disagreement, cancellation,
cleanup uncertainty or late return remain unknown. Actual case records preserve
physical execution audits and original stdout. Failed operations retain available
partial records. Neither state establishes project completion authority.

Copied handle/observation identities, stdout and content hashes are **LOCAL Go API
data only**. Do not serialize them directly into evalreports or normal exports.
[Normal wire privacy](../standard/privacy-v0.1.md) prohibits raw local identifiers,
private content and private content digests. A later reviewed V2 keyed projection
is required. This API adds no report fields, writer or export behavior.

Factories cap each input/retained stdout at the existing declaration bounds and
an artifact at96 MiB. Execution caps combined retained handle bytes at256 MiB.
Those limits do not bound earlier caller allocations or every repeated physical
copy. Original source reads reuse bounded export/library primitives. Generated
task reads and the fresh0700 workspace retain the existing cooperative
no-outside-writer filesystem assumption, not an adversarial hard IO guarantee.

Prepared reference bytes can come from exact existing factual preparation loads.
An actual project artifact producer, terminal source fence, original-start
handoff, durable observer, trusted completion projection and full-cost population
proof remain separate missing dependencies. A generic content-valid candidate or
local finite observation supplies none of them.
