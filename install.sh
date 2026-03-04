#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="/usr/local/bin/ralph"

ln -sf "$SCRIPT_DIR/ralph.sh" "$TARGET"
echo "Installed $TARGET -> $SCRIPT_DIR/ralph.sh"
