#!/usr/bin/env bats

load test_helper

# Proves: early failure on missing plan file.
@test "--plan-file with nonexistent file exits with error" {
  run bash -c "$(printf '%q' "$RALPH_SH") --plan-file /nonexistent/plan.md -d /tmp 2>&1" </dev/null
  [[ "$status" -ne 0 ]]
  [[ "$output" == *"not found"* ]]
}

# Proves: valid plans accepted.
@test "--plan-file with existing file proceeds" {
  setup_test_repo
  local plan="$TEST_TMPDIR/plan.md"
  echo "- [ ] Test task" > "$plan"
  # Just check it gets past validation (will fail later without claude, but
  # should not fail with "not found")
  run bash -c "$(printf '%q' "$RALPH_SH") --plan-file $(printf '%q' "$plan") -d $(printf '%q' "$PROJECT_DIR") --plan 2>&1" </dev/null
  [[ "$output" != *"not found"* ]]
  teardown_test_repo
}
