#!/usr/bin/env bash
# Sync with remote, wait for version tag, then build.
# For manual use — evolve calls build-go.sh directly.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

# Sync with remote
git -C "$root" pull --rebase --quiet 2>/dev/null || true
if [ "$(git -C "$root" rev-list --count @{u}..HEAD 2>/dev/null)" != "0" ]; then
  git -C "$root" push --quiet 2>/dev/null || true
fi

# Poll for new version tag from GitHub Action
old_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")
for i in 1 2 3 4 5 6; do
  git -C "$root" fetch --tags --quiet 2>/dev/null || true
  new_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")
  if [ "$new_tag" != "$old_tag" ]; then
    break
  fi
  if [ "$i" -lt 6 ]; then
    sleep 3
  fi
done

# Build
exec "$(dirname "$0")/build-go.sh"
