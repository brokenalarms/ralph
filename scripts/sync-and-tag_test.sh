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

  cat > "$tmp/repo/package.json" <<'PKG'
{"version": "1.0.0"}
PKG

  (cd "$tmp/repo" && git add package.json && git commit -m "init" --quiet && git push --quiet)
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

# Test: version bumped as a new commit when local commits exist
test_version_bumped_on_push() {
  # sync-and-tag.sh must bump the patch version and commit it as a new commit when pushing
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  (cd "$tmp/repo" && git commit --allow-empty -m "local change" --quiet)
  local_sha=$(git -C "$tmp/repo" rev-parse HEAD)

  bash "$tmp/repo/scripts/sync-and-tag.sh" 2>&1

  version=$(node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('$tmp/repo/package.json','utf8')).version)")
  head_sha=$(git -C "$tmp/repo" rev-parse HEAD)
  parent_sha=$(git -C "$tmp/repo" rev-parse HEAD~1)

  [ "$version" = "1.0.1" ]         || fail "version bump: expected 1.0.1, got $version"
  [ "$head_sha" != "$local_sha" ]  || fail "version bump: HEAD should be new bump commit, not the local change commit"
  [ "$parent_sha" = "$local_sha" ] || fail "version bump: parent of HEAD should be the original local commit"
  git -C "$tmp/repo" log -1 --format="%s" | grep -q "bump version" || fail "version bump: HEAD commit message should mention 'bump version'"
}

# Test: second run with no new local commits does not increment version again
test_no_double_bump_on_second_run() {
  # After pushing once, a second invocation with nothing new must not produce another version bump
  local tmp
  tmp=$(setup_repo)
  trap "rm -rf '$tmp'" RETURN

  (cd "$tmp/repo" && git commit --allow-empty -m "local change" --quiet)

  bash "$tmp/repo/scripts/sync-and-tag.sh" 2>&1
  v1=$(node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('$tmp/repo/package.json','utf8')).version)")

  bash "$tmp/repo/scripts/sync-and-tag.sh" 2>&1
  v2=$(node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('$tmp/repo/package.json','utf8')).version)")

  [ "$v1" = "1.0.1" ]  || fail "double bump: first run should bump to 1.0.1, got $v1"
  [ "$v1" = "$v2" ]    || fail "double bump: second run must not bump again ($v1 → $v2)"
}

# Test: pre-push hook does not amend HEAD
test_prepush_hook_no_amend() {
  # The pre-push hook must not amend HEAD; version bumping moved to sync-and-tag.sh
  grep -q "\-\-amend" "$SCRIPT_DIR/hooks/pre-push" && fail "pre-push hook: must not contain --amend"
  return 0
}

test_tag_no_build_when_uptodate
test_tag_pull_failure_no_build
test_build_runs_via_compose
test_build_skipped_on_sync_failure
test_version_bumped_on_push
test_no_double_bump_on_second_run
test_prepush_hook_no_amend

if ((failures > 0)); then
  echo "$failures test(s) failed"
  exit 1
fi
echo "All tests passed"
