# Releasing git-hint (maintainers)

## Build

```sh
go run ./tools/release build -version v0.1.0 -out dist    # clean work tree required
go run ./tools/release verify -dir dist
scripts/release/smoke.sh dist                             # this machine's platform
```

`tools/release` builds `darwin/amd64`, `darwin/arm64`, `linux/amd64` and `linux/arm64` with `CGO_ENABLED=0`, `-trimpath`, an empty build ID and the version, commit and commit time set at link time. Each archive holds `git-hint`, `LICENSE`, `THIRD_PARTY_NOTICES.md`, `README.md` and the license texts of the Go toolchain and every linked module (`licenses/`). The tool rejects a binary that contains test fixture content, home directory paths, private IPv4 addresses or credential patterns. It writes `SHA256SUMS` and `manifest.json`.

**Reproducibility.** The archive time and the version date are the commit time, so the same commit built with the same Go version gives byte-identical archives on any host. Limits: a different Go version, different module sources (the module checksums in `go.sum` prevent this) or a different `-version` value change the output.

## Workflows

| Workflow | Trigger | Does |
|---|---|---|
| `ci` | push to `main`, pull request | format, vet, tests (Linux amd64 and arm64, macOS), race test of the MCP server, conformance suite, OpenCode plugin tests, release build and smoke test |
| `release` (manual) | `workflow_dispatch` with a version | build, verify, smoke test and MCP Inspector check on four native runners; publishes nothing |
| `release` (tag) | push of a tag `vX.Y.Z` or `vX.Y.Z-<label>[.N]` with label alpha, beta, rc or dev (the pre-release forms that the HINT data standard allows in a producer version) | the same, then build provenance attestation, GitHub release with `docs/releases/<tag>.md` as notes, then download, provenance check and smoke test of the published archives on the four runners |

All actions are pinned to commit SHAs. Jobs have read-only permissions; only the publish job can write releases and attestations, with the built-in token.

## Steps

1. Write `docs/releases/vX.Y.Z.md` (supported platforms, tested systems, data standard version, agent support, known limits).
2. Run the `release` workflow manually with the version; wait for all smoke jobs.
3. Push a signed or annotated tag from `main`: `git tag -s vX.Y.Z -m "git-hint vX.Y.Z" && git push origin vX.Y.Z`. Only maintainers can push release tags (repository rule).
4. Check the `verify-published` jobs.
