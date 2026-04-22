#!/usr/bin/env bash
# Build Go ralph binary from current source. No git operations.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"

version=$(node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('$root/package.json','utf8')).version)" 2>/dev/null || echo "0.1.0-dev")
echo "[build] Building ralph v${version}"

ldflags="-X github.com/brokenalarms/ralph/internal/config.Version=${version}"
if cd "$root/go" && go build -ldflags "$ldflags" -o "$HOME/.local/bin/ralph" ./cmd/ralph/; then
  echo "[build] Installed to ~/.local/bin/ralph"
else
  echo "[build] Build failed" >&2
  exit 1
fi
