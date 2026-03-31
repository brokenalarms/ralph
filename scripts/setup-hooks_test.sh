#!/usr/bin/env bash
# Verify setup-hooks.sh installs both post-merge and post-rewrite hooks.
set -euo pipefail

failures=0
fail() { echo "FAIL: $1"; ((failures++)); }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

git -C "$tmp" init --quiet
mkdir -p "$tmp/scripts"
cp "$(dirname "$0")/setup-hooks.sh" "$tmp/scripts/"
cp "$(dirname "$0")/build-go.sh" "$tmp/scripts/"

(cd "$tmp" && bash scripts/setup-hooks.sh)

# post-merge hook exists and is executable
[[ -x "$tmp/.git/hooks/post-merge" ]] || fail "post-merge hook missing or not executable"

# post-rewrite hook exists and is executable
[[ -x "$tmp/.git/hooks/post-rewrite" ]] || fail "post-rewrite hook missing or not executable"

# Both hooks invoke build-go.sh
grep -q 'build-go.sh' "$tmp/.git/hooks/post-merge" || fail "post-merge doesn't call build-go.sh"
grep -q 'build-go.sh' "$tmp/.git/hooks/post-rewrite" || fail "post-rewrite doesn't call build-go.sh"

# post-rewrite only fires on rebase, not amend
grep -q 'rebase' "$tmp/.git/hooks/post-rewrite" || fail "post-rewrite doesn't check for rebase argument"

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
