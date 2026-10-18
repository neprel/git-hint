#!/bin/sh
# Clean-host smoke test of a git-hint release archive. It needs only sh, tar, git and a
# SHA-256 tool; no Go, no network, no HINT server, no coding agent.
#
#   scripts/release/smoke.sh <release dir with SHA256SUMS> [os/arch]
#
# Steps: verify the archive checksum; extract it; run git-hint with a clean PATH (the
# archive directory, /usr/bin, /bin) and a temporary HOME; check `git-hint --version`,
# `git hint --version`, help, the capability report, an offline init + collect + export
# preview, doctor, and the MCP server (initialize, tools/list over stdio).
set -eu
dist=$(cd "$1" && pwd)
case "${2:-}" in
"") os=$(uname -s | tr 'A-Z' 'a-z'); arch=$(uname -m) ;;
*) os=${2%/*}; arch=${2#*/} ;;
esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
archive=$(cd "$dist" && ls git-hint_*_"${os}_${arch}".tar.gz)
fail() { echo "smoke FAIL: $*" >&2; exit 1; }
step() { echo "smoke: $*"; }

step "checksum of $archive"
line=$(grep "  $archive\$" "$dist/SHA256SUMS") || fail "no checksum line"
want=${line%% *}
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$dist/$archive"); else got=$(shasum -a 256 "$dist/$archive"); fi
[ "${got%% *}" = "$want" ] || fail "checksum mismatch"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar -xzf "$dist/$archive" -C "$tmp"
bin=$(cd "$tmp"/git-hint_* && pwd)
[ -x "$bin/git-hint" ] || fail "no git-hint program in the archive"
for f in LICENSE THIRD_PARTY_NOTICES.md README.md licenses/go/LICENSE; do [ -f "$bin/$f" ] || fail "missing $f"; done

mkdir -p "$tmp/home"
run() { env -i PATH="$bin:/usr/bin:/bin" HOME="$tmp/home" LANG=C "$@"; }
command -v git >/dev/null 2>&1 || fail "git not found"
run git --version >/dev/null || fail "git is not in /usr/bin or /bin"

step "version"
v1=$(run git-hint --version) || fail "git-hint --version"
v2=$(run git hint --version) || fail "git hint --version (Git external subcommand)"
[ "$v1" = "$v2" ] || fail "the two version outputs differ"
echo "$v1" | head -1
echo "$v1" | grep "commit [0-9a-f]\{40\}" >/dev/null || fail "no commit in the version"
# `git hint --help` is answered by Git (it opens a manual page); use `git hint help`
run git-hint --help | grep "usage: git hint" >/dev/null || fail "git-hint --help"
run git hint help | grep "usage: git hint" >/dev/null || fail "git hint help"
caps=$(run git hint version --capabilities) || fail "capabilities"
echo "$caps" | grep "platform $os/$arch: supported" >/dev/null || fail "platform not reported as supported"
echo "$caps" | grep "embedded grammars: none" >/dev/null || fail "grammar line"

step "offline init, collect, export preview (no server)"
p="$tmp/project"
mkdir -p "$p/pkg"
printf 'package pkg\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n' > "$p/pkg/add.go"
printf 'package pkg\n\nimport "fmt"\n\nfunc Show() { fmt.Println(Add(1, 2)) }\n' > "$p/pkg/show.go"
(cd "$p" && run git init -q && run git -c user.name=s -c user.email=s@example.invalid add -A \
	&& run git -c user.name=s -c user.email=s@example.invalid commit -qm base) || fail "git setup"
(cd "$p" && run git hint init >/dev/null) || fail "init"
(cd "$p" && run git hint collect >/dev/null) || fail "collect"
(cd "$p" && run git hint status >/dev/null) || fail "status"
(cd "$p" && run git hint export --dry-run >/dev/null) || fail "export preview"
(cd "$p" && run git hint doctor >/dev/null) || fail "doctor"
[ -z "$(cd "$p" && run git status --porcelain)" ] || fail "the work tree changed (store not ignored?)"

step "MCP over stdio"
# stdin stays open for 3 s: the server ends the session at end of input
out=$(cd "$p" && { printf '%s\n%s\n%s\n' \
	'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
	'{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	'{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 3; } | run git hint mcp 2>/dev/null) || fail "mcp"
for t in hint_status hint_collect hint_export_preview hint_recommend hint_plan hint_import_recommendation \
	hint_changes hint_evidence hint_compare hint_find_definition hint_importers hint_file_outline hint_context; do
	echo "$out" | grep "\"name\":\"$t\"" >/dev/null || fail "MCP tool $t not listed"
done
echo "$out" | grep '"protocolVersion":"2025-11-25"' >/dev/null || fail "protocol version"

step "OK $os/$arch"
