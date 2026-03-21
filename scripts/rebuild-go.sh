#!/usr/bin/env bash
# Rebuild Go ralph binary if go/ files changed between $1 and $2 (or HEAD).
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
old="${1:-HEAD~1}"
new="${2:-HEAD}"

if git diff-tree --no-commit-id --name-only -r "$old" "$new" | grep -q '^go/'; then
  version=$(git -C "$root" describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo "v0.1.0-dev")
  ldflags="-X github.com/brokenalarms/ralph/internal/config.Version=${version#v}"
  cd "$root/go" && go build -ldflags "$ldflags" -o "$HOME/.local/bin/ralph" ./cmd/ralph/ 2>/dev/null &
fi
