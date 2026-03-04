#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: valid git refs from arbitrary text.
@test "Converts spaces and special chars" {
  result=$(slugify "Fix auth bug!")
  [[ "$result" == "fix-auth-bug" ]]
}

# Proves: branch name limits.
@test "Truncates long names" {
  local long_name="this is a very long task name that exceeds the fifty character limit and should be truncated"
  result=$(slugify "$long_name")
  [[ ${#result} -le 50 ]]
}
