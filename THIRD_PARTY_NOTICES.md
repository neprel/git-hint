# Third-party notices

The `git-hint` binary includes the following components. Each keeps its license.

| Component | Version | License | Copyright |
|---|---|---|---|
| `mvdan.cc/sh/v3` (package `syntax`) | v3.14.1 | BSD-3-Clause | Copyright (c) 2016, Daniel Martí |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | Apache-2.0 | Santhosh Kumar Tekuri |
| `github.com/cyberphone/json-canonicalization` (Go) | `19d51d7fe467` | Apache-2.0 | Copyright 2018 Anders Rundgren |
| `golang.org/x/text` | v0.14.0 | BSD-3-Clause | Copyright (c) 2009 The Go Authors |
| Go standard library | Go toolchain | BSD-3-Clause | Copyright (c) 2009 The Go Authors |

The full license texts are in each module's `LICENSE` file (`go mod download -json` shows the location). None of the modules has a NOTICE file. The shell parser is used for syntax only; its interpreter package is not used.

The OpenCode plugin (`adapters/opencode/hint-adapter.ts`) is original code. It uses the public plugin interface of OpenCode (MIT) and includes no OpenCode code.
