#!/usr/bin/env bash
# Pull latest and push local commits. Version bumping happens in the pre-push
# hook; tagging happens in CI from package.json.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

echo "[sync] Pulling latest..."
if ! git -C "$root" pull --rebase 2>&1; then
  echo "[sync] Pull --rebase failed" >&2
  exit 1
fi

ahead=$(git -C "$root" rev-list --count @{u}..HEAD 2>/dev/null || echo "0")
if [ "$ahead" != "0" ]; then
  echo "[sync] Pushing $ahead commit(s)..."
  if ! git -C "$root" push 2>&1; then
    echo "[sync] Push failed" >&2
    exit 1
  fi
fi
