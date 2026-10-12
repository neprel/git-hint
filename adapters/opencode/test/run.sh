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
export HINT_KEY_DIR="$tmp/keys"
(cd "$tmp/project" && "$tmp/bin/git-hint" init >/dev/null)
cd "$here"
HINT_TEST_BIN="$tmp/bin/git-hint" HINT_TEST_PROJECT=$(cd "$tmp/project" && pwd -P) bun test plugin.test.ts
