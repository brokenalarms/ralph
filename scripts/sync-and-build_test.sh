#!/usr/bin/env bash
# Verify sync-and-build.sh exits early when git sync operations fail,
# and proceeds past sync when operations succeed.
set -euo pipefail

failures=0
fail() { echo "FAIL: $1"; ((failures++)); }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

setup_repo() {
  local tmp
  tmp=$(mktemp -d)

  git init --bare --quiet "$tmp/remote.git"
  git clone --quiet "$tmp/remote.git" "$tmp/repo"

  cat > "$tmp/repo/package.json" <<'PKG'
{"version": "1.0.0"}
PKG

  (cd "$tmp/repo" && git add package.json && git commit -m "init" --quiet && git push --quiet)
  git -C "$tmp/repo" tag v0.0.1
  git -C "$tmp/repo" push --tags --quiet

  mkdir -p "$tmp/repo/scripts"
  cp "$SCRIPT_DIR/sync-and-build.sh" "$tmp/repo/scripts/"
  cp "$SCRIPT_DIR/sync-and-tag.sh" "$tmp/repo/scripts/"

  cat > "$tmp/repo/scripts/build-go.sh" <<'STUB'
#!/usr/bin/env bash
echo "BUILD_RAN"
STUB
  chmod +x "$tmp/repo/scripts/build-go.sh"

  # Install post-merge hook (mirrors real setup)
  mkdir -p "$tmp/repo/.git/hooks"
  cat > "$tmp/repo/.git/hooks/post-merge" <<'HOOK'
#!/usr/bin/env bash
root="$(git rev-parse --show-toplevel)"
"$root/scripts/build-go.sh"
HOOK
  chmod +x "$tmp/repo/.git/hooks/post-merge"

  echo "$tmp"
}

# Test: push failure exits before pull
test_push_failure() {
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  (cd "$tmp/repo" && git commit --allow-empty -m "local change" --quiet)

  cat > "$tmp/remote.git/hooks/pre-receive" <<'HOOK'
#!/usr/bin/env bash
exit 1
HOOK
  chmod +x "$tmp/remote.git/hooks/pre-receive"

  rc=0
  output=$(bash "$tmp/repo/scripts/sync-and-build.sh" 2>&1) || rc=$?

  [ "$rc" -ne 0 ]                            || fail "push failure: exit code should be non-zero"
  ! echo "$output" | grep -q "BUILD_RAN"     || fail "push failure: build ran despite failed push"
  echo "$output" | grep -qi "push.*fail"      || fail "push failure: no clear error message"
}

# Test: pull failure exits with error
test_pull_failure() {
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  git -C "$tmp/repo" remote set-url origin "file:///nonexistent/path"

  rc=0
  output=$(bash "$tmp/repo/scripts/sync-and-build.sh" 2>&1) || rc=$?

  [ "$rc" -ne 0 ]                            || fail "pull failure: exit code should be non-zero"
  echo "$output" | grep -qi "pull.*fail"      || fail "pull failure: no clear error message"
}

# Test: successful sync triggers build via post-merge hook
test_success_builds() {
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  # Create a remote commit so pull has something to merge
  (cd "$tmp/repo" && git commit --allow-empty -m "remote change" --quiet && git push --quiet)
  git -C "$tmp/repo" reset --hard HEAD~1 --quiet

  output=$(bash "$tmp/repo/scripts/sync-and-build.sh" 2>&1)
  rc=$?

  [ "$rc" -eq 0 ]                            || fail "success: script exited with error $rc: $output"
  echo "$output" | grep -q "BUILD_RAN"       || fail "success: post-merge hook didn't trigger build"
}

test_push_failure
test_pull_failure
test_success_builds

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
