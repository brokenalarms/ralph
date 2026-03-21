#!/usr/bin/env bash
# Rebuild Go ralph binary if go/ files changed between $1 and $2 (or HEAD).
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
old="${1:-HEAD~1}"
new="${2:-HEAD}"

if git diff-tree --no-commit-id --name-only -r "$old" "$new" | grep -q '^go/'; then
  cd "$root/go" && go install ./cmd/ralph/ 2>/dev/null &
fi
