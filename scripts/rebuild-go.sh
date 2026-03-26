#!/usr/bin/env bash
# Rebuild Go ralph binary. Always rebuilds — Go's build cache makes no-op builds fast.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

# Sync with remote: pull first, then push if ahead.
git -C "$root" pull --rebase --quiet 2>/dev/null || true
if [ "$(git -C "$root" rev-list --count @{u}..HEAD 2>/dev/null)" != "0" ]; then
  git -C "$root" push --quiet 2>/dev/null || true
fi

# Get current tag before polling
old_tag=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "none")

# Poll for a new tag (GitHub Action takes a few seconds)
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

version=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "v0.1.0-dev")
echo "[rebuild-go] Building ralph ${version}"

ldflags="-X github.com/brokenalarms/ralph/internal/config.Version=${version#v}"
if cd "$root/go" && go build -ldflags "$ldflags" -o "$HOME/.local/bin/ralph" ./cmd/ralph/; then
  echo "[rebuild-go] Installed to ~/.local/bin/ralph"
else
  echo "[rebuild-go] Build failed" >&2
  exit 1
fi
