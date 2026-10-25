# Expected backend start capture

`pipeline.SuiteOptions.ExpectedBackend` and `runner.RunOptions.ExpectedBackend`
optionally require an exact backend metadata match before the agent starts. They
use the existing common Docker runner and backend-capture transport. Nil preserves
existing behavior, capture ordering and launch identities. No CLI/default enables
this option.

Supply a `runner.CaptureSnapshot` with every allowlisted fact: image digest,
project-keyed host/container/served-model references, engine start timestamp,
restart count, prompt-details and prefix-caching booleans, maximum model length,
and supported reasoning/tool parsers. Missing or unknown facts refuse. Explicit
false booleans remain observed values. The runner takes owned copies.

With this option, the normal start capture occurs after setup and prelaunch
capture, before the caller's launch hook. The hook remains the last step before
the agent start request. The
runner compares the complete sanitized response with the expected snapshot under
the original context and existing capture bounds. Error, timeout, cancellation,
missing facts or mismatch refuses before agent start and model traffic. The normal
finish path retains captures and performs cleanup. There is no extra probe,
external receipt, TTL or renewed time window.

This observes metadata equality only. It does not observe health, mounted weight
contents, loaded weights or byte/source correspondence. The backend can change
after the capture, including during the hook. Normal end-capture and downstream consistency checks remain
required. The option creates no producer trust, eligibility or canonical-cost
claim and changes no activity schema or local claim policy.
