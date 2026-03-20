#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: auto_merge_current_branch returns cleanly when there is no worktree
# branch set (e.g. --no-worktree mode), so --auto-merge is a safe no-op.
@test "auto_merge_current_branch skips when no worktree branch" {
  WORKTREE_BRANCH=""
  WORK_DIR="$PROJECT_DIR"
  run auto_merge_current_branch
  [[ "$status" -eq 0 ]]
}

# Proves: auto_merge_current_branch skips when work dir is the project dir
# (no git worktree isolation), avoiding merging from the project dir itself.
@test "auto_merge_current_branch skips when work dir is project dir" {
  WORKTREE_BRANCH="ralph/project/01-some-task"
  WORK_DIR="$PROJECT_DIR"
  run auto_merge_current_branch
  [[ "$status" -eq 0 ]]
}

# Proves: auto_merge_current_branch fails gracefully when gh CLI is not available,
# so the feature degrades rather than crashing the loop.
@test "auto_merge_current_branch warns without gh CLI" {
  init_ralph_dir
  setup_worktree
  rename_branch_for_task "test task"

  # Hide gh from PATH
  local orig_path="$PATH"
  PATH="/usr/bin:/bin"
  run auto_merge_current_branch
  PATH="$orig_path"

  [[ "$status" -eq 1 ]]
  [[ "$output" == *"gh CLI not found"* ]]
}

# Proves: auto_merge_current_branch returns 0 when no PR exists for the branch,
# logging a skip message instead of failing — the branch just hasn't been pushed yet.
@test "auto_merge_current_branch skips when no PR exists" {
  if ! command -v gh &>/dev/null; then
    skip "gh CLI not available"
  fi

  init_ralph_dir
  setup_worktree
  rename_branch_for_task "unpushed task"

  run auto_merge_current_branch
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"No open PR found"* || "$output" == *"skipping auto-merge"* ]]
}

# Proves: --auto-merge flag is parsed correctly and sets AUTO_MERGE=true.
@test "auto-merge flag is parsed in CLI" {
  # Re-source to get the parse block
  local output
  output=$(bash -c '
    RALPH_SOURCED=true
    source "'"$RALPH_SH"'"
    echo "$AUTO_MERGE"
  ')
  [[ "$output" == "false" ]]

  # Simulate flag parsing by checking variable after sourcing with flag
  output=$(bash -c '
    set -- --auto-merge --no-worktree
    RALPH_SOURCED=true

    # Override main/init_ralph_dir and setup functions to prevent execution
    main() { :; }

    # Source ralph.sh which will parse args via the while loop
    # We need to extract just the flag parsing
    source "'"$RALPH_SH"'"
    echo "$AUTO_MERGE"
  ')
  [[ "$output" == "true" ]]
}
