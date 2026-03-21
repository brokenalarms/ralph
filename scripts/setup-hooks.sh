#!/usr/bin/env bash
# Install git hooks for the ralph repository.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
hooks_dir="$root/.git/hooks"

cat > "$hooks_dir/post-commit" <<'HOOK'
#!/usr/bin/env bash
"$(git rev-parse --show-toplevel)/scripts/rebuild-go.sh" HEAD~1 HEAD
HOOK

cat > "$hooks_dir/post-merge" <<'HOOK'
#!/usr/bin/env bash
"$(git rev-parse --show-toplevel)/scripts/rebuild-go.sh" HEAD@{1} HEAD
HOOK

chmod +x "$hooks_dir/post-commit" "$hooks_dir/post-merge"

echo "Git hooks installed."
