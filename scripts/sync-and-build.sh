#!/usr/bin/env bash
# Sync with remote, push local commits, wait for CI version tag.
# Pull triggers the post-merge hook which fetches tags and builds.
# When we push and get a new tag, we rebuild to pick up the new version.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

# Pull first — gets us up to date and triggers post-merge hook → build.
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

  # Wait for CI to create the version tag, then rebuild with it.
  old_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")
  echo -n "[sync] Waiting for version tag (current: $old_tag)"
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

  exec "$root/scripts/build-go.sh"
fi
