#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="$HOME/.local/bin/ralph"

mkdir -p "$(dirname "$TARGET")"
ln -sf "$SCRIPT_DIR/ralph.sh" "$TARGET"
echo "Installed $TARGET -> $SCRIPT_DIR/ralph.sh"

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$HOME/.local/bin"; then
  echo "Note: Add ~/.local/bin to your PATH if not already present"
fi
