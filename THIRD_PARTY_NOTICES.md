# Third-party notices

The `git-hint` binary includes the following components. Each keeps its license.

| Component | Version | License | Copyright |
|---|---|---|---|
| `mvdan.cc/sh/v3` (package `syntax`) | v3.14.1 | BSD-3-Clause | Copyright (c) 2016, Daniel Martí |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | Apache-2.0 | Santhosh Kumar Tekuri |
| `github.com/cyberphone/json-canonicalization` (Go) | `19d51d7fe467` | Apache-2.0 | Copyright 2018 Anders Rundgren |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause | Copyright (c) 2009 The Go Authors |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MIT and Apache-2.0 (license transition stated in its LICENSE) | The Go MCP SDK Authors |
| `github.com/google/jsonschema-go` | v0.4.3 | MIT | Copyright (c) 2025 JSON Schema Go Project Authors |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | Copyright (c) 2019 Segment.io, Inc. |
| `github.com/segmentio/asm` | v1.1.3 | MIT | Copyright (c) 2021 Segment |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | Copyright (C) 2016, Kohei YOSHIDA |
| `golang.org/x/oauth2`, `golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/time` | v0.35.0, v0.20.0, v0.47.0, v0.15.0 | BSD-3-Clause | Copyright 2009 The Go Authors |
| Go standard library | Go toolchain | BSD-3-Clause | Copyright (c) 2009 The Go Authors |

Release archives contain the full license texts of the Go toolchain and of each module in `licenses/`. For a source build, the texts are in each module's `LICENSE` file (`go mod download -json` shows the location). None of the modules has a NOTICE file. The shell parser is used for syntax only; its interpreter package is not used.

The OpenCode plugin (`adapters/opencode/hint-adapter.ts`) is original code. It uses the public plugin interface of OpenCode (MIT) and includes no OpenCode code.
