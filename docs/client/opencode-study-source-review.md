# OpenCode single-root study launch: source review

Status: source review only. The separate [internal study launch implementation](study-launch-policy.md) remains unqualified until finite real-binary validation. Ordinary launch defaults remain unchanged.

The reviewed tag is `9b4ec5714d481559990db0a816d5dec19541a814` (2.0.26). The reported binary build source is `5e73d5cb1f788637b1b3d5cf1298ea694cbad0c5`. Their [primary comparison](https://github.com/anomalyco/opencode/compare/5e73d5cb1f788637b1b3d5cf1298ea694cbad0c5...9b4ec5714d481559990db0a816d5dec19541a814) contains one release commit changing package versions and the lockfile, with no inspected source changes. This does not prove that a distributed binary implements the reviewed source.

## Permission and child-session paths

The legacy `permission.task` key [normalizes to `subagent`](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/v1/config/migrate.ts). The [native subagent tool](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/tool/plugin/subagent.ts) checks nesting depth and permission before creating or continuing a child. [Permission evaluation](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/permission.ts) returns explicit deny before publishing an ask. The CLI's `--auto` only answers asks; it does not override that deny. A blanket deny also removes the tool from the [advertised snapshot](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/tool.ts).

Other paths exist. [Configured commands](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/config/plugin/command.ts) create children for subagent/subtask commands or a subagent-mode command agent without checking the subagent permission. The current noninteractive CLI submits a plain prompt; this finding does not establish direct model access to configured commands. The plugin host exposes session creation with a parent. The server exposes session creation and forks. Shell commands and custom tools can start independent processes. No absence claim follows from root-only CLI output.

## Configuration precedence

[Configuration loading](https://github.com/anomalyco/opencode/blob/9b4ec5714d481559990db0a816d5dec19541a814/packages/core/src/config.ts) orders well-known, global, explicit file, direct project files, project directories, and injected content. Later matching permission rules win; agent-specific rules follow general rules. A generated global configuration alone does not control the effective configuration. Project directories can discover plugins.

A future opt-in study policy needs an isolated inspected configuration/state environment, disabled project configuration, no unreviewed plugins or configured commands, a fresh exclusive server, and policy bytes in the actual launch identity. `subagent` deny and zero subagent depth cover the native tool only. Unknown tools, shell child launches, conflicting evidence, incomplete capture, or unverified effective controls require UNKNOWN/exclusion. A finite real-binary verification remains a separate acceptance step. This review does not enable a trusted producer or change public schemas.

## Actual starting source

The task's declared BASE is an input overlay, not evidence of the resulting container filesystem. An opt-in source capture must read the unstarted container's `/app` after task/config overlays and topology validation, before the agent launch enclosure. It must use bounded archive validation, bind exact artifact bytes and monotonic capture offsets to the actual attempt, and keep declared BASE separate from observed start. Raw source stays in local audit artifacts. A resolver must scan the observed capture for that opt-in run; unavailable or invalid capture permits no BASE fallback. This capture alone does not enable source export, calibrated results, or trusted producer evidence.

## Local archive validation bounds

The internal `CapturePrelaunchTree` opt-in keeps ordinary valid-run behavior unchanged. Its source archive must contain both complete tar end blocks. File payload padding and any extra record padding must be zero. Extra record padding must contain complete 512-byte blocks. Physical EOF at an entry boundary, partial payload/padding/end blocks, concatenated archives, and nonzero trailing data fail before agent launch. This intentionally rejects malformed legacy input that a permissive tar reader can accept.

Three independent limits apply: 200,000 tree entries; 2 GiB total regular-file content; and 2,557,084,672 raw archive bytes. The raw bound includes headers, PAX/GNU extension metadata, payload padding, end blocks, and record padding. The shared freezer enforces it while reading the source, before prelaunch capture can succeed. Failed capture removes only its own output; an existing artifact remains untouched. Valid USTAR, PAX, GNU long-name archives and complete zero record padding keep their canonical artifact bytes and digest. These checks do not enable producer trust or a single-root launch policy.
