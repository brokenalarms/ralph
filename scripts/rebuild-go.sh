#!/usr/bin/env bash
# Sync with remote, wait for version tag, then build.
# For manual use — evolve calls build-go.sh directly.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

# Sync with remote
echo "[rebuild-go] Pulling latest..."
if ! git -C "$root" pull --rebase 2>&1; then
  echo "[rebuild-go] Pull --rebase failed, aborting build" >&2
  exit 1
fi

ahead=$(git -C "$root" rev-list --count @{u}..HEAD 2>/dev/null || echo "0")
if [ "$ahead" != "0" ]; then
  echo "[rebuild-go] Pushing $ahead commit(s)..."
  if ! git -C "$root" push 2>&1; then
    echo "[rebuild-go] Push failed, aborting build" >&2
    exit 1
  fi
fi

# Only poll for new version tag if we pushed (GitHub Action creates tags on push).
if [ "$ahead" != "0" ]; then
  old_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")
  echo -n "[rebuild-go] Waiting for version tag (current: $old_tag)"
  delay=1
  for i in 1 2 3 4 5 6 7; do
    git -C "$root" fetch --tags --quiet 2>/dev/null || true
    new_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")
    if [ "$new_tag" != "$old_tag" ]; then
      echo " → $new_tag"
      break
    fi
    if [ "$i" -lt 7 ]; then
      echo -n "."
      sleep "$delay"
      delay=$((delay * 2))
    else
      echo " (timed out, using $old_tag)"
    fi
  done
else
  git -C "$root" fetch --tags --quiet 2>/dev/null || true
fi

# Build
exec "$(dirname "$0")/build-go.sh"
