# doltRebase Verification

Manual verification guide for `doltRebase` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_rebase_verify_test.go` (`TestRebaseVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestRebaseVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with two branches and commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("rebasedb")
db.dropDatabase()

// C1: initial commit on main.
db.items.insertOne({_id: 1, v: 1})
const r1 = db.getSiblingDB("rebasedb@main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch at C1.
const rBranch = db.getSiblingDB("rebasedb@main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranch)
// Expected: { branch: "feature", ok: 1 }

// C2: feature adds _id:2.
db.getSiblingDB("rebasedb@feature").items.insertOne({_id: 2, v: 2})
const r2 = db.getSiblingDB("rebasedb@feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(r2)
// Expected: { commitId: "<hashC2>", branch: "feature", message: "feature-adds-2", ok: 1 }
const hashC2 = r2.commitId

// C3: main advances (feature diverges from main).
db.items.insertOne({_id: 3, v: 3})
const r3 = db.getSiblingDB("rebasedb@main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
printjson(r3)
// Expected: { commitId: "<hashC3>", branch: "main", message: "main-adds-3", ok: 1 }
const hashC3 = r3.commitId

print("hashC1 =", hashC1)
print("hashC2 =", hashC2)
print("hashC3 =", hashC3)
```

After setup:
- **main** (HEAD = C3): `items` = `[ {_id:1,v:1}, {_id:3,v:3} ]`
- **feature** (HEAD = C2): `items` = `[ {_id:1,v:1}, {_id:2,v:2} ]`
- feature diverges from main at C1

---

## Scenario 1: Clean rebase  -- response shape

Rebase feature onto main. C2 is replayed on top of C3.

```js
const rRebase = db.getSiblingDB("rebasedb@feature").runCommand({doltRebase: 1, onto: "main", committer: "rebaser <rebaser@acme.com>"})
printjson(rRebase)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitsReplayed` equals `1` (C2 replayed onto C3)
- `newTip` is a non-empty string, distinct from `hashC2`
- `newTip` is not `hashC3` (it's the rebased copy of C2, not main's tip)

> **Note:** For rebased commits, `author` is preserved from the original commit while
> `committer` identifies who performed the rebase, and `committerTimestamp` records when
> the replay occurred. This distinction is visible in `doltLog`.

---

## Scenario 2: Data visible after rebase

```js
db.getSiblingDB("rebasedb@feature").items.countDocuments({})
// Expected: 3

db.getSiblingDB("rebasedb@feature").items.findOne({_id: 3})
// Expected: { _id: 3, v: 3 }  (_id:3 from main is now visible on feature)

db.getSiblingDB("rebasedb@feature").items.findOne({_id: 2})
// Expected: { _id: 2, v: 2 }  (original feature commit still present)
```

---

## Scenario 3: Rebased commit is single-parent (parent = main's C3)

```js
const rLog = db.getSiblingDB("rebasedb@feature").runCommand({doltLog: 1, limit: 1})
printjson(rLog)
// Expected: { commits: [ { commitId: "<newTip>", parent1: "<hashC3>", message: "feature-adds-2",
//            author: "test <test@example.com>", committer: "rebaser <rebaser@acme.com>", committerTimestamp: ISODate("..."), ... } ], ok: 1 }

const head = rLog.commits[0]
print("head.parent1 =", head.parent1)
print("head.parent2 =", head.parent2)  // should be undefined
print("head.author =", head.author)        // original author preserved
print("head.committer =", head.committer)  // rebaser identity
```

Key checks:
- `head.parent2` is absent (rebased commit is single-parent, not a merge commit)
- `head.parent1` equals `hashC3` (main's tip before the rebase)
- `head.author` equals `"test <test@example.com>"` (preserved from the original commit)
- `head.committer` equals `"rebaser <rebaser@acme.com>"` (the explicit committer from the rebase command)
- `head.committerTimestamp` records when the replay occurred

---

## Scenario 4: Already up-to-date (no commits to replay)

Feature is already rebased onto main, so a second rebase replays nothing.

```js
const rRebase2 = db.getSiblingDB("rebasedb@feature").runCommand({doltRebase: 1, onto: "main"})
printjson(rRebase2)
// Expected: { commitsReplayed: 0, newTip: "<hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitsReplayed` equals `0`

---

## Scenario 5: Abort rebase

Advance main and feature with a non-conflicting commit, then start a clean rebase and
attempt to abort it (no rebase in progress -> error).

```js
// Advance main.
db.getSiblingDB("rebasedb@main").items.insertOne({_id: 4, v: 4})
const r4main = db.getSiblingDB("rebasedb@main").runCommand({doltCommit: 1, message: "main-adds-4", author: "test <test@example.com>"})
printjson(r4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-4", ok: 1 }

// Advance feature.
db.getSiblingDB("rebasedb@feature").items.insertOne({_id: 5, v: 5})
const r5feat = db.getSiblingDB("rebasedb@feature").runCommand({doltCommit: 1, message: "feature-adds-5", author: "test <test@example.com>"})
printjson(r5feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-5", ok: 1 }

// Clean rebase succeeds immediately.
const rClean = db.getSiblingDB("rebasedb@feature").runCommand({doltRebase: 1, onto: "main"})
printjson(rClean)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }

// Attempting abort when no rebase is in progress returns an error.
const rAbort = db.getSiblingDB("rebasedb@feature").runCommand({doltRebase: 1, abort: 1})
printjson(rAbort)
// Expected: { ok: 0, errmsg: "..." }  (no rebase in progress to abort)
```

Key checks:
- The clean rebase returns `ok: 1`
- The abort attempt returns `ok: 0` with an error message

---

## Scenario 6: Conflict during rebase  -- structured error response

Use a fresh database.

```js
var cdb = db.getSiblingDB("rebaseconflict")
cdb.dropDatabase()

cdb.items.insertOne({_id: 1, v: 1})
const rc1 = cdb.getSiblingDB("rebaseconflict@main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(rc1)
// Expected: { commitId: "<hash>", branch: "main", message: "initial", ok: 1 }

const rBranchC = cdb.getSiblingDB("rebaseconflict@main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranchC)
// Expected: { branch: "feature", ok: 1 }

cdb.getSiblingDB("rebaseconflict@feature").items.insertOne({_id: 2, v: 2})
const rc2 = cdb.getSiblingDB("rebaseconflict@feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(rc2)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-2", ok: 1 }

cdb.items.insertOne({_id: 3, v: 3})
const rc3 = cdb.getSiblingDB("rebaseconflict@main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
printjson(rc3)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-3", ok: 1 }

// Create conflict: both branches modify _id:1.
cdb.getSiblingDB("rebaseconflict@main").items.updateOne({_id: 1}, {$set: {v: 100}})
const rc4main = cdb.getSiblingDB("rebaseconflict@main").runCommand({doltCommit: 1, message: "main-changes-1", author: "test <test@example.com>"})
printjson(rc4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-changes-1", ok: 1 }

cdb.getSiblingDB("rebaseconflict@feature").items.updateOne({_id: 1}, {$set: {v: 200}})
const rc4feat = cdb.getSiblingDB("rebaseconflict@feature").runCommand({doltCommit: 1, message: "feature-changes-1", author: "test <test@example.com>"})
printjson(rc4feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-changes-1", ok: 1 }

// Rebase  -- expect conflict (throws in mongosh).
try {
  cdb.getSiblingDB("rebaseconflict@feature").runCommand({doltRebase: 1, onto: "main"})
} catch (e) {
  print(e)
  // MongoServerError: doltRebase: unresolved conflicts in 1 collection(s)
}
// The rebase is now staged with conflicts. Use doltConflicts to inspect (or abort below).
```

Key checks:
- `runCommand` throws a `MongoServerError` (ok:0 surfaces as an exception in mongosh)
- Error message contains `"doltRebase"` and mentions conflicts
- The rebase is staged  -- `doltConflicts` can inspect it

```js
// Abort the conflicted rebase.
const rAbortC = cdb.getSiblingDB("rebaseconflict@feature").runCommand({doltRebase: 1, abort: 1})
printjson(rAbortC)
// Expected: { newTip: "<pre-rebase-hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `newTip` is present (the pre-rebase branch HEAD)

---

## Scenario 7: Conflict during rebase  -- resolve and continue

Use another fresh database.

```js
var rdb = db.getSiblingDB("rebaseresolve")
rdb.dropDatabase()

rdb.items.insertOne({_id: 1, v: 1})
const rr1 = rdb.getSiblingDB("rebaseresolve@main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(rr1)
// Expected: { commitId: "<hash>", branch: "main", message: "initial", ok: 1 }

const rBranchR = rdb.getSiblingDB("rebaseresolve@main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranchR)
// Expected: { branch: "feature", ok: 1 }

rdb.getSiblingDB("rebaseresolve@feature").items.insertOne({_id: 2, v: 2})
const rr2 = rdb.getSiblingDB("rebaseresolve@feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(rr2)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-2", ok: 1 }

rdb.items.insertOne({_id: 3, v: 3})
const rr3 = rdb.getSiblingDB("rebaseresolve@main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
printjson(rr3)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-3", ok: 1 }

// Create conflict: both branches modify _id:1.
rdb.getSiblingDB("rebaseresolve@main").items.updateOne({_id: 1}, {$set: {v: 100}})
const rr4main = rdb.getSiblingDB("rebaseresolve@main").runCommand({doltCommit: 1, message: "main-modifies-1", author: "test <test@example.com>"})
printjson(rr4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-modifies-1", ok: 1 }

rdb.getSiblingDB("rebaseresolve@feature").items.updateOne({_id: 1}, {$set: {v: 200}})
const rr4feat = rdb.getSiblingDB("rebaseresolve@feature").runCommand({doltCommit: 1, message: "feature-modifies-1", author: "test <test@example.com>"})
printjson(rr4feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-modifies-1", ok: 1 }

// Start rebase  -- expect conflict (throws in mongosh).
try {
  rdb.getSiblingDB("rebaseresolve@feature").runCommand({doltRebase: 1, onto: "main"})
} catch (e) {
  // MongoServerError: doltRebase: unresolved conflicts in 1 collection(s)
}

// Verify that doltStatus reflects the in-progress rebase and its conflicts.
const rStatus = rdb.getSiblingDB("rebaseresolve@feature").runCommand({doltStatus: 1})
printjson(rStatus)
// Expected:
// {
//   branch: "feature",
//   collections: [...],
//   mergeState: "rebase",
//   conflicts: [ { collection: "items", count: 1 } ],
//   ok: 1
// }
// Key checks:
// - mergeState equals "rebase"
// - conflicts lists per-collection conflict counts
// - These fields are only present because a rebase is in progress

// Inspect conflicts  -- returns all conflicts grouped by collection.
const rConflicts = rdb.getSiblingDB("rebaseresolve@feature").runCommand({
    doltConflicts: 1
})
printjson(rConflicts)
// Expected:
// {
//   collections: [
//     {
//       collection: "items",
//       conflicts: [
//         { conflictId: "<hex-hash>", _id: 1, base: { v: 1 },
//           ours: { v: 200 }, theirs: { v: 100 },
//           ourDiffType: "modified", theirDiffType: "modified" }
//       ]
//     }
//   ],
//   ok: 1
// }
// _id promoted to top level; ours = feature's version (v:200), theirs = main's version (v:100)
const conflictId = rConflicts.collections[0].conflicts[0].conflictId
print("conflictId =", conflictId)

// Resolve using "ours" (keep feature's value v:200).
const rResolve = rdb.getSiblingDB("rebaseresolve@feature").runCommand({
    doltResolveConflict: 1, collection: "items", conflictId: conflictId, resolution: "ours"
})
printjson(rResolve)
// Expected: { ok: 1 }

// Continue the rebase.
const rContinue = rdb.getSiblingDB("rebaseresolve@feature").runCommand({
    doltRebase: 1, continue: 1
})
printjson(rContinue)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }
```

Key checks:
- `doltConflicts` returns `collections` array with per-document conflicts grouped by collection, each with `conflictId`, `base`, `ours`, `theirs`, `ourDiffType`, `theirDiffType`
- After `doltResolveConflict`, `ok` equals `1`
- After `doltRebase continue:1`, `ok` equals `1`, `commitsReplayed` equals `1`, `newTip` is present
