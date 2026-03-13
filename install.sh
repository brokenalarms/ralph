#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="$HOME/.local/bin/ralph"

# --- Dependencies ---
DEPS=(jq tmux gh)
missing=()
for dep in "${DEPS[@]}"; do
  command -v "$dep" &>/dev/null || missing+=("$dep")
done

if [[ ${#missing[@]} -gt 0 ]]; then
  echo "Installing dependencies: ${missing[*]}"
  if command -v brew &>/dev/null; then
    brew install "${missing[@]}"
  else
    echo "Error: brew not found. Install Homebrew first, or manually install: ${missing[*]}"
    exit 1
  fi
fi

# --- Symlink ---
mkdir -p "$(dirname "$TARGET")"
ln -sf "$SCRIPT_DIR/ralph.sh" "$TARGET"
echo "Installed $TARGET -> $SCRIPT_DIR/ralph.sh"

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$HOME/.local/bin"; then
  echo "Note: Add ~/.local/bin to your PATH if not already present"
fi
