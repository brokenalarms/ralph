#!/usr/bin/env bash
# Verify rebuild-go.sh exits early when git sync operations fail,
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
  (cd "$tmp/repo" && git commit --allow-empty -m "init" --quiet && git push --quiet)
  git -C "$tmp/repo" tag v0.0.1
  git -C "$tmp/repo" push --tags --quiet

  mkdir -p "$tmp/repo/scripts"
  cp "$SCRIPT_DIR/rebuild-go.sh" "$tmp/repo/scripts/"

  cat > "$tmp/repo/scripts/build-go.sh" <<'STUB'
#!/usr/bin/env bash
echo "BUILD_RAN"
STUB
  chmod +x "$tmp/repo/scripts/build-go.sh"

  echo "$tmp"
}

# Test: pull --rebase failure exits before build
test_pull_failure() {
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  git -C "$tmp/repo" remote set-url origin "file:///nonexistent/path"

  rc=0
  output=$(bash "$tmp/repo/scripts/rebuild-go.sh" 2>&1) || rc=$?

  [ "$rc" -ne 0 ]                            || fail "pull failure: exit code should be non-zero"
  ! echo "$output" | grep -q "BUILD_RAN"     || fail "pull failure: build ran despite failed pull"
  echo "$output" | grep -qi "pull.*fail"      || fail "pull failure: no clear error message"
}

# Test: push failure exits before build
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
  output=$(bash "$tmp/repo/scripts/rebuild-go.sh" 2>&1) || rc=$?

  [ "$rc" -ne 0 ]                            || fail "push failure: exit code should be non-zero"
  ! echo "$output" | grep -q "BUILD_RAN"     || fail "push failure: build ran despite failed push"
  echo "$output" | grep -qi "push.*fail"      || fail "push failure: no clear error message"
}

# Test: successful sync proceeds past sync phase to tag polling
test_success_reaches_polling() {
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  # Run in background — if sync succeeds, it enters the tag polling loop
  bash "$tmp/repo/scripts/rebuild-go.sh" > "$tmp/output" 2>&1 &
  local pid=$!

  # Give it time to pass sync and enter polling
  sleep 2

  # If still running after 2s, sync passed and it's in the polling loop
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null || true
    local output
    output=$(cat "$tmp/output")
    echo "$output" | grep -q "Waiting for version tag" || fail "success: didn't reach tag polling"
  else
    # Process exited — check it wasn't a sync failure
    wait "$pid" 2>/dev/null
    local rc=$?
    local output
    output=$(cat "$tmp/output")
    if [ "$rc" -ne 0 ]; then
      fail "success: script exited with error $rc during sync: $output"
    fi
    # If it exited 0, build-go.sh ran (which is fine too)
  fi
}

test_pull_failure
test_push_failure
test_success_reaches_polling

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
