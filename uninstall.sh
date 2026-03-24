#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

make -C "$SCRIPT_DIR" uninstall
echo "Ralph uninstalled"
