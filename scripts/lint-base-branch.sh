#!/usr/bin/env bash
#
# Base-branch single-source-of-truth lint (docs/specs/architecture.md §6).
#
# The base branch is resolved ONCE at startup into cfg.BaseBranch and threaded
# into git.New as r.baseBranch. Every consumer reads that single value; nothing
# re-derives the base from git (origin/HEAD), a detectDefaultBranch indirection,
# or a hardcoded literal. Re-derivation is the regression that fed the verifier
# origin/<wrong-base>...HEAD (a multi-hundred-file garbage diff) and stagnated
# completed work. See bead ralph-pobg.
#
# The ONE sanctioned exception is RemoteDefaultBranch() in
# go/internal/git/git_helpers.go — the flag-free helper used only by the
# `ralph merge` subcommand entry point.
set -euo pipefail
cd "$(dirname "$0")/.."

src() { grep -rn --include='*.go' --exclude='*_test.go' "$@"; }
fail=0

# 1. The deleted indirection must not return. Read r.baseBranch / cfg.BaseBranch.
if src -w 'detectDefaultBranch' go/internal go/cmd; then
  echo "ERROR: 'detectDefaultBranch' is forbidden — read the threaded base (r.baseBranch / cfg.BaseBranch)." >&2
  fail=1
fi

# 2. The loop owns cfg.BaseBranch and must never re-derive the base from git.
if src -e 'RemoteDefaultBranch' -e 'symbolic-ref' go/internal/loop; then
  echo "ERROR: internal/loop must not re-derive the base branch — thread cfg.BaseBranch." >&2
  fail=1
fi

# 3. origin/HEAD base re-derivation is allowed ONLY in RemoteDefaultBranch
#    (go/internal/git/git_helpers.go), the single flag-free `ralph merge` entry.
offenders="$(src -l 'refs/remotes/origin/HEAD' go/internal/git | grep -v 'git_helpers.go' || true)"
if [ -n "$offenders" ]; then
  echo "ERROR: origin/HEAD base re-derivation outside RemoteDefaultBranch:" >&2
  echo "$offenders" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "base-branch SOT lint FAILED (docs/specs/architecture.md §6)." >&2
  exit 1
fi
echo "base-branch SOT lint passed."
