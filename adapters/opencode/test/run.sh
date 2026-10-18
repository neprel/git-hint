#!/bin/sh
# Offline test of the OpenCode adapter plugin: builds git-hint, prepares a temporary Git
# project, and runs the Bun tests. No OpenCode process, no model, no network.
# Usage: adapters/opencode/test/run.sh   (needs go, git and bun)
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(cd "$root" && go build -o "$tmp/bin/git-hint" ./cmd/git-hint)
mkdir -p "$tmp/project/canary_dir_e1d2"
printf 'package main\n\nfunc main() {}\n' > "$tmp/project/canary_dir_e1d2/app.go"
git -C "$tmp/project" init -q
printf 'package main\n\n// Rate gives a rate. HINTCANARY_RATE_COMMENT\nfunc Rate(a, b int) int { return a + b }\n' > "$tmp/project/canary_dir_e1d2/rate.go"
export HINT_KEY_DIR="$tmp/keys"
(cd "$tmp/project" && "$tmp/bin/git-hint" init >/dev/null && "$tmp/bin/git-hint" collect >/dev/null)
# one real hint_context answer of the MCP server (stdio), for the text-only conversion test
{ printf '%s\n%s\n%s\n' \
	'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}' \
	'{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	'{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hint_context","arguments":{"seed":"Rate"}}}'; sleep 2; } |
	(cd "$tmp/project" && "$tmp/bin/git-hint" mcp) > "$tmp/mcp.jsonl" 2>/dev/null
cd "$here"
HINT_TEST_BIN="$tmp/bin/git-hint" HINT_TEST_PROJECT=$(cd "$tmp/project" && pwd -P) HINT_TEST_MCP="$tmp/mcp.jsonl" bun test plugin.test.ts
