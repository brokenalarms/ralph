#!/usr/bin/env bash
# Pull latest and push local commits. Bumps patch version as a new commit
# before pushing so the bump is included in the resolved push refs.
# Tagging happens in CI from package.json.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
package_json="$root/package.json"

echo "[sync] Pulling latest..."
if ! git -C "$root" pull --rebase 2>&1; then
  echo "[sync] Pull --rebase failed" >&2
  exit 1
fi

ahead=$(git -C "$root" rev-list --count @{u}..HEAD 2>/dev/null || echo "0")
if [ "$ahead" != "0" ]; then
  current=$(node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('$package_json', 'utf8')).version)")
  IFS='.' read -r major minor patch <<< "$current"
  next="${major}.${minor}.$((patch + 1))"

  node -e "
    const fs = require('fs');
    const pkg = JSON.parse(fs.readFileSync('$package_json', 'utf8'));
    pkg.version = '$next';
    fs.writeFileSync('$package_json', JSON.stringify(pkg, null, 2) + '\n');
  "

  git -C "$root" add package.json
  git -C "$root" commit -m "chore: bump version to $next"
  echo "[sync] $current -> $next"

  echo "[sync] Pushing $((ahead + 1)) commit(s)..."
  if ! git -C "$root" push 2>&1; then
    echo "[sync] Push failed" >&2
    exit 1
  fi
fi
