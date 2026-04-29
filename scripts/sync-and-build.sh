#!/usr/bin/env bash
# Sync with remote, push local commits, then rebuild.
set -euo pipefail

root="${RALPH_PROJECT_DIR:-$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)}"

if ! git -C "$root" fetch origin 2>&1; then
  echo "[sync] Pull failed" >&2
  exit 1
fi
if ! git -C "$root" rebase origin/main 2>&1; then
  echo "[sync] Rebase onto origin/main failed" >&2
  exit 1
fi

ahead=$(git -C "$root" rev-list --count origin/main..HEAD 2>/dev/null || echo "0")
if [ "$ahead" != "0" ]; then
  if ! git -C "$root" push 2>&1; then
    echo "[sync] Push failed" >&2
    exit 1
  fi
fi

exec "$root/scripts/build-go.sh"
