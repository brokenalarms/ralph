# Git usage
- never push to main - atomic commits comprising a feature should be pushed as a PR from the branch you're working on
# Testing
- Tests should be put in place to lock in new features and prevent regressions.
- They should explain in a comment for each why the test is being created, and what user functionality it is proving, so that a test has a specific feature based meaning, and isn't just written to be correct eg assert 1 = true.
