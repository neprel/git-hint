# Optional offline ripgrep for the OpenCode study runtime

This programmatic opt-in supplies exactly one pinned dependency to OpenCode 2.0.26.
It does not enable ordinary CLI installation or assert image/runtime compatibility.
The ordinary runtime and the original `single-root-v1` selection remain unchanged.

Set `runner.RunOptions.Ripgrep` to `&runner.RipgrepSource{BinaryFile: localFile}`.
For a controlled suite, set `pipeline.SuiteOptions.RipgrepBinary` instead.
Both require OpenCode, `single-root-v1`, prelaunch capture and the frozen planned
launch digest. Plan with `runner.DockerLaunchDigestWithRipgrep`, not the original
`DockerLaunchDigestWithPolicy`. The suite uses the same renderer for both planned
and observed launch identities. There is no CLI flag or automatic default.

The runner reads a bounded regular file without following a leaf symlink. It
checks exact length, descriptor/path identity and SHA-256. It refuses an unknown
or mismatched artifact, unsupported host admission or unsupported runtime target.
There is no download, PATH fallback, generic installer or caller-selected pin.

The single supported artifact is the upstream ripgrep 15.1.0 GNU aarch64 release:

- Release archive: `ripgrep-15.1.0-aarch64-unknown-linux-gnu.tar.gz`
- Upstream: <https://github.com/BurntSushi/ripgrep/releases/tag/15.1.0>
- Archive SHA-256: `2b661c6ef508e902f388e9098d9c4c5aca72c87b55922d94abdba830b4dc885e`
- Extracted `rg`: 4,543,848 bytes; SHA-256 `968cabe8efed72fd8fd482cb76b6084fcb695fc5293af7fb62296b02f487fb69`
- License: MIT or Unlicense; retain upstream `COPYING`, `LICENSE-MIT` and `UNLICENSE` with artifact preparation.

The runner does not extract an archive. Artifact preparation and acquisition are
separate operations. The reviewed binary is ELF64 AArch64 ET_DYN and requires
`/lib/ld-linux-aarch64.so.1`, GLIBC 2.17/2.18 and its dynamic dependencies. Only the
`linux-arm64` GNU target is supported. In particular, the musl target is refused.
A matching digest does not prove that an actual image supplies the loader/libraries.

One common recipe copies `rg` to `/opt/hint/bin/rg`, owned by root with mode 0755.
The existing PATH already selects this directory first. The study inspection
requires prior absence, then checks the installed regular-file metadata and exact
content digest in the same fresh, not-started, exact-image container. This file
uses a separate fixed archive bound of its exact size plus 64 KiB. Existing
configuration/launcher inspection keeps its 2 MiB bound and strict TAR reader.

The new runtime identity binds the dependency pin, target, install mode/path and
inspection rule. The actual execution identity also binds the verified binary
and image. Existing receipts/class evidence cannot stand in for these identities.
Local provenance retains the dependency version/hash. Normal privacy projection
continues to control exported launch/execution identities.

Permissions, gate egress, read-only HOME, fixed configuration paths, model/request
limits and task selection do not change. The opt-in does not prove executable
behavior, OpenCode tool resolution, source/binary correspondence, no child
sessions, eligibility, or a successful end-to-end journey. Actual-image ABI and
finite tool behavior validation remain separate acceptance requirements.
