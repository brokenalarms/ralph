#!/usr/bin/env bash
# Rebuild Go ralph binary. Always rebuilds — Go's build cache makes no-op builds fast.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

echo "[rebuild-go] Copying prompts into go/cmd/ralph/prompts/"
rm -rf "$root/go/cmd/ralph/prompts"
cp -r "$root/prompts" "$root/go/cmd/ralph/prompts"

version=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "v0.1.0-dev")
echo "[rebuild-go] Building ralph ${version}"

ldflags="-X github.com/brokenalarms/ralph/internal/config.Version=${version#v}"
if cd "$root/go" && go build -ldflags "$ldflags" -o "$HOME/.local/bin/ralph" ./cmd/ralph/; then
  echo "[rebuild-go] Installed to ~/.local/bin/ralph"
else
  echo "[rebuild-go] Build failed" >&2
  exit 1
fi
