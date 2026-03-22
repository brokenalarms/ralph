#!/usr/bin/env bash
# Install git hooks for the ralph repository.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
hooks_dir="$root/.git/hooks"

cat > "$hooks_dir/post-merge" <<'HOOK'
#!/usr/bin/env bash
"$(git rev-parse --show-toplevel)/scripts/rebuild-go.sh"
HOOK

cat > "$hooks_dir/pre-commit" <<'HOOK'
#!/usr/bin/env bash
# If any prompts/ files are staged, sync them to go/cmd/ralph/prompts/
root="$(git rev-parse --show-toplevel)"
if git diff --cached --name-only | grep -q '^prompts/'; then
  cp -r "$root/prompts/"* "$root/go/cmd/ralph/prompts/" 2>/dev/null || true
  git add "$root/go/cmd/ralph/prompts/" 2>/dev/null || true
fi
HOOK

chmod +x "$hooks_dir/post-merge" "$hooks_dir/pre-commit"

echo "Git hooks installed (post-merge + pre-commit prompt sync)."
