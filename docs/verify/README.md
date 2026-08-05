# Verification Documents

Each file in this directory is a manual verification guide for a DumboDB
version-control command. Work through the scenarios top to bottom in
`mongosh` to confirm correct behavior.

## Automated Tests

Every verify doc has a matching automated test in `tests/verify/` (package
`verify`). The shared harness and response decoders live in `tests/support`.

| Document | Test file (in `tests/verify/`) | Test function |
|----------|--------------------------------|---------------|
| branch.md | branch_test.go | TestBranchVerify |
| branch-status.md | branch_status_test.go | TestBranchStatusVerify |
| cherry-pick.md | cherry_pick_test.go | TestCherryPickVerify |
| commit.md | commit_test.go | TestCommitVerify |
| diff.md | diff_test.go | TestDiffVerify |
| log.md | log_test.go | TestLogVerify |
| log-pagination-filtering.md | log_pagination_filtering_test.go | TestLogPaginationVerify |
| merge.md | merge_test.go | TestMergeVerify |
| rebase.md | rebase_test.go | TestRebaseVerify |
| reset.md | reset_test.go | TestResetVerify |
| revert.md | revert_test.go | TestRevertVerify |
| rootish.md | rootish_test.go | TestRootishVerify |
| status.md | status_test.go | TestStatusVerify |
| tag.md | tag_test.go | TestTagVerify |
| undrop.md | undrop_test.go | TestUndropVerify |
| auth-rbac.md | auth_rbac_test.go | TestAuthRBACVerify |
| index-branch-isolation.md | index_branch_isolation_test.go | TestIndexBranchIsolationVerify |
| index-maintenance.md | index_maintenance_test.go | TestIndexMaintenanceVerify |
| index-merge.md | index_merge_test.go | TestIndexMergeVerify |
| view-merge.md | view_merge_test.go | TestViewMergeVerify |

Run all verify tests:

```bash
go test ./tests/verify/ -count=1 -timeout=10m -v
```

## Rules

- The verify doc is the spec. The automated test validates it.
- When you change a verify doc, update the matching test in the same commit.
- When you change behavior, update both the doc and the test.
