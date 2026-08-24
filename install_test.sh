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

# --- Test: bd binary is probed but the beads Homebrew formula is installed ---
# There is no Homebrew formula named "bd" (brew info bd -> no such formula);
# the formula providing the bd binary is named "beads". Run the dependency
# block for real against stubbed binaries so the probed-name -> formula-name
# mapping is exercised rather than pattern-matched.
dep_block=$(sed -n '/^# --- Homebrew dependencies ---$/,/^# --- Clone from template if new ---$/p' "$SCRIPT_DIR/install.sh" | sed '$d')
[[ -n "$dep_block" ]] || fail "could not extract the Homebrew dependency block from install.sh"

stub_root=$(mktemp -d)
trap 'rm -rf "$stub_root"' EXIT
brew_log="$stub_root/brew.log"
mkdir -p "$stub_root/brew_bin" "$stub_root/others_bin" "$stub_root/bd_bin"
cat >"$stub_root/brew_bin/brew" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$brew_log"
EOF
chmod +x "$stub_root/brew_bin/brew"
for stub in others_bin/gh others_bin/tmux others_bin/terminal-notifier bd_bin/bd; do
  printf '#!/bin/sh\n' >"$stub_root/$stub"
  chmod +x "$stub_root/$stub"
done

# Echoes what the dependency block asked brew to install, given a stub PATH.
brew_invocations_with_path() {
  : >"$brew_log"
  PATH="$1" "$BASH" -c "set -euo pipefail
$dep_block" >/dev/null || echo "dependency block exited non-zero under bash $BASH_VERSION (PATH=$1)" >&2
  cat "$brew_log"
}

installed=$(brew_invocations_with_path "$stub_root/brew_bin")
[[ "$installed" == "install beads gh tmux terminal-notifier" ]] ||
  fail "with nothing installed, expected 'install beads gh tmux terminal-notifier', got '$installed'"

installed=$(brew_invocations_with_path "$stub_root/brew_bin:$stub_root/others_bin")
[[ "$installed" == "install beads" ]] ||
  fail "with only bd missing, expected 'install beads', got '$installed'"

installed=$(brew_invocations_with_path "$stub_root/brew_bin:$stub_root/others_bin:$stub_root/bd_bin")
[[ -z "$installed" ]] ||
  fail "with the bd binary on PATH nothing should be installed, got '$installed'"

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
