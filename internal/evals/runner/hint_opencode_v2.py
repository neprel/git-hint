"""HINT adapter for OpenCode v2 under Harbor 0.23 (custom agent import path).

Harbor's built-in OpenCode agent installs the npm package opencode-ai (the 1.x line) and
runs it with flags that v2 does not have. OpenCode v2 is the npm package
@opencode/cli-<target>: one static binary per platform. This adapter keeps Harbor's
configuration and trajectory code and changes two steps:

- install: download the pinned target tarball from the npm registry and compare its
  SHA-512 with the pinned value before the binary is placed (no install script, no
  shell rc change, no Node.js);
- run: the v2 command line (private server, JSON events, auto-approve).

The pins come from the npm registry's dist.integrity values. HINT writes this file next
to the Harbor run; it is not a fork of Harbor.
"""

import shlex
from typing import override

from harbor.agents.installed.base import NonZeroAgentExitCodeError, with_prompt_template
from harbor.agents.installed.opencode import OpenCode
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

# version -> target -> SHA-512 (hex) of cli-<target>-<version>.tgz
PINS = {
    "2.0.22": {
        "linux-x64": "0e5575aa01030e756aa5358c3eabfbb421c2717a1dcd904568c7318ec898758e84e601c7d90ebc25f6ac564b324359eeea6d7cf3b59028684eb1ea44fa82b263",
        "linux-arm64": "b7fcaebbd0ea77f338e1d147f796e986eee290ee85013310b9593ff754019c2333b0ff3356b5d4ef30310f5c152e9618503a88a271268bcd17113945ab1edc87",
        "linux-x64-musl": "24b935ad01c2e8bbf59c067962fabdbe968e62c7b0c810963aecd094493f5f072766dd1f1a1eaf7cf3e6261bc1873f5524a0bf6c2e70375dc4b8eab80a67b092",
        "linux-arm64-musl": "0a111ed68e7275464effb37a2c185ba7a4d21b87311088a5f517a3bfc7d33f188a142054bbc137c1a973a411804aec2f15daf888497667f593a5b5876923c8f8",
    },
    "2.0.26": {
        "linux-x64": "508036ff5224f0768de780b8c69ec7f8e1e045fabdf795d48b1ad7fa42e616cf043e7d5f01c9a7dcea2237eddcdd57378c457f2c79a3d02cb548bea75b8c1d1b",
        "linux-arm64": "206df5e88f3056a9dda21cc502858d787f3778c03cb0a40546d31eba4d967e0e0353585a092833009cf7fe06f2b63c21e9b5dd591ed5dc5092adf8e8ada28383",
        "linux-x64-musl": "29fbcbbda70bc81d6849925180ebe2af984811f83606ca66f85dd2129add062cc18de9b9be8310ced88e608f4444b237d1ff6e14c81741629e994fe364aa6d5c",
        "linux-arm64-musl": "51ccd0f35593122a633209cb372f6e5c254b9b01e3465731fefb02ce14e97b1fa51dfbc0e434351ecfba0e32d61fd5eda3edf16a658c211da056cb6a2471b151",
    },
}


def install_command(version: str) -> str:
    """The shell command that installs the pinned binary as the agent user."""
    pins = PINS.get(version)
    if pins is None:
        raise ValueError(f"OpenCode {version} has no pinned hash")
    cases = " ".join(f"{t}) want={h};;" for t, h in sorted(pins.items()))
    return (
        "set -eu; "
        "case $(uname -m) in aarch64|arm64) arch=arm64;; x86_64) arch=x64;; *) echo unsupported architecture >&2; exit 1;; esac; "
        "if [ $arch = x64 ] && ! grep -qw avx2 /proc/cpuinfo; then echo 'OpenCode v2 needs the baseline build (no AVX2); not pinned' >&2; exit 1; fi; "
        "target=linux-$arch; "
        "if ldd --version 2>&1 | grep -qi musl || [ -f /etc/alpine-release ]; then target=$target-musl; fi; "
        f"case $target in {cases} *) echo no pin for $target >&2; exit 1;; esac; "
        "tmp=$(mktemp -d); "
        f"curl -fsSL -o $tmp/cli.tgz https://registry.npmjs.org/@opencode/cli-$target/-/cli-$target-{version}.tgz; "
        'got=$(sha512sum $tmp/cli.tgz | cut -d" " -f1); '
        'if [ "$got" != "$want" ]; then echo "OpenCode tarball hash mismatch" >&2; exit 1; fi; '
        "tar -xzf $tmp/cli.tgz -C $tmp package/bin/opencode; "
        "mkdir -p ~/.local/bin; mv $tmp/package/bin/opencode ~/.local/bin/opencode; chmod 755 ~/.local/bin/opencode; rm -rf $tmp; "
        "~/.local/bin/opencode --version"
    )


def config_command(model: str) -> str:
    """Writes the v2 config: the model through the bundled OpenAI-compatible provider
    (Chat Completions at OPENAI_BASE_URL, HINT's counting proxy), no auto-update, no
    sharing. v2's built-in "openai" provider uses the Responses API, whose stream the LAN
    server does not produce in the form v2 expects (live smoke 2026-10-18)."""
    mid = model.split("/", 1)[1]
    if not mid.replace("-", "").replace("_", "").replace(".", "").isalnum():
        raise ValueError("unexpected model name")
    return (
        '[ -n "${OPENAI_BASE_URL:-}" ] || { echo "no model URL" >&2; exit 1; }; '
        "mkdir -p ~/.config/opencode && "
        'printf \'{"provider":{"hint":{"npm":"@ai-sdk/openai-compatible","name":"hint",'
        '"options":{"baseURL":"%s","apiKey":"%s"},"models":{"' + mid + '":{"name":"' + mid + '"}}}},'
        '"autoupdate":false,"share":"disabled"}\' "$OPENAI_BASE_URL" "${OPENAI_API_KEY:-none}" '
        "> ~/.config/opencode/opencode.json"
    )


def run_command(model: str, instruction: str, resume: bool, flags: str) -> str:
    """The v2 command line; JSON events go to /logs/agent/opencode.txt as in Harbor."""
    mid = model.split("/", 1)[1]
    return (
        config_command(model) + " && "
        "~/.local/bin/opencode run --standalone --format json --thinking --auto "
        f"{'--continue ' if resume else ''}{flags}-m {shlex.quote('hint/' + mid)} -- {shlex.quote(instruction)} "
        "2>&1 </dev/null | stdbuf -oL tee /logs/agent/opencode.txt"
    )


class OpenCodeV2(OpenCode):
    """OpenCode v2 with a pinned, hash-checked binary."""

    @override
    def get_version_command(self) -> str | None:
        return "~/.local/bin/opencode --version"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "bash", "coreutils"))
        await self.exec_as_agent(environment, command=install_command(self._version or ""))

    @override
    @with_prompt_template
    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        self._instruction = instruction
        if not self.model_name or "/" not in self.model_name:
            raise ValueError("Model name must be in the format provider/model_name")
        env = dict(self.model_connection.env)
        env["OPENCODE_FAKE_VCS"] = "git"
        env["XDG_DATA_HOME"] = "/logs/agent/opencode/xdg-data"
        env["XDG_STATE_HOME"] = "/logs/agent/opencode/xdg-state"
        skills = self._build_register_skills_command()
        if skills:
            await self.exec_as_agent(environment, command=skills, env=env)
        flags = self.build_cli_flags()
        await self.exec_as_agent(
            environment,
            command=run_command(self.model_name, instruction, self._resume, (flags + " ") if flags else ""),
            env=env,
        )
        if messages := self._error_messages():
            raise NonZeroAgentExitCodeError("OpenCode emitted error event(s): " + "; ".join(messages[:3]))
