#!/usr/bin/env bash
# Install git hooks for the ralph repository.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
hooks_dir="$root/.git/hooks"

cat > "$hooks_dir/post-merge" <<'HOOK'
#!/usr/bin/env bash
root="$(git rev-parse --show-toplevel)"
"$root/scripts/build-go.sh"
HOOK

chmod +x "$hooks_dir/post-merge"

cat > "$hooks_dir/post-rewrite" <<'HOOK'
#!/usr/bin/env bash
if [ "$1" = "rebase" ]; then
  root="$(git rev-parse --show-toplevel)"
  "$root/scripts/build-go.sh"
fi
HOOK

chmod +x "$hooks_dir/post-rewrite"

# pre-push: bump patch version in package.json and amend into HEAD
ln -sf "$root/scripts/hooks/pre-push" "$hooks_dir/pre-push"

echo "Git hooks installed (post-merge, post-rewrite → build; pre-push → version bump)."
