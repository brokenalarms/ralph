#!/usr/bin/env bash
set -euo pipefail

TARGET="$HOME/.local/bin/ralph"

rm -f "$TARGET"
echo "Removed $TARGET"
