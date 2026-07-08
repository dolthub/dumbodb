# DumboDB Command Reference

DumboDB extends MongoDB's wire protocol with version-control commands that expose [Dolt](https://github.com/dolthub/dolt)'s branching, committing, and diffing capabilities directly through mongosh or any MongoDB driver.

## Connection encoding

All versioning commands target a specific branch by encoding the branch name in the database name:

```
<db>@<rootish>
```

| Form | Example | Writable? |
|------|---------|-----------|
| Plain name (defaults to `main`) | `mydb` | Yes |
| Branch name | `mydb@feature` | Yes |
| Commit hash (32-char base32) | `mydb@na7kfra98...` | No |
| Ancestor expression | `mydb@main~2` | No |

Use `db.getSiblingDB("mydb@feature")` in mongosh to connect to a branch.

## Available Commands

Every `dumbo*` command has an identical `dolt*` alias:

| Primary | Alias |
|---------|-------|
| `dumboCommit` | `doltCommit` |
| `dumboBranch` | `doltBranch` |
| `dumboBranchStatus` | `doltBranchStatus` |
| `dumboMerge` | `doltMerge` |
| `dumboCherryPick` | `doltCherryPick` |
| `dumboRebase` | `doltRebase` |
| `dumboLog` | `doltLog` |
| `dumboStatus` | `doltStatus` |
| `dumboDiff` | `doltDiff` |
| `dumboReset` | `doltReset` |
| `dumboRevert` | `doltRevert` |
| `dumboConflicts` | `doltConflicts` |
| `dumboResolveConflict` | `doltResolveConflict` |
| `dumboTag` | `doltTag` |
| `dumboGC` | `doltGC` |
| `dumboUndrop` | `doltUndrop` |

---

## dumboCommit

Commits the current working set on the branch encoded in the database name.

**Alias:** `doltCommit`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `message` | string | no | `""` | Commit message |
| `author` | string | **yes** |  -- | Commit author, e.g. `"alice <alice@example.com>"` |
| `timestamp` | Date | no | current time | Commit timestamp (BSON Date) |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | Dolt commit hash (32-char base32) |
| `branch` | string | Branch name the commit was made on |
| `message` | string | Commit message (echoed) |
| `author` | string | Author (echoed) |
| `timestamp` | Date | Commit timestamp |
| `committer` | string | Committer identity (`"Name <email>"`). Equals `author` for regular commits; differs for cherry-pick and rebase. |
| `committerTimestamp` | Date | Timestamp of when the commit was applied |
| `ok` | number | `1` on success |

### Example

```js
// Connect to the "orders" database on branch "main"
var db = db.getSiblingDB("orders@main")

db.orders.insertOne({ _id: 1, amount: 100, status: "pending" })

db.runCommand({ dumboCommit: 1, message: "add order #1", author: "alice <alice@acme.com>" })
// {
//   commitId:           "v9ra3pmi0f6kotj5k3fganpmb3oi9t1k",
//   branch:             "main",
//   message:            "add order #1",
//   author:             "alice <alice@acme.com>",
//   timestamp:          ISODate("2026-04-14T20:00:00.000Z"),
//   committer:          "alice <alice@acme.com>",
//   committerTimestamp: ISODate("2026-04-14T20:00:00.000Z"),
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

## dumboBranch

Creates or deletes a branch from the rootish encoded in the database name.

**Alias:** `doltBranch`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `branch` | string | **yes** |  -- | Name of the branch to create or delete |
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
db.getSiblingDB("orders@main").runCommand({ dumboBranch: 1, branch: "feature" })
// { branch: "feature", ok: 1 }

// Create a branch from an ancestor commit
db.getSiblingDB("orders@main~2").runCommand({ dumboBranch: 1, branch: "rollback-point" })
// { branch: "rollback-point", ok: 1 }

// Safe-delete a merged branch
db.getSiblingDB("orders@main").runCommand({ dumboBranch: 1, branch: "feature", delete: 1 })
// { branch: "feature", ok: 1 }

// Force-delete a branch with unmerged commits
db.getSiblingDB("orders@main").runCommand({ dumboBranch: 1, branch: "abandoned", forceDelete: 1 })
// { branch: "abandoned", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `branch` is empty | `BadValue: dumboBranch: branch name must not be empty` |
| `delete` and `forceDelete` both set | `BadValue: dumboBranch: delete and forceDelete are mutually exclusive` |
| Safe-delete on branch with unmerged commits | `OperationFailed: ... unmerged commits` |

### Notes

- Branch creation works from any rootish: branch name, commit hash, or `branch~N` ancestor expression.
- The new branch HEAD equals the commit resolved from the source rootish.
- Data on the new branch is fully isolated from its source.

---

## dumboBranchStatus

Reports how many commits each target refspec is ahead and behind a base refspec.
"Ahead" counts commits reachable from the target but not the base; "behind" counts
the reverse. Read-only.

**Alias:** `doltBranchStatus`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `base` | string | **yes** |  -- | Base refspec to compare each target against |
| `targets` | array of strings, or string | **yes** |  -- | Target refspecs; a single string is treated as a one-element array. Must name at least one target |

A refspec is a commit hash, branch name, tag name, ancestor expression (`main~2`,
`b2^1`), `HEAD`, or `HEAD~N`. `HEAD`/`HEAD~N` resolve against the branch encoded in
the database name.

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `base` | object | `{ target, hash }` for the base refspec |
| `targets` | array | One entry per target, in request order |
| `ok` | number | `1` on success |

### Target entry

| Field | Type | Description |
|-------|------|-------------|
| `target` | string | The target refspec, echoed verbatim (`HEAD~1` stays `HEAD~1`) |
| `hash` | string | Resolved commit hash (32-char base32) |
| `commitsAhead` | int32 | Commits reachable from the target but not the base |
| `commitsBehind` | int32 | Commits reachable from the base but not the target |

The `base` object has the same `target` (verbatim) and `hash` fields.

### Example

```js
db.getSiblingDB("orders@main").runCommand({
  dumboBranchStatus: 1,
  base: "main",
  targets: ["feature", "HEAD~1"]
})
// {
//   base: { target: "main", hash: "<hash>" },
//   targets: [
//     { target: "feature", hash: "<hash>", commitsAhead: 2, commitsBehind: 1 },
//     { target: "HEAD~1",  hash: "<hash>", commitsAhead: 0, commitsBehind: 1 }
//   ],
//   ok: 1
// }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `base` missing | `BadValue: required parameter "base" is missing` |
| `targets` missing | `Location40414: BSON field 'dumboBranchStatus.targets' is missing but a required field` |
| `targets` empty (`[]`) | `BadValue: dumboBranchStatus: at least one target is required` |
| Empty-string target | `BadValue: dumboBranchStatus: rootish must not be empty` |
| Target/base cannot be resolved | `OperationFailed: ... resolving target ...` |

### Notes

- Comparing a refspec to itself yields `commitsAhead: 0, commitsBehind: 0`.
- Tags resolve to their target commit, so they report the same counts as the commit they point at.
- A merge commit is ahead by every commit reachable from it but not the base -- the merged-in branch commits plus the merge commit itself, not just the merge commit.
- The order of `targets` in the response matches the request order.

---

## dumboMerge

Merges a source branch into the branch encoded in the database name.

**Alias:** `doltMerge`

### Parameters (merge initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `merge_in` | string | **yes** |  -- | Name of the branch to merge in |
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
| `committer` | string | Committer identity (`"Name <email>"`). Equals `author` for merge commits. |
| `committerTimestamp` | Date | Timestamp of when the merge commit was created |
| `ok` | number | `1` on success |

### Response fields (conflicts  -- ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | Per-collection conflict counts: `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders@main")

// Standard merge
main.runCommand({ dumboMerge: 1, merge_in: "feature" })
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
main.runCommand({ dumboMerge: 1, continue: 1 })
// { commitId: "xyz789...", message: "Merge branch 'feature' into 'main'", ok: 1 }

// Abort a conflicted merge:
main.runCommand({ dumboMerge: 1, abort: 1 })
// { message: "merge aborted", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `merge_in` missing or empty | `BadValue: dumboMerge: from branch name must not be empty` |
| `noFF` and `ffOnly` both set | `BadValue: dumboMerge: noFF and ffOnly are mutually exclusive` |
| Merge produces conflicts | `ok: 0` response with `conflicts` array |

### Notes

- When conflicts occur, the branch HEAD is unchanged; the staged working set reflects partial merge with "ours" values for conflicting documents.
- Use `dumboConflicts` to inspect and `dumboResolveConflict` to resolve each conflict, then call `dumboMerge continue:1`.
- `abort: 1` restores the branch to its pre-merge state.

---

## dumboCherryPick

Applies the diff introduced by a named commit onto the current branch, creating a new commit.

**Alias:** `doltCherryPick`

### Parameters (cherry-pick initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `commit` | string | **yes** |  -- | Commit hash to cherry-pick |
| `message` | string | no | auto | Custom commit message (default: original message + annotation) |
| `committer` | string | no |  -- | `"Name <email>"` committer identity. When omitted, committer equals the original commit's author. |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort the in-progress cherry-pick |
| `committer` | string | no |  -- | Committer identity override (same as initiation) |

For `continue`, `message` and `committer` are optional overrides.

### Response fields (success)

| Field | Type | Description |
|-------|------|-------------|
| `commitId` | string | New commit hash on the target branch |
| `message` | string | Commit message (original + cherry-pick annotation) |
| `committer` | string | Committer identity (`"Name <email>"`). This is the person performing the cherry-pick; `author` is preserved from the original commit. |
| `committerTimestamp` | Date | Timestamp of when the cherry-pick was applied |
| `ok` | number | `1` |

### Response fields (abort)

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Confirmation message |
| `ok` | number | `1` |

### Response fields (conflicts  -- ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders@main")

// Cherry-pick a commit from another branch onto main
main.runCommand({ dumboCherryPick: 1, commit: "na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// {
//   commitId:           "new-hash...",
//   message:            "add order #1\n\n(cherry picked from commit na7kfra98h45fr2u5qtr30o2ggm7vh61)",
//   committer:          "alice <alice@acme.com>",
//   committerTimestamp: ISODate("2026-04-14T20:00:00.000Z"),
//   ok: 1
// }
```

> **Note:** The `author` of a cherry-picked commit is preserved from the original commit.
> The `committer` identifies who performed the cherry-pick.

### Error cases

| Condition | Error |
|-----------|-------|
| `commit` is an unsupported rootish form (HEAD, reflog, range, caret) | `BadValue: dumboCherryPick: ...` |
| Cherry-pick produces conflicts | `ok: 0` response with `conflicts` array |

---

## dumboRebase

Reapplies all commits on the current branch not reachable from `onto` onto the tip of `onto`, rewriting branch history.

**Alias:** `doltRebase`

### Parameters (rebase initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `onto` | string | **yes** |  -- | Branch name or rootish to rebase onto |
| `committer` | string | no |  -- | `"Name <email>"` committer identity for replayed commits. When omitted, committer equals the original commit's author. |

### Parameters (continue / abort)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `continue` | bool/int | no | `false` | Continue after resolving conflicts |
| `abort` | bool/int | no | `false` | Abort and restore original branch state |
| `committer` | string | no |  -- | Committer identity override (same as initiation) |

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

### Response fields (conflicts  -- ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `conflictCommit` | string | Hash of the commit being replayed when conflict occurred |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
// Rebase feature onto main with an explicit committer
db.getSiblingDB("orders@feature").runCommand({ dumboRebase: 1, onto: "main", committer: "alice <alice@acme.com>" })
// { commitsReplayed: 3, newTip: "abc123...", ok: 1 }

// If conflicts arise  -- resolve them, then continue
db.getSiblingDB("orders@feature").runCommand({ dumboRebase: 1, continue: 1, committer: "alice <alice@acme.com>" })

// Abort
db.getSiblingDB("orders@feature").runCommand({ dumboRebase: 1, abort: 1 })
// { newTip: "original-tip...", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `onto` is an unsupported rootish form | `BadValue: dumboRebase: ...` |
| Conflict during replay | `ok: 0` response with `conflicts` and `conflictCommit` |

### Notes

- Rebase rewrites commit history; don't rebase branches that others have already pulled.
- When paused on a conflict, `conflictCommit` identifies which original commit caused it.
- Replayed commits preserve the original `author` but set `committer` to the person performing the rebase and `committerTimestamp` to the time of replay. This distinction is visible in `dumboLog`.

---

## dumboLog

Returns commit history for the branch encoded in the database name, walking the
full commit graph in reverse topological order. Both parents of merge commits
are visited; ties between commits at the same height are broken by newer
timestamp first.

**Alias:** `doltLog`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `limit` | int32 | no | unset (default 20) | Maximum number of commits to return. `0` explicitly requests an empty list. |
| `from` | string or array of strings | no | HEAD | Seed commit(s) for the traversal frontier. A single hash starts there (and still walks both parents of merges); an array seeds the walk with every listed commit. Pass back a prior response's `next` to page. |
| `all` | bool | no | `false` | Seed the walk with the HEAD of every branch, so it spans all branches (`git log --all`; tags excluded). Mutually exclusive with `from`. |
| `filters` | array | no | unset | Entries are a collection-name string (whole collection) or a `{collection: spec}` document; returns only commits that touched matching documents (see Filtering). `spec` is a single `_id`, an array of `_id`s, or a `{$match: <query>}` predicate. |
| `stat` | bool | no | `false` | When true, include per-collection change counts (`stat` array) for each commit (analogous to `git log --stat`). Scoped to the matched docs when `filters` is set. |
| `patch` | bool | no | `false` | When true, include full document-level diffs (`diff` array) for each commit (analogous to `git log --patch`). Scoped to the matched docs when `filters` is set. |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `commits` | array | List of commit objects (see below) |
| `next` | array of strings | **Only when more history remains.** The frontier seed set for the next page; pass it back as `from`. Omitted when the traversal is exhausted. |
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
| `committer` | string | Committer identity. Equals `author` for regular commits/merges/reverts; differs for cherry-pick and rebase (committer is the applier, author is preserved from the original). |
| `committerTimestamp` | Date | Timestamp of when the commit was applied |
| `stat` | array | **Only when `stat: true`.** Per-collection change counts: `[{name, status, added, modified, deleted}, ...]` |
| `diff` | array | **Only when `patch: true`.** Full document diffs per collection (same shape as `dumboDiff` collections). |

### Example

```js
db.getSiblingDB("orders@main").runCommand({ dumboLog: 1, limit: 2 })
// {
//   commits: [
//     {
//       commitId:           "v9ra3pmi0f6kotj5k3fganpmb3oi9t1k",
//       refs:               ["HEAD", "main"],
//       parent1:            "tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b",
//       message:            "add order #1",
//       timestamp:          ISODate("2026-04-14T20:00:00.000Z"),
//       author:             "alice <alice@acme.com>",
//       committer:          "alice <alice@acme.com>",
//       committerTimestamp: ISODate("2026-04-14T20:00:00.000Z")
//     },
//     {
//       commitId:           "tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b",
//       parent1:            "5vi6e5t3riqpgh6fq0j1pf0r0imuqhsn",
//       message:            "Initialize database",
//       timestamp:          ISODate("2026-04-14T08:55:12.000Z"),
//       author:             "DumboDB",
//       committer:          "DumboDB",
//       committerTimestamp: ISODate("2026-04-14T08:55:12.000Z")
//     }
//   ],
//   ok: 1
// }
```

### Pagination

The traversal boundary of a commit DAG is a *set* of commits (the frontier),
not a single pointer, so pagination uses a seed set rather than `skip`. Each
response carries `next` (the frontier) while more history remains; feed it
back as `from` to fetch the following page, and stop when `next` is absent:

```js
let from = undefined
do {
  const page = db.runCommand({ dumboLog: 1, limit: 50, ...(from && { from }) })
  for (const c of page.commits) print(c.commitId, c.message)
  from = page.next            // array of hashes, or undefined at the end
} while (from)
```

`next` is a transparent array of commit hashes (order is not significant);
across pages the commits tile the history exactly once, in the same order as
an unpaginated walk.

To page across **all** branches, start with `all: true` instead of `from`
(they are mutually exclusive); it seeds the first page with every branch HEAD,
and subsequent pages continue from `next` as usual:

```js
db.runCommand({ dumboLog: 1, limit: 50, all: true })  // page 1 spans all branches
```

### Filtering

`filters` restricts the log to commits that **touched** matching documents,
identified by collection and either an `_id` or a `$match` query. It is an array
of single-key `{collection: spec}` entries; the spec is one `_id`, an array of
`_id`s, or a `$match` query (see below). A commit is included if it added,
removed, or modified (versus its first parent) one of those documents in that
collection, for **any** listed entry (OR).

An `_id` value may be **any valid BSON `_id` type** -- number, string,
`ObjectId`, date, decimal, or a document/subdocument -- and is matched with the
same equality `find({_id: ...})` uses: numeric cross-type coercion
(`int`/`long`/`double`), and exact, field-order-sensitive equality for document
`_id`s. The only type that is never a valid `_id` is an array, which is why an
array value is unambiguously the id-list form. Because `_id` is immutable,
every insert, update, and delete of a document is a touch -- the filter cleanly
answers "show me the commits in this document's history."

```js
// history of one document (the common case):
db.runCommand({ dumboLog: 1, filters: [ { orders: 42 } ] })

// several documents, possibly across collections (OR):
db.runCommand({ dumboLog: 1, filters: [ { orders: [1, 2] }, { users: 7 } ] })

// _id can be any valid _id type, including a subdocument:
db.runCommand({ dumboLog: 1, filters: [ { events: { region: "us", seq: 5 } } ] })

// whole collection: a bare collection-name string matches any _id:
db.runCommand({ dumboLog: 1, filters: [ "orders" ] })

// whole collection OR a specific doc in another collection:
db.runCommand({ dumboLog: 1, filters: [ "orders", { users: 7 } ] })
```

A **bare collection-name string** entry is the whole-collection wildcard: the
commit qualifies if it touched any document in that collection. A string entry
is distinct from a `{collection: id}` document, so there's no ambiguity, and a
whole-collection entry subsumes any specific `_id`s listed for the same
collection.

A list element may also be a **`{$match: <query>}`** predicate. Unlike an
`_id`, a `$match` is evaluated **per commit**: a commit is included when it
**touched** (added, removed, or modified versus its first parent) a document
satisfying the query.

```js
// commits that touched an order while it was pending:
db.runCommand({ dumboLog: 1, filters: [ { orders: [ { $match: { status: "pending" } } ] } ] })
```

`$match` elements, explicit `_id`s, and id-lists in the same entry OR together.
For AND, put the conditions in a single `$match` query: its fields are ANDed
(e.g. `{ $match: { status: "pending", customer: "4242" } }`), and it supports
the usual `find()` boolean operators (`$and`/`$or`/`$nor`) internally for
anything more complex.

**Touched mechanics.** For a modified document, `$match` matches if
**either** the pre-image (parent) **or** the post-image (this commit) satisfies
the query. Consequences worth knowing:

- A commit that changes a document *out of* the matched set is still included
  -- e.g. under `{status:"pending"}`, the commit that ships a pending order
  matches via its pre-image.
- A commit is included only if it *touched* a document the filter matches; a
  matching document merely existing in the commit's snapshot is not enough
  (presence alone is not a match).
- `stat`/`patch` for an included commit are scoped to the documents that
  matched at that commit (the same pre/post-image rule).

Only `$match` is supported as a list operator; any other `$`-operator is
rejected. `$`-prefixed field names are never valid in an `_id`, which is what
lets `{$match: ...}` coexist unambiguously with `_id`s (including composite
document `_id`s).

**`$changed` -- match a field that changed.** Inside a `$match`, a field may use
the `{$changed: true}` qualifier, which matches when that field **differs**
between the commit and its first parent -- regardless of the values. Unlike a
value predicate (which inspects a single image), `$changed` is a property of the
before/after pair, so no value matcher (`$regex`, `$exists`, ...) can express
it.

```js
// commits where an order's status field changed (any value -> any value):
filters: [ { orders: [ { $match: { status: { $changed: true } } } ] } ]

// status changed AND that order belongs to a specific customer:
filters: [ { orders: [ { $match: { status: { $changed: true }, customer: "4242" } } ] } ]

// (status changed OR priority is high) for that customer -- full $and/$or nesting:
filters: [ { orders: [ { $match: { customer: "4242", $or: [
  { status: { $changed: true } },
  { priority: "high" }
] } } ] } ]
```

`$changed` semantics:
- **Presence-counting**: a value change, a field added, a field removed, and a
  whole-document add or delete all count as the field changing.
- **Nested**: `{ "shipping.carrier": { $changed: true } }` matches a change to
  that nested field; a change to an enclosing object (or the whole document)
  also counts as the nested field changing.
- Combines with value conditions (implicit AND) and `$and`/`$or`/`$nor`
  nesting; AND binds within a single document.
- `$changed` is a DumboDB extension to `$match` only -- it is not supported as a
  `find()` operator, and its value must be `true`.

With `filters` active, `limit` counts matching commits: the walk continues
past non-matching commits until `limit` matches are found. `next` is
positioned after the last commit examined, so paging does not re-scan skipped
commits, and `filters` composes with `from` and `all`.

When `stat`/`patch` is requested with `filters`, the output is **scoped** to
the matched documents -- only the filtered collections, and within them only
the requested `_id`s that changed (like `git log -p -- path`).

### Error cases

- `filters` that is not an array, an entry that is neither a collection-name
  string nor a single-key document, an empty `_id` array, an `_id`-list element
  that is itself an array, or a `$`-operator other than `$match`, returns
  `TypeMismatch` / `BadValue`.
- `all` together with `from` returns `BadValue` (they are mutually exclusive).
- A `from` array element that is not a string returns `TypeMismatch`; an
  unparseable or unknown commit hash returns `OperationFailed`.
- Missing backend support returns `OperationFailed`.

### Notes

- Every database is initialized with a root `"Initialize database"` commit; the root commit has no `parent1`.
- `refs` appears only on commits that are the HEAD of one or more branches. The connection branch gets both `"HEAD"` and its bare name; other branches get only the bare name.
- Merge commits include `parent2`.
- `from` starts traversal at the given commit(s); the walk still visits both parents of any merge commit reachable from that start.

---

## dumboStatus

Returns uncommitted changes on the branch encoded in the database name.

**Alias:** `doltStatus`

### Parameters

None (beyond the implicit `$db` connection).

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `branch` | string | Active branch name (or rootish for read-only connections) |
| `dirty` | bool | `true` when there are uncommitted changes; `false` otherwise |
| `readonly` | bool | `true` when the connection is a read-only rootish (commit hash, ancestor, tag); `false` on branches |
| `commitId` | string | HEAD commit hash. Only present when `dirty` is `false` and `readonly` is `false`. |
| `collections` | array | Changed collections: `[{name, status, added, modified, deleted}, ...]`. Only present on writable connections. |
| `mergeState` | string | **Only present during an in-progress operation.** One of `"merge"`, `"cherry-pick"`, `"rebase"`, or `"revert"`. |
| `conflicts` | array | **Only present during an in-progress operation.** Per-collection conflict counts: `[{collection, count}, ...]`. |
| `ok` | number | `1` |

### Collection entry

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Collection name |
| `status` | string | `"added"`, `"modified"`, or `"deleted"` (see table below) |
| `added` | int | Docs in the working-set copy but not in the HEAD copy |
| `modified` | int | Docs present in both with different content (counted per doc, not per field) |
| `deleted` | int | Docs in the HEAD copy but not in the working-set copy |

### Status values

| Value | Meaning |
|-------|---------|
| `"added"` | Collection exists in working set but not in HEAD |
| `"modified"` | Collection exists in both but content differs |
| `"deleted"` | Collection exists in HEAD but not in working set |

### Example

```js
var db = db.getSiblingDB("orders@main")

db.orders.insertOne({ _id: 99, amount: 500 })

db.runCommand({ dumboStatus: 1 })
// {
//   branch: "main",
//   collections: [
//     { name: "orders", status: "modified", added: 1, modified: 0, deleted: 0 }
//   ],
//   ok: 1
// }

// After committing  -- clean state
db.runCommand({ dumboCommit: 1, message: "add order 99", author: "alice <alice@acme.com>" })
db.runCommand({ dumboStatus: 1 })
// { branch: "main", collections: [], ok: 1 }

// During a merge conflict  -- mergeState and conflicts appear
db.runCommand({ dumboMerge: 1, merge_in: "feature" })
// (merge returns ok:0 with conflicts)

db.runCommand({ dumboStatus: 1 })
// {
//   branch: "main",
//   collections: [
//     { name: "orders", status: "modified", added: 0, modified: 1, deleted: 0 }
//   ],
//   mergeState: "merge",
//   conflicts: [ { collection: "orders", count: 1 } ],
//   ok: 1
// }
```

### Notes

- Only collections with uncommitted changes appear; unchanged collections are omitted.
- `collections` is always an array (empty when clean).
- Counts are **document-level**. A change spanning several fields in one document still counts as one modified doc; use `dumboDiff` for field-level detail.

---

## dumboDiff

Returns a document-level diff between two states for the branch encoded in the database name.

**Alias:** `doltDiff`

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
var db = db.getSiblingDB("orders@main")

// Insert baseline and commit
db.orders.insertOne({ _id: 1, amount: 100 })
const r = db.runCommand({ dumboCommit: 1, message: "baseline", author: "alice <alice@acme.com>" })
const hashBase = r.commitId

// Modify the working set
db.orders.updateOne({ _id: 1 }, { $set: { amount: 150 } })

// Diff HEAD vs working set
db.runCommand({ dumboDiff: 1 })
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
db.runCommand({ dumboDiff: 1, from: hashBase, to: "HEAD" })
```

### Error cases

| Condition | Error |
|-----------|-------|
| Unsupported rootish form in `from` or `to` | `OperationFailed: rootish ...` |

### Notes

- Omit both `from` and `to` to get working-set changes vs HEAD.
- Only collections with at least one change appear in `collections`.
- Unchanged fields do not appear in `modified[].diff`.
- `HEAD` resolves to the connection's own branch tip (not necessarily `main`).
- Reversing `from` and `to` inverts the diff: `added` and `removed` swap roles.

---

## dumboReset

Moves the branch HEAD to the specified commit. Supports soft (default) and hard modes.

**Alias:** `doltReset`

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
var db = db.getSiblingDB("orders@main")

// Soft reset to one commit back  -- uncommitted changes preserved
const log = db.runCommand({ dumboLog: 1, limit: 2 })
const previousHash = log.commits[1].commitId

db.runCommand({ dumboReset: 1, to: previousHash })
// { commitId: "<previousHash>", ok: 1 }

// Hard reset to HEAD  -- discard all uncommitted changes
db.runCommand({ dumboReset: 1, hard: true })
// { commitId: "<current-HEAD-hash>", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `to` is not a valid commit hash | `OperationFailed: ...` |

### Notes

- Soft reset with no `to` is a no-op for HEAD but still returns `ok: 1`.
- Hard reset is irreversible: uncommitted changes in the working set are permanently discarded.

---

## dumboRevert

Applies the inverse diff of a named commit onto the current branch, creating a new commit that undoes those changes.

**Alias:** `doltRevert`

### Parameters (revert initiation)

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `commit` | string | **yes** |  -- | Commit hash to revert |
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
| `committer` | string | Committer identity (`"Name <email>"`). Equals `author` for revert commits. |
| `committerTimestamp` | Date | Timestamp of when the revert commit was created |
| `ok` | number | `1` |

### Response fields (abort)

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Confirmation |
| `ok` | number | `1` |

### Response fields (conflicts  -- ok: 0)

| Field | Type | Description |
|-------|------|-------------|
| `conflicts` | array | `[{collection, count}, ...]` |
| `ok` | number | `0` |
| `errmsg` | string | Error description |

### Example

```js
var main = db.getSiblingDB("orders@main")

// View history to find the commit to revert
const log = main.runCommand({ dumboLog: 1, limit: 3 })
const badCommitHash = log.commits[0].commitId

// Revert it
main.runCommand({ dumboRevert: 1, commit: badCommitHash })
// {
//   commitId:           "new-revert-hash...",
//   message:            "Revert \"add order #1\"\n\nThis reverts commit <badCommitHash>.",
//   committer:          "alice <alice@acme.com>",
//   committerTimestamp: ISODate("2026-04-14T20:00:00.000Z"),
//   ok: 1
// }
```

### Error cases

| Condition | Error |
|-----------|-------|
| `commit` is an unsupported rootish form | `BadValue: dumboRevert: ...` |
| Revert produces conflicts | `ok: 0` response with `conflicts` array |

### Notes

- The revert creates a new commit; it does not alter history.
- Unlike `dumboReset`, reverting is safe to use on shared branches.
- The auto-generated message includes the original commit message and hash.

---

## dumboConflicts

Returns conflict information for an in-progress merge, cherry-pick, or rebase on the branch encoded in the database name.

**Alias:** `doltConflicts`

### Parameters

None (beyond the implicit `$db` connection).

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `collections` | array | Per-collection conflict groups (see below) |
| `ok` | number | `1` |

### Collection conflict group

| Field | Type | Description |
|-------|------|-------------|
| `collection` | string | Collection name |
| `conflicts` | array | Per-document conflict entries (see below) |

### Conflict entry fields

| Field | Type | Description |
|-------|------|-------------|
| `conflictId` | string | Unique identifier for this conflict (used with `dumboResolveConflict`) |
| `type` | string | `"documentEdit"` (one identity edited on both sides) or `"uniqueKeyCollision"` (two identities contending for one unique-index key) |
| `reason` | document | Why the two states cannot both stand (see below) |
| `base` | document or null | The common-ancestor side as `{ _id, doc }`; null if absent in the ancestor. No `diffType`. |
| `ours` | document or null | Our side as `{ _id, doc, diffType }`; null if our branch deleted the document |
| `theirs` | document or null | Their side as `{ _id, doc, diffType }`; null if their branch deleted the document |

Each non-null side carries its own `_id` (a `uniqueKeyCollision` has different
`_id`s for `ours` and `theirs`), the full `doc`, and -- for `ours`/`theirs` -- a
`diffType` of `"added"`, `"modified"`, or `"deleted"`.

#### `reason` fields

| Field | Type | Description |
|-------|------|-------------|
| `code` | string | `"bothModified"`, `"modifyDelete"`, `"deleteModify"` (document edits), or `"uniqueKeyCollision"` |
| `message` | string | Human-readable explanation |
| `index` | string | Unique index name; present only for `uniqueKeyCollision` |
| `key` | document | The colliding key value; present only for `uniqueKeyCollision` |

### Example

```js
var main = db.getSiblingDB("orders@main")

// After a merge conflict (both branches modified _id:1):
main.runCommand({ dumboConflicts: 1 })
// {
//   collections: [
//     {
//       collection: "orders",
//       conflicts: [
//         {
//           conflictId: "2onhBAqtYZDVqr4WfXh8pA",
//           type:   "documentEdit",
//           reason: { code: "bothModified",
//                     message: "branch 'main' (ours) and branch 'feature' (theirs) both modified document 1" },
//           base:   { _id: 1, doc: { _id: 1, amount: 100 } },
//           ours:   { _id: 1, doc: { _id: 1, amount: 150 }, diffType: "modified" },
//           theirs: { _id: 1, doc: { _id: 1, amount: 200 }, diffType: "modified" }
//         }
//       ]
//     }
//   ],
//   ok: 1
// }

// A uniqueKeyCollision (each branch added a different doc on the same unique key):
// {
//   conflictId: "...",
//   type:   "uniqueKeyCollision",
//   reason: { code: "uniqueKeyCollision",
//             message: 'unique index "by_sku": branch \'main\' (ours) and branch \'feature\' (theirs) both have sku = "S-1"',
//             index: "by_sku", key: { sku: "S-1" } },
//   base:   null,
//   ours:   { _id: 10, doc: { _id: 10, sku: "S-1" }, diffType: "added" },
//   theirs: { _id: 20, doc: { _id: 20, sku: "S-1" }, diffType: "added" }
// }
```

---

## dumboResolveConflict

Resolves a single document conflict in the current in-progress merge, cherry-pick, or rebase.

**Alias:** `doltResolveConflict`

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `collection` | string | **yes** |  -- | Collection containing the conflict |
| `conflictId` | string | **yes** |  -- | Conflict identifier from `dumboConflicts` |
| `resolution` | string | **yes** |  -- | `"ours"`, `"theirs"`, or `"custom"` |
| `value` | document | conditional |  -- | Required when `resolution` is `"custom"`; the document to use |

### Resolution options

| Value | `documentEdit` | `uniqueKeyCollision` |
|-------|----------------|----------------------|
| `"ours"` | Keep the local (into-branch) version | Keep ours's document on the key; discard theirs's contender |
| `"theirs"` | Use the incoming (from-branch) version | Key-ownership swap: evict ours's document and install theirs's contender under the key |
| `"custom"` | Use the document provided in `value` | Use the document provided in `value` (rejected if it collides with a different document on a unique index) |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `ok` | number | `1` on success |

### Example

```js
var main = db.getSiblingDB("orders@main")

// Resolve using our version
main.runCommand({
  dumboResolveConflict: 1,
  collection: "orders",
  conflictId: "2onhBAqtYZDVqr4WfXh8pA",
  resolution: "ours"
})
// { ok: 1 }

// Resolve using their version
main.runCommand({
  dumboResolveConflict: 1,
  collection: "orders",
  conflictId: "2onhBAqtYZDVqr4WfXh8pA",
  resolution: "theirs"
})
// { ok: 1 }

// Resolve with a custom document
main.runCommand({
  dumboResolveConflict: 1,
  collection: "orders",
  conflictId: "2onhBAqtYZDVqr4WfXh8pA",
  resolution: "custom",
  value: { _id: 1, amount: 175, status: "reconciled" }
})
// { ok: 1 }

// After resolving all conflicts, complete the merge:
main.runCommand({ dumboMerge: 1, continue: 1 })
```

### Error cases

| Condition | Error |
|-----------|-------|
| `resolution` is `"custom"` but `value` is missing | `BadValue: dumboResolveConflict: resolution 'custom' requires a 'value' document` |
| Unknown `conflictId` | `OperationFailed: ...` |

### Notes

- Resolve all conflicts in a collection before moving to the next.
- After all conflicts across all collections are resolved, call the appropriate `continue` command (`dumboMerge`, `dumboCherryPick`, or `dumboRebase`).

---

## Conflict resolution workflow

The same three-step pattern applies to `dumboMerge`, `dumboCherryPick`, and `dumboRebase` when they produce conflicts:

```js
var main = db.getSiblingDB("orders@main")

// Step 1: Operation returns ok: 0 with conflict summary
main.runCommand({ dumboMerge: 1, merge_in: "feature" })
// { conflicts: [ { collection: "orders", count: 1 } ], ok: 0, errmsg: "..." }

// Step 2: Inspect and resolve each conflict
const detail = main.runCommand({ dumboConflicts: 1 })
detail.collections.forEach(col => {
  col.conflicts.forEach(c => {
    main.runCommand({
      dumboResolveConflict: 1,
      collection: col.collection,
      conflictId: c.conflictId,
      resolution: "ours"
    })
  })
})

// Step 3: Continue to create the final commit
main.runCommand({ dumboMerge: 1, continue: 1 })
// { commitId: "...", message: "Merge branch 'feature' into 'main'", ok: 1 }
```

---

## dumboTag

Create, list, or delete tags at specific commits. Tags are stored using Dolt's `refs/tags/<name>` refspec and are interoperable with the `dolt tag` CLI.

**Alias:** `doltTag`


### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `name` | string | no |  -- | Tag name. Required for create/delete. Must not contain `@` or whitespace. Omit to list all tags. |
| `hash` | string | no | current branch HEAD | Rootish (commit hash, branch, tag, or ancestor expression) to tag |
| `delete` | bool | no | `false` | Set to `true` to delete the named tag |
| `message` | string | no | `""` | Tag description |
| `author` | string | no | `"dumbodb <dumbodb@dumbodb>"` | Tagger identity "Name <email>" |

### Response fields (create / delete)

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Tag name |
| `commitId` | string | Commit hash the tag points to |
| `author` | string | Tagger identity "Name <email>" |
| `message` | string | Tag description |
| `timestamp` | Date | Creation time |
| `ok` | number | `1` on success |

### Response fields (list)

| Field | Type | Description |
|-------|------|-------------|
| `tags` | array | List of tag objects, each with `name`, `commitId`, `author`, `message`, `timestamp` |
| `ok` | number | `1` on success |

### Examples

**List all tags:**

```js
db.runCommand({ dumboTag: 1 })
// { tags: [ { name: "v1.0", commitId: "abc123...", ... } ], ok: 1 }
```

**Create a tag at the current branch HEAD:**

```js
db.runCommand({ dumboTag: 1, name: "v1.0" })
// { name: "v1.0", commitId: "...", author: "dumbodb <dumbodb@dumbodb>", message: "", timestamp: ISODate("..."), ok: 1 }
```

**Create a tag at a specific commit with metadata:**

```js
db.runCommand({
  dumboTag: 1,
  name: "v2.0",
  hash: "na7kfra98h45fr2u5qtr30o2ggm7vh61",
  message: "Production release",
  author: "alice",
  email: "alice@acme.com"
})
```

**Tag a relative ancestor:**

```js
db.runCommand({ dumboTag: 1, name: "before-refactor", hash: "main~3" })
```

**Delete a tag:**

```js
db.runCommand({ dumboTag: 1, name: "v1.0", delete: true })
```

---

## dumboGC

Run garbage collection on the database's chunk store. Sweeps chunks that are no longer reachable from any branch ref or active session, and (in `full` mode) compacts the surviving chunks into archive files in the oldgen store.

**Alias:** `doltGC`

GC scope is the entire underlying chunk store for the database, regardless of which branch the command is invoked against: `db.getSiblingDB("orders@feature").runCommand({dumboGC: 1})` and `db.getSiblingDB("orders@main").runCommand({dumboGC: 1})` have the same effect, because one chunk store backs every branch in a logical database.

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `mode` | string | no | `"default"` | `"default"`: sweep new-gen and unreferenced old-gen chunks. `"full"`: also rewrite reachable chunks (compacts the store). |

### Response fields

| Field | Type | Description |
|-------|------|-------------|
| `db` | string | Resolved base database name (branch selector stripped) |
| `mode` | string | Effective mode, `"default"` or `"full"` |
| `durationMs` | number | Wall-clock duration of the GC pass, in milliseconds |
| `sizeBefore` | number | Bytes on disk before GC |
| `sizeAfter` | number | Bytes on disk after GC |
| `chunksBefore` | number | Chunk count before GC |
| `chunksAfter` | number | Chunk count after GC |
| `ok` | number | `1` on success |

All numeric fields are encoded as BSON Doubles. Chunk counts and byte sizes both fit exactly under 2^53.

### Examples

**Default-mode GC (sweep unreachable chunks):**

```js
db.runCommand({ dumboGC: 1 })
// {
//   db: "orders",
//   mode: "default",
//   durationMs: 42,
//   sizeBefore: 9876543,
//   sizeAfter: 5432109,
//   chunksBefore: 12345,
//   chunksAfter: 6789,
//   ok: 1
// }
```

**Full-mode GC (revisit the entire chunk store):**

```js
db.runCommand({ dumboGC: 1, mode: "full" })
```

Default mode walks new-gen chunks only -- chunks already promoted to oldgen archives are assumed to still be reachable and are not revisited. Full mode walks the entire chunk store, including oldgen archives, so chunks that survived earlier GCs but have since become unreachable (typically because the branch or tag that kept them alive was deleted) are reclaimed. Use full mode after deleting long-lived branches or tags whose chunks have already been archived; default mode would skip those chunks regardless of reachability.

### Errors

| Condition | Error |
|-----------|-------|
| `mode` is not `"default"` or `"full"` | `OperationFailed: DumboDBGC: unknown mode "<value>"` |
| Target database does not exist | `OperationFailed: DumboDBGC: database "<name>" does not exist` |
| Command run from a non-session context | `OperationFailed: DumboDBGC: no session in context` |

### Notes

- `dumboGC` is a durable command (routed through the session commit fence), so it is mutually exclusive with other durable commands on the same connection.
- The calling session is excluded from the GC safepoint's wait set, so the running command does not deadlock on itself. Other connections' in-flight commands are awaited at the pre-finalize safepoint and block briefly until GC completes.

---

## dumboUndrop

Restores a soft-deleted database, or lists the databases available to undrop.

When a database is dropped with `dropDatabase`, it is not deleted: its directory is moved into the preserved-drops directory and can be restored. `dumboUndrop` reverses that move. Repeat drops of the same name are all retained, distinguished by a `dropId`. Pass `to_database` to restore under a different name.

**Alias:** `doltUndrop`

**Admin-only:** must be run against the `admin` database.

### Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `name` | string | no | `""` | Database to restore. Omit to list databases available to undrop. Must be a root database name (no `@branch`/`@revision`). |
| `dropId` | string | no | `""` | Selects a specific drop when `name` has more than one preserved copy. Omit to restore the most recent drop. Use the `dropId` from the list response. |
| `to_database` | string | no | `""` | Restore the drop under this name instead of its original. Requires `name`; must be a root database name (no `@branch`/`@revision`). |

### Response fields (list mode, no `name`)

| Field | Type | Description |
|-------|------|-------------|
| `dropped` | array | One entry per preserved drop, most recently dropped first |
| `dropped[].name` | string | Database name |
| `dropped[].dropId` | string | Unique id of this drop |
| `dropped[].droppedAt` | Date | When the drop happened |
| `ok` | number | `1` on success |

### Response fields (restore mode, with `name`)

| Field | Type | Description |
|-------|------|-------------|
| `undropped` | string | Restored database name (the `to_database` name when one is given) |
| `dropId` | string | Id of the drop that was restored |
| `ok` | number | `1` on success |

### Example

```js
var admin = db.getSiblingDB("admin")

// List what can be undropped
admin.runCommand({ dumboUndrop: 1 })
// {
//   dropped: [
//     { name: "orders", dropId: "1775505756999075683", droppedAt: ISODate("2026-06-24T21:00:00.000Z") }
//   ],
//   ok: 1
// }

// Restore it
admin.runCommand({ dumboUndrop: 1, name: "orders" })
// { undropped: "orders", dropId: "1775505756999075683", ok: 1 }

// Restore it under a different name
admin.runCommand({ dumboUndrop: 1, name: "orders", to_database: "orders_recovered" })
// { undropped: "orders_recovered", dropId: "1775505756999075683", ok: 1 }
```

### Error cases

| Condition | Error |
|-----------|-------|
| Not run against `admin` | `OperationFailed: dumboUndrop: can only be run against the admin database` |
| No dropped database with that name | `DatabaseDoesNotExist: undrop: no dropped database named "<name>"; ...` |
| `dropId` does not match any drop | `DatabaseDoesNotExist: undrop: database "<name>" has no dropped copy with dropId "<id>"` |
| A live database with the target name already exists | `OperationFailed: undrop: a live database named "<name>" already exists` |
| `name` is revision-qualified (`db@rev`) | `OperationFailed: dumboUndrop: name must be a root database, ...` |
| `to_database` given without `name` | `OperationFailed: dumboUndrop: to_database requires name` |
| `to_database` is revision-qualified (`db@rev`) | `OperationFailed: dumboUndrop: to_database must be a root database, ...` |

### Notes

- The full commit history of the restored database is preserved exactly as it was at drop time.
- Preserved databases are permanently deleted automatically once they are more than 30 days old. A background job checks hourly and logs an INFO line for each deletion. Undrop a database before then to recover it.
- System databases (`admin`, `config`, `local`) cannot be dropped, so they are never preserved.
