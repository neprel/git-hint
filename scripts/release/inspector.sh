#!/bin/sh
# MCP Inspector check of an installed git-hint: lists the tools with the Inspector's strict
# schema checks (any schema error or portability warning fails), and calls hint_status in a
# temporary project. Needs git and Node.js (npx); no HINT server, no coding agent.
#
#   scripts/release/inspector.sh <directory with git-hint>
#
# INSPECTOR overrides the Inspector command (default: npx of the pinned version).
set -eu
bin=$(cd "$1" && pwd)
inspector=${INSPECTOR:-"npx --yes @modelcontextprotocol/inspector@2.8.0"}
fail() { echo "inspector FAIL: $*" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
p="$tmp/project"
mkdir -p "$p"
printf 'package main\n\nfunc main() {}\n' > "$p/main.go"
git -C "$p" init -q
(cd "$p" && HINT_KEY_DIR="$tmp/keys" "$bin/git-hint" init >/dev/null) || fail "init"
cat > "$tmp/servers.json" <<JSON
{"mcpServers": {"hint": {"command": "$bin/git-hint", "args": ["mcp", "--project", "$p"], "env": {"HINT_KEY_DIR": "$tmp/keys"}}}}
JSON
list=$($inspector --cli --config "$tmp/servers.json" --server hint --method tools/list --strict 2>"$tmp/strict.err") || fail "tools/list"
if grep -Eq '^(Warning|Error)|[1-9][0-9]* (errors?|warnings?)' "$tmp/strict.err"; then
	grep -v '^git-hint mcp' "$tmp/strict.err" >&2
	fail "tool schema errors or portability warnings"
fi
n=0
for t in hint_status hint_collect hint_evals hint_eval_run hint_export_preview hint_recommend hint_plan hint_import_recommendation \
	hint_changes hint_evidence hint_compare hint_find_definition hint_importers hint_file_outline hint_context; do
	echo "$list" | grep "\"name\": *\"$t\"" >/dev/null || fail "tool $t not listed"
	n=$((n + 1))
done
status=$($inspector --cli --config "$tmp/servers.json" --server hint --method tools/call --tool-name hint_status) || fail "tools/call"
echo "$status" | grep -q '"isError": *true' && fail "hint_status returned an error"
echo "inspector: OK, $n tools listed, hint_status called"
