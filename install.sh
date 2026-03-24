#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Homebrew dependencies ---
BREW_DEPS=(bd gh tmux)
missing=()
for dep in "${BREW_DEPS[@]}"; do
  command -v "$dep" &>/dev/null || missing+=("$dep")
done

if [[ ${#missing[@]} -gt 0 ]]; then
  if ! command -v brew &>/dev/null; then
    echo "Error: brew not found. Install Homebrew first, or manually install: ${missing[*]}"
    exit 1
  fi
  echo "Installing dependencies: ${missing[*]}"
  brew install "${missing[@]}"
fi

# --- Go toolchain ---
if ! command -v go &>/dev/null; then
  echo "Error: go not found. Install Go from https://go.dev/dl/ or via 'brew install go'"
  exit 1
fi

# --- Build and install ---
echo "Building ralph..."
make -C "$SCRIPT_DIR" build

echo "Installing to ~/.local/bin/ralph..."
make -C "$SCRIPT_DIR" install

# --- Git hooks ---
"$SCRIPT_DIR/scripts/setup-hooks.sh"

echo "Done. Ralph is installed at ~/.local/bin/ralph"
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$HOME/.local/bin"; then
  echo "Note: Add ~/.local/bin to your PATH if not already present"
fi
