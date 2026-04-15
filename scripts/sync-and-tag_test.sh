#!/usr/bin/env bash
# Verify sync-and-tag.sh syncs without invoking build-go.sh,
# and that sync-and-build.sh still produces both sync and build.
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
  cp "$SCRIPT_DIR/sync-and-tag.sh" "$tmp/repo/scripts/"
  cp "$SCRIPT_DIR/sync-and-build.sh" "$tmp/repo/scripts/"

  cat > "$tmp/repo/scripts/build-go.sh" <<'STUB'
#!/usr/bin/env bash
echo "BUILD_RAN"
STUB
  chmod +x "$tmp/repo/scripts/build-go.sh"

  echo "$tmp"
}

# Test: sync-and-tag.sh does NOT invoke build-go.sh when up to date
test_tag_no_build_when_uptodate() {
  # sync-and-tag.sh should sync and exit without running build-go.sh
  # when there are no local commits to push.
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  output=$(bash "$tmp/repo/scripts/sync-and-tag.sh" 2>&1)
  rc=$?

  [ "$rc" -eq 0 ]                         || fail "tag no-build: exited with error $rc: $output"
  ! echo "$output" | grep -q "BUILD_RAN"  || fail "tag no-build: build ran when it should not have"
}

# Test: sync-and-tag.sh exits with error on pull failure without building
test_tag_pull_failure_no_build() {
  # When the remote is unreachable, sync-and-tag.sh should fail fast without building.
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  git -C "$tmp/repo" remote set-url origin "file:///nonexistent/path"

  rc=0
  output=$(bash "$tmp/repo/scripts/sync-and-tag.sh" 2>&1) || rc=$?

  [ "$rc" -ne 0 ]                         || fail "tag pull failure: exit code should be non-zero"
  ! echo "$output" | grep -q "BUILD_RAN"  || fail "tag pull failure: build ran despite pull failure"
  echo "$output" | grep -qi "pull.*fail"  || fail "tag pull failure: no clear error message"
}

# Test: sync-and-build.sh invokes build-go.sh (composed behavior preserved)
test_build_runs_via_compose() {
  # sync-and-build.sh must still invoke build-go.sh after sync — composed behavior.
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  # Create a remote commit so pull has something to fetch
  (cd "$tmp/repo" && git commit --allow-empty -m "remote change" --quiet && git push --quiet)
  git -C "$tmp/repo" reset --hard HEAD~1 --quiet

  output=$(bash "$tmp/repo/scripts/sync-and-build.sh" 2>&1)
  rc=$?

  [ "$rc" -eq 0 ]                        || fail "compose: script exited with error $rc: $output"
  echo "$output" | grep -q "BUILD_RAN"   || fail "compose: build-go.sh was not invoked"
}

# Test: sync-and-build.sh fails fast if sync-and-tag.sh fails (push blocked)
test_build_skipped_on_sync_failure() {
  # sync-and-build.sh should not run build-go.sh when the sync step fails.
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

  [ "$rc" -ne 0 ]                          || fail "compose fail: exit code should be non-zero"
  ! echo "$output" | grep -q "BUILD_RAN"   || fail "compose fail: build ran despite sync failure"
}

test_tag_no_build_when_uptodate
test_tag_pull_failure_no_build
test_build_runs_via_compose
test_build_skipped_on_sync_failure

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
