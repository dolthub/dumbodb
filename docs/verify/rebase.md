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

A running DocuDolt instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DocuDolt address if different.

---

## Setup: Create a database with two branches and commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("rebasedb")
db.dropDatabase()

// C1: initial commit on main.
db.items.insertOne({_id: 1, v: 1})
const r1 = db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch at C1.
const rBranch = db.getSiblingDB("rebasedb__d_main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranch)
// Expected: { branch: "feature", ok: 1 }

// C2: feature adds _id:2.
db.getSiblingDB("rebasedb__d_feature").items.insertOne({_id: 2, v: 2})
const r2 = db.getSiblingDB("rebasedb__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(r2)
// Expected: { commitId: "<hashC2>", branch: "feature", message: "feature-adds-2", ok: 1 }
const hashC2 = r2.commitId

// C3: main advances (feature diverges from main).
db.items.insertOne({_id: 3, v: 3})
const r3 = db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
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

## Scenario 1: Clean rebase — response shape

Rebase feature onto main. C2 is replayed on top of C3.

```js
const rRebase = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
printjson(rRebase)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitsReplayed` equals `1` (C2 replayed onto C3)
- `newTip` is a non-empty string, distinct from `hashC2`
- `newTip` is not `hashC3` (it's the rebased copy of C2, not main's tip)

---

## Scenario 2: Data visible after rebase

```js
db.getSiblingDB("rebasedb__d_feature").items.countDocuments({})
// Expected: 3

db.getSiblingDB("rebasedb__d_feature").items.findOne({_id: 3})
// Expected: { _id: 3, v: 3 }  (_id:3 from main is now visible on feature)

db.getSiblingDB("rebasedb__d_feature").items.findOne({_id: 2})
// Expected: { _id: 2, v: 2 }  (original feature commit still present)
```

---

## Scenario 3: Rebased commit is single-parent (parent = main's C3)

```js
const rLog = db.getSiblingDB("rebasedb__d_feature").runCommand({doltLog: 1, limit: 1})
printjson(rLog)
// Expected: { commits: [ { commitId: "<newTip>", parent1: "<hashC3>", message: "feature-adds-2", ... } ], ok: 1 }

const head = rLog.commits[0]
print("head.parent1 =", head.parent1)
print("head.parent2 =", head.parent2)  // should be undefined
```

Key checks:
- `head.parent2` is absent (rebased commit is single-parent, not a merge commit)
- `head.parent1` equals `hashC3` (main's tip before the rebase)

---

## Scenario 4: Already up-to-date (no commits to replay)

Feature is already rebased onto main, so a second rebase replays nothing.

```js
const rRebase2 = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
printjson(rRebase2)
// Expected: { commitsReplayed: 0, newTip: "<hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitsReplayed` equals `0`

---

## Scenario 5: Abort rebase

Advance main and feature with a non-conflicting commit, then start a clean rebase and
attempt to abort it (no rebase in progress → error).

```js
// Advance main.
db.getSiblingDB("rebasedb__d_main").items.insertOne({_id: 4, v: 4})
const r4main = db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "main-adds-4", author: "test <test@example.com>"})
printjson(r4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-4", ok: 1 }

// Advance feature.
db.getSiblingDB("rebasedb__d_feature").items.insertOne({_id: 5, v: 5})
const r5feat = db.getSiblingDB("rebasedb__d_feature").runCommand({doltCommit: 1, message: "feature-adds-5", author: "test <test@example.com>"})
printjson(r5feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-5", ok: 1 }

// Clean rebase succeeds immediately.
const rClean = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
printjson(rClean)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }

// Attempting abort when no rebase is in progress returns an error.
const rAbort = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, abort: 1})
printjson(rAbort)
// Expected: { ok: 0, errmsg: "..." }  (no rebase in progress to abort)
```

Key checks:
- The clean rebase returns `ok: 1`
- The abort attempt returns `ok: 0` with an error message

---

## Scenario 6: Conflict during rebase — structured error response

Use a fresh database.

```js
var cdb = db.getSiblingDB("rebaseconflict")
cdb.dropDatabase()

cdb.items.insertOne({_id: 1, v: 1})
const rc1 = cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(rc1)
// Expected: { commitId: "<hash>", branch: "main", message: "initial", ok: 1 }

const rBranchC = cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranchC)
// Expected: { branch: "feature", ok: 1 }

cdb.getSiblingDB("rebaseconflict__d_feature").items.insertOne({_id: 2, v: 2})
const rc2 = cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(rc2)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-2", ok: 1 }

cdb.items.insertOne({_id: 3, v: 3})
const rc3 = cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
printjson(rc3)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-3", ok: 1 }

// Create conflict: both branches modify _id:1.
cdb.getSiblingDB("rebaseconflict__d_main").items.updateOne({_id: 1}, {$set: {v: 100}})
const rc4main = cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "main-changes-1", author: "test <test@example.com>"})
printjson(rc4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-changes-1", ok: 1 }

cdb.getSiblingDB("rebaseconflict__d_feature").items.updateOne({_id: 1}, {$set: {v: 200}})
const rc4feat = cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltCommit: 1, message: "feature-changes-1", author: "test <test@example.com>"})
printjson(rc4feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-changes-1", ok: 1 }

// Rebase — expect conflict (throws in mongosh).
try {
  cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltRebase: 1, onto: "main"})
} catch (e) {
  print(e)
  // MongoServerError: doltRebase: unresolved conflicts in 1 collection(s)
}
// The rebase is now staged with conflicts. Use doltConflicts to inspect (or abort below).
```

Key checks:
- `runCommand` throws a `MongoServerError` (ok:0 surfaces as an exception in mongosh)
- Error message contains `"doltRebase"` and mentions conflicts
- The rebase is staged — `doltConflicts` can inspect it

```js
// Abort the conflicted rebase.
const rAbortC = cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltRebase: 1, abort: 1})
printjson(rAbortC)
// Expected: { newTip: "<pre-rebase-hash>", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `newTip` is present (the pre-rebase branch HEAD)

---

## Scenario 7: Conflict during rebase — resolve and continue

Use another fresh database.

```js
var rdb = db.getSiblingDB("rebaseresolve")
rdb.dropDatabase()

rdb.items.insertOne({_id: 1, v: 1})
const rr1 = rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
printjson(rr1)
// Expected: { commitId: "<hash>", branch: "main", message: "initial", ok: 1 }

const rBranchR = rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltBranch: 1, branch: "feature"})
printjson(rBranchR)
// Expected: { branch: "feature", ok: 1 }

rdb.getSiblingDB("rebaseresolve__d_feature").items.insertOne({_id: 2, v: 2})
const rr2 = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})
printjson(rr2)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-adds-2", ok: 1 }

rdb.items.insertOne({_id: 3, v: 3})
const rr3 = rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
printjson(rr3)
// Expected: { commitId: "<hash>", branch: "main", message: "main-adds-3", ok: 1 }

// Create conflict: both branches modify _id:1.
rdb.getSiblingDB("rebaseresolve__d_main").items.updateOne({_id: 1}, {$set: {v: 100}})
const rr4main = rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "main-modifies-1", author: "test <test@example.com>"})
printjson(rr4main)
// Expected: { commitId: "<hash>", branch: "main", message: "main-modifies-1", ok: 1 }

rdb.getSiblingDB("rebaseresolve__d_feature").items.updateOne({_id: 1}, {$set: {v: 200}})
const rr4feat = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltCommit: 1, message: "feature-modifies-1", author: "test <test@example.com>"})
printjson(rr4feat)
// Expected: { commitId: "<hash>", branch: "feature", message: "feature-modifies-1", ok: 1 }

// Start rebase — expect conflict (throws in mongosh).
try {
  rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltRebase: 1, onto: "main"})
} catch (e) {
  // MongoServerError: doltRebase: unresolved conflicts in 1 collection(s)
}

// Inspect conflicts.
const rConflicts = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltConflicts: 1, collection: "items"
})
printjson(rConflicts)
// Expected: { conflicts: [ { conflictId: "c0", base: { _id: 1, v: 1 },
//             ours: { _id: 1, v: 200 }, theirs: { _id: 1, v: 100 },
//             ourDiffType: "modified", theirDiffType: "modified" } ], ok: 1 }
// ours = feature's version (v:200), theirs = main's version (v:100)
const conflictId = rConflicts.conflicts[0].conflictId
print("conflictId =", conflictId)

// Resolve using "ours" (keep feature's value v:200).
const rResolve = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltResolveConflict: 1, collection: "items", conflictId: conflictId, resolution: "ours"
})
printjson(rResolve)
// Expected: { ok: 1 }

// Continue the rebase.
const rContinue = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltRebase: 1, continue: 1
})
printjson(rContinue)
// Expected: { commitsReplayed: 1, newTip: "<hash>", ok: 1 }
```

Key checks:
- `doltConflicts` returns per-document conflict detail with `conflictId`, `base`, `ours`, `theirs`, `ourDiffType`, `theirDiffType`
- After `doltResolveConflict`, `ok` equals `1`
- After `doltRebase continue:1`, `ok` equals `1`, `commitsReplayed` equals `1`, `newTip` is present
