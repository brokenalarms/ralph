#!/usr/bin/env bash
# Verify install.sh checks dependencies, builds Go binary, installs it, and runs setup-hooks.
set -euo pipefail

failures=0
fail() { echo "FAIL: $1"; ((failures++)); }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Test: dependency list includes bd, gh, tmux but not jq ---
# Go ralph uses encoding/json, so jq is no longer needed.
deps_line=$(grep -E '^BREW_DEPS=' "$SCRIPT_DIR/install.sh")
echo "$deps_line" | grep -q 'bd' || fail "BREW_DEPS missing bd"
echo "$deps_line" | grep -q 'gh' || fail "BREW_DEPS missing gh"
echo "$deps_line" | grep -q 'tmux' || fail "BREW_DEPS missing tmux"
echo "$deps_line" | grep -q 'jq' && fail "BREW_DEPS should not include jq"

# --- Test: clones from agent-skeleton template if repo is new ---
# When installing fresh, the script should bootstrap from the agent-skeleton template.
grep -q 'agent-skeleton' "$SCRIPT_DIR/install.sh" || fail "should clone from agent-skeleton template"
grep -q '\.git' "$SCRIPT_DIR/install.sh" || fail "should check if .git exists"

# --- Test: Go toolchain check is present ---
# The install script must verify that go is available before building.
grep -q 'command -v go' "$SCRIPT_DIR/install.sh" || fail "missing Go toolchain check"

# --- Test: builds via make ---
# The Go binary should be built using make, not a manual go build command.
grep -q 'make.*build' "$SCRIPT_DIR/install.sh" || fail "should build via make"

# --- Test: installs via make ---
# Installation should use make install to place the binary in ~/.local/bin.
grep -q 'make.*install' "$SCRIPT_DIR/install.sh" || fail "should install via make"

# --- Test: runs setup-hooks.sh ---
# Git hooks must be installed for post-merge/post-rewrite rebuilds.
grep -q 'setup-hooks.sh' "$SCRIPT_DIR/install.sh" || fail "should run setup-hooks.sh"

# --- Test: no longer symlinks ralph.sh ---
# Go ralph replaces bash ralph; the old symlink to ralph.sh should be gone.
grep -q 'ralph\.sh' "$SCRIPT_DIR/install.sh" && fail "should not reference ralph.sh"

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
