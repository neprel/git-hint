# Root CLI structural fixtures

These synthetic structural replays derive from three previously recorded qualification
captures of OpenCode 2.0.26. They contain no new model execution or calibration samples.
All session/message/call/part IDs, paths, commands, text, errors, titles, patches and output
content use synthetic replacements. Relative timing, event order, tool names, terminal
states and observed numeric shell exits remain representative of the recorded shapes.
Synthetic text lengths do not represent the original output lengths.

| Fixture | Records | Root terminal tools | Tool sequence | Terminal errors | Step starts / finishes |
|---|---:|---:|---|---:|---|
| p1 | 26 | 7 | read, read, read, read, edit, read, shell | 0 | 5 / 4 |
| p2 | 24 | 6 | read, read, read, read, edit, shell | 0 | 5 / 4 |
| p3 | 26 | 8 | grep, grep, shell, read, read, read, edit, shell | 2 | 5 / 4 |

The provider `executed:false` fields deliberately remain present on completed tools. The
decoder must not interpret them as success or failure. P3's errored greps have no output;
their missing sizes and uncertain navigation must not become measured zeros. An edit's
input and metadata file references preserve POSIX-normalized path equivalence.

The synthetic shell commands exercise DISCOVER and TEST classification. They do not claim
byte-for-byte command equivalence with qualification records. The shared classifier tests
separately cover READ, unknown and mixed shell commands, workdir resolution and plugin parity.

## Decoder contract

`DecodeCLIRoot(reader, CLIRootOptions)` returns one `CLIRootImport` only after the full capture
passes integrity checks. It performs no spool writes. Callers must persist its source and
observations atomically. The main pipeline owns actual frozen-baseline resolver construction,
work-unit closure and report/export integration; this fixture package changes none of them.

The orchestrator supplies the project key, canonical source configuration, actual opaque
task/family/run bindings, OpenCode version, `/app` resolver and complete-capture fact. A CLI
`step_finish` is not that fact. Missing final finishes produce diagnostics and no collection,
session/turn closure, usage, model-call, context or ledger record. The declaration covers
only observed root terminal tools; it never counts unobserved children or lost events as zero.

Source identity includes adapter/version, configuration and run/task/family declarations.
Observation identity uses the root session, message and call, independently of import time,
configuration and run relabeling. A changed body remains a conflict under that identity.
Calls with the same raw call ID in distinct messages remain distinct calls. Identical terminal
duplicates deduplicate; conflicting call or part identities reject the entire import.

Time fields use observed terminal state start/end. The interval can begin during input
tracking, so it is not execution duration. Import `CapturedAt` appears only as a labelled
diagnostic. Event provenance uses the observed CLI event timestamp. Replay with another
import time produces identical observations.

The `size` quantity counts decoded output-string UTF-8 bytes with the existing generic
`scanner` method. It is neither file size nor proven model delivery. No plugin or hook
measurement method describes these log records. Failed reads retain unknown size; errored
navigation adds an UNKNOWN secondary effect. DELEGATE similarly leaves child navigation
unknown. All raw content remains transient. `ToolEffect.Targets` also forbids accidental
JSON serialization of classifier paths.

The decoder bounds capture/line sizes and JSON nesting. It rejects malformed UTF-8/JSON,
duplicate object keys, truncated or unconfirmed captures, invalid required IDs/timestamps,
mixed root sessions, nonterminal tool records, invalid/conflicting exits and non-opaque
resolver results. Errors contain only static codes and record numbers. Unsupported agent
versions and unrecognized top-level event types fail closed.
