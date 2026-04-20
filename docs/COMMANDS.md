# DumboDB Command Reference

DumboDB extends MongoDB's wire protocol with version-control commands that expose [Dolt](https://github.com/dolthub/dolt)'s branching, committing, and diffing capabilities directly through mongosh or any MongoDB driver.

## Connection encoding

All versioning commands target a specific branch by encoding the branch name in the database name:

```
<db>__d_<rootish>
```

| Form | Example | Writable? |
|------|---------|-----------|
| Plain name (defaults to `main`) | `mydb` | Yes |
| Branch name | `mydb__d_feature` | Yes |
| Commit hash (32-char base32) | `mydb__d_na7kfra98...` | No |
| Ancestor expression | `mydb__d_main~2` | No |

Use `db.getSiblingDB("mydb__d_feature")` in mongosh to connect to a branch.

## Available Commands

Every `dolt*` command has an identical `dumbo*` alias for environments that filter unrecognized MongoDB commands:

| Primary | Alias |
|---------|-------|
| `doltCommit` | `dumboCommit` |
| `doltBranch` | `dumboBranch` |
| `doltMerge` | `dumboMerge` |
| `doltCherryPick` | `dumboCherryPick` |
| `doltRebase` | `dumboRebase` |
| `doltLog` | `dumboLog` |
| `doltStatus` | `dumboStatus` |
| `doltDiff` | `dumboDiff` |
| `doltReset` | `dumboReset` |
| `doltRevert` | `dumboRevert` |
| `doltCurrentBranch` | `dumboCurrentBranch` |
| `doltConflicts` | `dumboConflicts` |
| `doltResolveConflict` | `dumboResolveConflict` |

---

## doltCommit

Commits the current working set on the branch encoded in the database name.

**Alias:** `dumboCommit`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `message` | string | no | `""` | Commit message |
| `author` | string | **yes** | — | Commit author, e.g. `"alice <alice@example.com>"` |
| `timestamp` | Date | no | current time | Commit timestamp (BSON Date) |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | Dolt commit hash (32-char base32) |
| `branch` | string | Branch name the commit was made on |
| `message` | string | Commit message (echoed) |
| `author` | string | Author (echoed) |
| `timestamp` | Date | Commit timestamp |
| `ok` | number | `1` on success |

### Example

```js
// Connect to the "orders" database on branch "main"
var db = db.getSiblingDB("orders__d_main")

db.orders.insertOne({ _id: 1, amount: 100, status: "pending" })

db.runCommand({ doltCommit: 1, message: "add order #1", author: "alice <alice@acme.com>" })
// {
//   commitId:  "v9ra3pmi0f6kotj5k3fganpmb3oi9t1k",
//   branch:    "main",
//   message:   "add order #1",
//   author:    "alice <alice@acme.com>",
//   timestamp: ISODate("2026-04-14T20:00:00.000Z"),
//   ok: 1
// }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `author` missing | `OperationFailed: missing required field 'author'` |
| Connection is a read-only rootish (hash or ancestor expression) | writes are rejected before reaching commit |

### Notes

- Committing an unchanged working set succeeds; the new commit hash is still distinct from the previous one.
- `timestamp` accepts a BSON Date; pass `new Date("2024-01-01")` to pin the commit time.

---

## doltBranch

Creates or deletes a branch from the rootish encoded in the database name.

**Alias:** `dumboBranch`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `branch` | string | **yes** | — | Name of the branch to create or delete |
| `delete` | bool/int | no | `false` | Safe-delete: fails if the branch has unmerged commits |
| `forceDelete` | bool/int | no | `false` | Force-delete: succeeds unconditionally |

`delete` and `forceDelete` are mutually exclusive.

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `branch` | string | Branch name (echoed) |
| `ok` | number | `1` on success |

### Example

```js
// Create a "feature" branch from main HEAD
db.getSiblingDB("orders__d_main").runCommand({ doltBranch: 1, branch: "feature" })
// { branch: "feature", ok: 1 }

// Create a branch from an ancestor commit
db.getSiblingDB("orders__d_main~2").runCommand({ doltBranch: 1, branch: "rollback-point" })
// { branch: "rollback-point", ok: 1 }

// Safe-delete a merged branch
db.getSiblingDB("orders__d_main").runCommand({ doltBranch: 1, branch: "feature", delete: 1 })
// { branch: "feature", ok: 1 }

// Force-delete a branch with unmerged commits
db.getSiblingDB("orders__d_main").runCommand({ doltBranch: 1, branch: "abandoned", forceDelete: 1 })
// { branch: "abandoned", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `branch` is empty | `BadValue: doltBranch: branch name must not be empty` |
| `delete` and `forceDelete` both set | `BadValue: doltBranch: delete and forceDelete are mutually exclusive` |
| Safe-delete on branch with unmerged commits | `OperationFailed: … unmerged commits` |

### Notes

- Branch creation works from any rootish: branch name, commit hash, or `branch~N` ancestor expression.
- The new branch HEAD equals the commit resolved from the source rootish.
- Data on the new branch is fully isolated from its source.

---

## doltMerge

Merges a source branch into the branch encoded in the database name.

**Alias:** `dumboMerge`

### Parameters (merge initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `merge_in` | string | **yes** | — | Name of the branch to merge in |
| `message` | string | no | auto | Merge commit message (ignored on fast-forward / already-up-to-date) |
| `author` | string | no | `""` | `"Name <email>"` for the merge commit author |
| `noFF` | bool | no | `false` | Force a merge commit even when fast-forward is possible |
| `ffOnly` | bool | no | `false` | Fail if fast-forward is not possible |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue a paused merge after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort an in-progress merge |

For `continue`, `message` and `author` are optional overrides.

### Response fields (success)

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | Resulting commit hash |
| `message` | string | `"fast-forward"`, `"already up-to-date"`, or custom |
| `ok` | number | `1` on success |

### Response fields (conflicts — ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | Per-collection conflict counts: `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders__d_main")

// Standard merge
main.runCommand({ doltMerge: 1, merge_in: "feature" })
// { commitId: "abc123...", message: "Merge branch 'feature' into 'main'", ok: 1 }

// Fast-forward merge result
// { commitId: "<feature-tip-hash>", message: "fast-forward", ok: 1 }

// Conflict response
// {
//   conflicts: [ { collection: "orders", count: 2 } ],
//   ok: 0,
//   errmsg: "merge conflict in collection 'orders'"
// }

// After resolving conflicts:
main.runCommand({ doltMerge: 1, continue: 1 })
// { commitId: "xyz789...", message: "Merge branch 'feature' into 'main'", ok: 1 }

// Abort a conflicted merge:
main.runCommand({ doltMerge: 1, abort: 1 })
// { message: "merge aborted", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `merge_in` missing or empty | `BadValue: doltMerge: from branch name must not be empty` |
| `noFF` and `ffOnly` both set | `BadValue: doltMerge: noFF and ffOnly are mutually exclusive` |
| Merge produces conflicts | `ok: 0` response with `conflicts` array |

### Notes

- When conflicts occur, the branch HEAD is unchanged; the staged working set reflects partial merge with "ours" values for conflicting documents.
- Use `doltConflicts` to inspect and `doltResolveConflict` to resolve each conflict, then call `doltMerge continue:1`.
- `abort: 1` restores the branch to its pre-merge state.

---

## doltCherryPick

Applies the diff introduced by a named commit onto the current branch, creating a new commit.

**Alias:** `dumboCherryPick`

### Parameters (cherry-pick initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `commit` | string | **yes** | — | Commit hash to cherry-pick |
| `message` | string | no | auto | Custom commit message (default: original message + annotation) |
| `author` | string | no | `""` | `"Name <email>"` for the commit author |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort the in-progress cherry-pick |

For `continue`, `message` and `author` are optional overrides.

### Response fields (success)

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | New commit hash on the target branch |
| `message` | string | Commit message (original + cherry-pick annotation) |
| `ok` | number | `1` |

### Response fields (abort)

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Confirmation message |
| `ok` | number | `1` |

### Response fields (conflicts — ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders__d_main")

// Cherry-pick a commit from another branch onto main
main.runCommand({ doltCherryPick: 1, commit: "na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// {
//   commitId: "new-hash...",
//   message: "add order #1\n\n(cherry picked from commit na7kfra98h45fr2u5qtr30o2ggm7vh61)",
//   ok: 1
// }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `commit` is an unsupported rootish form (HEAD, reflog, range, caret) | `BadValue: doltCherryPick: …` |
| Cherry-pick produces conflicts | `ok: 0` response with `conflicts` array |

---

## doltRebase

Reapplies all commits on the current branch not reachable from `onto` onto the tip of `onto`, rewriting branch history.

**Alias:** `dumboRebase`

### Parameters (rebase initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `onto` | string | **yes** | — | Branch name or rootish to rebase onto |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort and restore original branch state |

### Response fields (success)

| Field | Type | Description |
|-------|------|-------------|
| `commitsReplayed` | int32 | Number of commits replayed |
| `newTip` | string | Commit hash of the new branch tip |
| `ok` | number | `1` |

### Response fields (abort)

| Field | Type | Description |
|-------|------|-------------|
| `newTip` | string | Restored branch tip hash |
| `ok` | number | `1` |

### Response fields (conflicts — ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `conflictCommit` | string | Hash of the commit being replayed when conflict occurred |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
// Rebase feature onto main
db.getSiblingDB("orders__d_feature").runCommand({ doltRebase: 1, onto: "main" })
// { commitsReplayed: 3, newTip: "abc123...", ok: 1 }

// If conflicts arise — resolve them, then continue
db.getSiblingDB("orders__d_feature").runCommand({ doltRebase: 1, continue: 1 })

// Abort
db.getSiblingDB("orders__d_feature").runCommand({ doltRebase: 1, abort: 1 })
// { newTip: "original-tip...", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `onto` is an unsupported rootish form | `BadValue: doltRebase: …` |
| Conflict during replay | `ok: 0` response with `conflicts` and `conflictCommit` |

### Notes

- Rebase rewrites commit history; don't rebase branches that others have already pulled.
- When paused on a conflict, `conflictCommit` identifies which original commit caused it.

---

## doltLog

Returns commit history for the branch encoded in the database name, walking the
full commit graph in reverse topological order. Both parents of merge commits
are visited; ties between commits at the same height are broken by newer
timestamp first.

**Alias:** `dumboLog`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `limit` | int32 | no | unset (default 20) | Maximum number of commits to return. `0` explicitly requests an empty list. |
| `from` | string | no | HEAD | Start traversal from this commit hash instead of HEAD |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `commits` | array | List of commit objects (see below) |
| `ok` | number | `1` |

### Commit object fields

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | Commit hash |
| `refs` | array of strings | Branch/tag decorations (only on branch-tip commits) |
| `parent1` | string | First parent hash (absent on root commit) |
| `parent2` | string | Second parent hash (merge commits only) |
| `message` | string | Commit message |
| `timestamp` | Date | Commit timestamp |
| `author` | string | Commit author |

### Example

```js
db.getSiblingDB("orders__d_main").runCommand({ doltLog: 1, limit: 2 })
// {
//   commits: [
//     {
//       commitId: "v9ra3pmi0f6kotj5k3fganpmb3oi9t1k",
//       refs:     ["HEAD", "main"],
//       parent1:  "tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b",
//       message:  "add order #1",
//       timestamp: ISODate("2026-04-14T20:00:00.000Z"),
//       author:   "alice <alice@acme.com>"
//     },
//     {
//       commitId: "tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b",
//       parent1:  "5vi6e5t3riqpgh6fq0j1pf0r0imuqhsn",
//       message:  "Initialize database",
//       timestamp: ISODate("2026-04-14T08:55:12.000Z"),
//       author:   "DumboDB"
//     }
//   ],
//   ok: 1
// }
```

### Error cases

None specific to this command; missing backend support returns `OperationFailed`.

### Notes

- Every database is initialized with a root `"Initialize database"` commit; the root commit has no `parent1`.
- `refs` appears only on commits that are the HEAD of one or more branches. The connection branch gets both `"HEAD"` and its bare name; other branches get only the bare name.
- Merge commits include `parent2`.
- `from` starts traversal at the given commit; the walk still visits both parents of any merge commit reachable from that start.

---

## doltStatus

Returns uncommitted changes on the branch encoded in the database name.

**Alias:** `dumboStatus`

### Parameters

None (beyond the implicit `$db` connection).

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `branch` | string | Active branch name |
| `collections` | array | Changed collections: `[{name, status}, ...]` |
| `ok` | number | `1` |

### Status values

| Value | Meaning |
|-------|---------|
| `"added"` | Collection exists in working set but not in HEAD |
| `"modified"` | Collection exists in both but content differs |
| `"deleted"` | Collection exists in HEAD but not in working set |

### Example

```js
var db = db.getSiblingDB("orders__d_main")

db.orders.insertOne({ _id: 99, amount: 500 })

db.runCommand({ doltStatus: 1 })
// {
//   branch: "main",
//   collections: [
//     { name: "orders", status: "modified" }
//   ],
//   ok: 1
// }

// After committing — clean state
db.runCommand({ doltCommit: 1, message: "add order 99", author: "alice <alice@acme.com>" })
db.runCommand({ doltStatus: 1 })
// { branch: "main", collections: [], ok: 1 }
```

### Notes

- Only collections with uncommitted changes appear; unchanged collections are omitted.
- `collections` is always an array (empty when clean).

---

## doltDiff

Returns a document-level diff between two states for the branch encoded in the database name.

**Alias:** `dumboDiff`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `from` | string | no | HEAD | Starting rootish (commit hash, branch name, ancestor expression, or `"HEAD"`) |
| `to` | string | no | working set | Ending rootish (same forms; omit to compare to current working set) |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `collections` | array | Changed collections (see below) |
| `ok` | number | `1` |

### Collection diff object

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Collection name |
| `added` | array | Full documents added in `to` |
| `removed` | array | Full documents present in `from` but absent in `to` |
| `modified` | array | Modified document diff entries (see below) |

### Modified document entry

| Field | Type | Description |
|-------|------|-------------|
| `_id` | any | Document `_id` |
| `diff` | array | Per-field diffs: `{type, path, from?, to?}` |

### Field diff entry

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | `"added"`, `"removed"`, or `"modified"` |
| `path` | string | JSONPath to the field, e.g. `"$.score"` or `"$.address.city"` |
| `from` | any | Old value (absent for `"added"`) |
| `to` | any | New value (absent for `"removed"`) |

### Example

```js
var db = db.getSiblingDB("orders__d_main")

// Insert baseline and commit
db.orders.insertOne({ _id: 1, amount: 100 })
const r = db.runCommand({ doltCommit: 1, message: "baseline", author: "alice <alice@acme.com>" })
const hashBase = r.commitId

// Modify the working set
db.orders.updateOne({ _id: 1 }, { $set: { amount: 150 } })

// Diff HEAD vs working set
db.runCommand({ doltDiff: 1 })
// {
//   collections: [
//     {
//       name:    "orders",
//       added:   [],
//       removed: [],
//       modified: [
//         { _id: 1, diff: [ { type: "modified", path: "$.amount", from: 100, to: 150 } ] }
//       ]
//     }
//   ],
//   ok: 1
// }

// Diff between two commits
db.runCommand({ doltDiff: 1, from: hashBase, to: "HEAD" })
```

### Error cases

| Condition | Error |
|-----------|-------|
| Unsupported rootish form in `from` or `to` | `OperationFailed: rootish …` |

### Notes

- Omit both `from` and `to` to get working-set changes vs HEAD.
- Only collections with at least one change appear in `collections`.
- Unchanged fields do not appear in `modified[].diff`.
- `HEAD` resolves to the connection's own branch tip (not necessarily `main`).
- Reversing `from` and `to` inverts the diff: `added` and `removed` swap roles.

---

## doltReset

Moves the branch HEAD to the specified commit. Supports soft (default) and hard modes.

**Alias:** `dumboReset`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `to` | string | no | HEAD | Target commit hash |
| `hard` | bool | no | `false` | Hard reset: also resets the working tree to the target commit |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | Hash of the commit HEAD now points to |
| `ok` | number | `1` |

### Modes

| Mode | HEAD | Working set |
|------|------|-------------|
| Soft (default) | Moves to target | Preserved (uncommitted changes survive) |
| Hard (`hard: true`) | Moves to target | Reset to target (uncommitted changes discarded) |

### Example

```js
var db = db.getSiblingDB("orders__d_main")

// Soft reset to one commit back — uncommitted changes preserved
const log = db.runCommand({ doltLog: 1, limit: 2 })
const previousHash = log.commits[1].commitId

db.runCommand({ doltReset: 1, to: previousHash })
// { commitId: "<previousHash>", ok: 1 }

// Hard reset to HEAD — discard all uncommitted changes
db.runCommand({ doltReset: 1, hard: true })
// { commitId: "<current-HEAD-hash>", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `to` is not a valid commit hash | `OperationFailed: …` |

### Notes

- Soft reset with no `to` is a no-op for HEAD but still returns `ok: 1`.
- Hard reset is irreversible: uncommitted changes in the working set are permanently discarded.

---

## doltRevert

Applies the inverse diff of a named commit onto the current branch, creating a new commit that undoes those changes.

**Alias:** `dumboRevert`

### Parameters (revert initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `commit` | string | **yes** | — | Commit hash to revert |
| `message` | string | no | auto | Custom commit message |
| `author` | string | no | `""` | `"Name <email>"` for the commit author |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort the in-progress revert |

For `continue`, `message` and `author` are optional overrides.

### Response fields (success)

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | New revert commit hash |
| `message` | string | Auto-generated revert message |
| `ok` | number | `1` |

### Response fields (abort)

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Confirmation |
| `ok` | number | `1` |

### Response fields (conflicts — ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders__d_main")

// View history to find the commit to revert
const log = main.runCommand({ doltLog: 1, limit: 3 })
const badCommitHash = log.commits[0].commitId

// Revert it
main.runCommand({ doltRevert: 1, commit: badCommitHash })
// {
//   commitId: "new-revert-hash...",
//   message:  "Revert \"add order #1\"\n\nThis reverts commit <badCommitHash>.",
//   ok: 1
// }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `commit` is an unsupported rootish form | `BadValue: doltRevert: …` |
| Revert produces conflicts | `ok: 0` response with `conflicts` array |

### Notes

- The revert creates a new commit; it does not alter history.
- Unlike `doltReset`, reverting is safe to use on shared branches.
- The auto-generated message includes the original commit message and hash.

---

## doltCurrentBranch

Returns the branch name for the connection encoded in the database name.

**Alias:** `dumboCurrentBranch`

### Parameters

None (beyond the implicit `$db` connection).

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `branch` | string | Current branch name |
| `ok` | number | `1` |

### Example

```js
db.getSiblingDB("orders").runCommand({ doltCurrentBranch: 1 })
// { branch: "main", ok: 1 }

db.getSiblingDB("orders__d_feature").runCommand({ doltCurrentBranch: 1 })
// { branch: "feature", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| Connection is a read-only rootish (commit hash or ancestor expression) | `OperationFailed: doltCurrentBranch: no current branch name (connection is at a specific commit, not a named branch)` |

### Notes

- Read-only rootishes (commit hashes, `branch~N`) have no "current branch" and return an error.
- Use this to confirm which branch a connection is targeting, particularly in scripts that build the encoded DB name dynamically.

---

## doltConflicts

Returns conflict information for an in-progress merge, cherry-pick, or rebase on the branch encoded in the database name.

**Alias:** `dumboConflicts`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `collection` | string | no | `""` | If specified, return per-document conflict details for this collection |

### Response fields (summary mode — no `collection`)

| Field | Type | Description |
|-------|------|-------------|
| `collections` | array | `[{name, count}, ...]` — per-collection conflict counts |
| `ok` | number | `1` |

### Response fields (detail mode — with `collection`)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | Per-document conflict entries (see below) |
| `ok` | number | `1` |

### Conflict entry fields

| Field | Type | Description |
|-------|------|-------------|
| `conflictId` | string | Unique identifier for this conflict (used with `doltResolveConflict`) |
| `base` | document or null | The common ancestor document |
| `ours` | document or null | Our version of the document |
| `theirs` | document or null | Their version of the document |
| `ourDiffType` | string | How we changed it: `"added"`, `"modified"`, `"deleted"` |
| `theirDiffType` | string | How they changed it |

### Example

```js
var main = db.getSiblingDB("orders__d_main")

// After a merge conflict:
main.runCommand({ doltConflicts: 1 })
// { collections: [ { name: "orders", count: 2 } ], ok: 1 }

// Inspect individual conflicts:
main.runCommand({ doltConflicts: 1, collection: "orders" })
// {
//   conflicts: [
//     {
//       conflictId:    "c0",
//       base:          { _id: 1, amount: 100 },
//       ours:          { _id: 1, amount: 150 },
//       theirs:        { _id: 1, amount: 200 },
//       ourDiffType:   "modified",
//       theirDiffType: "modified"
//     }
//   ],
//   ok: 1
// }
```

---

## doltResolveConflict

Resolves a single document conflict in the current in-progress merge, cherry-pick, or rebase.

**Alias:** `dumboResolveConflict`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `collection` | string | **yes** | — | Collection containing the conflict |
| `conflictId` | string | **yes** | — | Conflict identifier from `doltConflicts` |
| `resolution` | string | **yes** | — | `"ours"`, `"theirs"`, or `"custom"` |
| `value` | document | conditional | — | Required when `resolution` is `"custom"`; the document to use |

### Resolution options

| Value | Behavior |
|-------|----------|
| `"ours"` | Keep the local (into-branch) version |
| `"theirs"` | Use the incoming (from-branch) version |
| `"custom"` | Use the document provided in `value` |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `ok` | number | `1` on success |

### Example

```js
var main = db.getSiblingDB("orders__d_main")

// Resolve using our version
main.runCommand({
  doltResolveConflict: 1,
  collection: "orders",
  conflictId: "c0",
  resolution: "ours"
})
// { ok: 1 }

// Resolve using their version
main.runCommand({
  doltResolveConflict: 1,
  collection: "orders",
  conflictId: "c0",
  resolution: "theirs"
})
// { ok: 1 }

// Resolve with a custom document
main.runCommand({
  doltResolveConflict: 1,
  collection: "orders",
  conflictId: "c0",
  resolution: "custom",
  value: { _id: 1, amount: 175, status: "reconciled" }
})
// { ok: 1 }

// After resolving all conflicts, complete the merge:
main.runCommand({ doltMerge: 1, continue: 1 })
```

### Error cases

| Condition | Error |
|-----------|-------|
| `resolution` is `"custom"` but `value` is missing | `BadValue: doltResolveConflict: resolution 'custom' requires a 'value' document` |
| Unknown `conflictId` | `OperationFailed: …` |

### Notes

- Resolve all conflicts in a collection before moving to the next.
- After all conflicts across all collections are resolved, call the appropriate `continue` command (`doltMerge`, `doltCherryPick`, or `doltRebase`).

---

## Conflict resolution workflow

The same three-step pattern applies to `doltMerge`, `doltCherryPick`, and `doltRebase` when they produce conflicts:

```js
var main = db.getSiblingDB("orders__d_main")

// Step 1: Operation returns ok: 0 with conflict summary
main.runCommand({ doltMerge: 1, merge_in: "feature" })
// { conflicts: [ { collection: "orders", count: 1 } ], ok: 0, errmsg: "..." }

// Step 2: Inspect and resolve each conflict
const detail = main.runCommand({ doltConflicts: 1, collection: "orders" })
detail.conflicts.forEach(c => {
  main.runCommand({
    doltResolveConflict: 1,
    collection: "orders",
    conflictId: c.conflictId,
    resolution: "ours"
  })
})

// Step 3: Continue to create the final commit
main.runCommand({ doltMerge: 1, continue: 1 })
// { commitId: "...", message: "Merge branch 'feature' into 'main'", ok: 1 }
```
