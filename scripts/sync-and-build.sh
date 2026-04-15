#!/usr/bin/env bash
# Sync with remote, push local commits, wait for CI version tag, then rebuild.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

"$root/scripts/sync-and-tag.sh"
exec "$root/scripts/build-go.sh"
