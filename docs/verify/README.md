# Verification Documents

Each file in this directory is a manual verification guide for a DumboDB
version-control command. Work through the scenarios top to bottom in
`mongosh` to confirm correct behavior.

## Automated Tests

Every verify doc has a matching automated test in `tests/`:

| Document | Test file | Test function |
|----------|-----------|---------------|
| branch.md | versioning_branch_verify_test.go | TestBranchVerify |
| cherry-pick.md | versioning_cherry_pick_verify_test.go | TestCherryPickVerify |
| commit.md | versioning_commit_verify_test.go | TestCommitVerify |
| diff.md | versioning_diff_verify_test.go | TestDiffVerify |
| log.md | versioning_log_verify_test.go | TestLogVerify |
| log-pagination-filtering.md | versioning_log_pagination_verify_test.go | TestLogPaginationVerify |
| merge.md | versioning_merge_verify_test.go | TestMergeVerify |
| rebase.md | versioning_rebase_verify_test.go | TestRebaseVerify |
| reset.md | versioning_reset_verify_test.go | TestResetVerify |
| revert.md | versioning_revert_verify_test.go | TestRevertVerify |
| rootish.md | versioning_rootish_verify_test.go | TestRootishVerify |
| status.md | versioning_status_verify_test.go | TestStatusVerify |
| tag.md | versioning_tag_verify_test.go | TestTagVerify |
| index-branch-isolation.md | index_branch_isolation_verify_test.go | TestIndexBranchIsolationVerify |

Run all verify tests:

```bash
go test ./tests/ -run "Verify" -count=1 -timeout=5m -v
```

## Rules

- The verify doc is the spec. The automated test validates it.
- When you change a verify doc, update the matching test in the same commit.
- When you change behavior, update both the doc and the test.
