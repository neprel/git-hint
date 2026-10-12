# Install git-hint

`git-hint` is one executable. Git runs it as `git hint` when it is on `PATH`. It needs Git 2.30 or later; it does not need Go, a C library, a HINT server or a coding agent.

## 1. Platforms

| Platform | Archive | Status |
|---|---|---|
| macOS, Apple silicon | `git-hint_<version>_darwin_arm64.tar.gz` | supported |
| macOS, Intel | `git-hint_<version>_darwin_amd64.tar.gz` | supported |
| Linux, x86-64 | `git-hint_<version>_linux_amd64.tar.gz` | supported |
| Linux, ARM64 | `git-hint_<version>_linux_arm64.tar.gz` | supported |
| Windows | none | deferred: not built, not supported, not tested |

The release notes of each version name the systems on which the archives were tested. The program is built without C code (`CGO_ENABLED=0`): the Linux program is statically linked and does not depend on the C library version.

## 2. Download and verify

Download the archive for your platform, `SHA256SUMS` and, optionally, `manifest.json` from the release page `https://github.com/neprel/git-hint/releases/tag/<version>`. With the GitHub CLI:

```sh
V=v0.1.0
gh release download "$V" --repo neprel/git-hint --pattern "git-hint_*_darwin_arm64.tar.gz" --pattern SHA256SUMS
```

Verify the checksum before you extract the archive:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing     # macOS
sha256sum -c SHA256SUMS --ignore-missing         # Linux
```

Each line must end with `OK`. Optional: verify the build provenance (the release workflow attests every archive):

```sh
gh attestation verify git-hint_0.1.0_darwin_arm64.tar.gz --repo neprel/git-hint
```

## 3. Add to PATH

```sh
tar -xzf git-hint_0.1.0_darwin_arm64.tar.gz
mkdir -p ~/.local/bin
cp git-hint_0.1.0_darwin_arm64/git-hint ~/.local/bin/
export PATH="$HOME/.local/bin:$PATH"          # add this line to your shell profile
```

On macOS, a program that a browser downloaded can carry the quarantine attribute. A download with `gh`, `curl` or `wget` does not. If macOS blocks the program, verify the checksum first, then remove the attribute: `xattr -d com.apple.quarantine ~/.local/bin/git-hint`.

## 4. Check the installation

```sh
git-hint --version
git hint --version
git hint version --capabilities
```

The two version lines must be equal. `git hint --help` opens Git's manual page lookup; use `git hint help` or `git-hint --help` for the command list.

`version --capabilities` lists what this build supports: Go declarations and imports (Go standard library parser), other languages at file level, no embedded grammars, the MCP protocol versions and tools, the agent adapters, and optional external indexers. The optional indexers (`scip-go`, `scip-typescript`, `scip-python`, `sem`) are separate programs; none is bundled and this version does not use them. Their absence does not reduce the features of this version.

## 5. Build from source

`git hint version` shows where its build data come from (`metadata:`). The data differ by install method:

| Method | Version | Commit and date | `metadata:` |
|---|---|---|---|
| Release archive | release version | commit and commit time of the release | `release` |
| `go build ./cmd/git-hint` in a Git checkout | `0.1.0-dev` (the source default) | from Git; `+modified` when the work tree has changes | `git` |
| `go install github.com/neprel/git-hint/cmd/git-hint@v0.1.0` | the module version, `0.1.0` | `unknown`: Go records no commit for a module download | `go-install` |
| `go install …@latest` before a tagged release, or `@<commit>` | `0.1.0-dev`; the Go pseudo-version is shown as `module v0.0.0-…` | `unknown`; the pseudo-version contains the commit time and a commit prefix | `go-install` |
| `go build` without `.git` (for example from a source archive) | `0.1.0-dev` | `unknown` | `unstamped` |

Limits: only release archives carry a verified commit, a checksum and provenance. A source build is as trustworthy as the source tree it comes from. A version that the HINT data standard does not accept (such as a Go pseudo-version) is never written into documents; the client writes `0.1.0-dev` instead.

```sh
go install github.com/neprel/git-hint/cmd/git-hint@v0.1.0     # Go 1.27 or later
```

Release archives are built with `go run ./tools/release build` from the files of one commit only. The same commit and Go version give the same programs and tar streams on any host, and byte-identical archives on a host of the same CPU architecture (see [releasing](releasing.md)).

## 6. Update and uninstall

- Update: install the new archive over the old program. The local store (`.hint/`) and the project key stay; the release notes state any store or data format change.
- Uninstall: delete the program. To remove HINT data, delete `.hint/` in each project and the key directory (`~/Library/Application Support/git-hint/` on macOS, `$XDG_CONFIG_HOME/git-hint/` or `~/.config/git-hint/` on Linux). The OpenCode adapter file is `.opencode/plugins/hint-adapter.ts` in a project.

## 7. Privacy

Installing and running `git-hint` sends nothing. Only `git hint recommend` (and the MCP tool `hint_recommend`, when the local rule allows it) sends a validated, content-free package, and only to the server that you configure. There is no default server and no telemetry. See [privacy](privacy.md).
