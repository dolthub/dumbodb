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

// C1: initial commit on main
db.items.insertOne({_id: 1, v: 1})
db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})

// Create "feature" branch at C1
db.getSiblingDB("rebasedb__d_main").runCommand({doltBranch: 1, branch: "feature"})

// C2: feature adds _id:2
db.getSiblingDB("rebasedb__d_feature").items.insertOne({_id: 2, v: 2})
db.getSiblingDB("rebasedb__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})

// C3: main advances (feature diverges from main)
db.items.insertOne({_id: 3, v: 3})
db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})
```

---

## Scenario 1: Clean rebase — response shape

```js
var result = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
```

**Expected:**
- `ok: 1`
- `commitsReplayed: 1` (C2 replayed onto C3)
- `newTip` is a non-empty string, distinct from C2's original hash

---

## Scenario 2: Data visible after rebase

```js
db.getSiblingDB("rebasedb__d_feature").items.countDocuments({})
// Expected: 3

db.getSiblingDB("rebasedb__d_feature").items.findOne({_id: 3})
// Expected: {_id: 3, v: 3}  (_id:3 from main is now visible)

db.getSiblingDB("rebasedb__d_feature").items.findOne({_id: 2})
// Expected: {_id: 2, v: 2}  (original feature commit still present)
```

---

## Scenario 3: Rebased commit is single-parent (parent = main's C3)

```js
var logResult = db.getSiblingDB("rebasedb__d_feature").runCommand({doltLog: 1, limit: 1})
var head = logResult.commits[0]
```

**Expected:**
- `head.parent2` is absent (rebased commit is single-parent)
- `head.parent1` equals C3's hash (main's tip before rebase)

---

## Scenario 4: Already up-to-date (no commits to replay)

```js
var result = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
```

**Expected:**
- `ok: 1`
- `commitsReplayed: 0`

---

## Scenario 5: Abort rebase

Advance main and feature with a non-conflicting commit, then start a rebase and abort.

```js
db.getSiblingDB("rebasedb__d_main").items.insertOne({_id: 4, v: 4})
db.getSiblingDB("rebasedb__d_main").runCommand({doltCommit: 1, message: "main-adds-4", author: "test <test@example.com>"})

db.getSiblingDB("rebasedb__d_feature").items.insertOne({_id: 5, v: 5})
db.getSiblingDB("rebasedb__d_feature").runCommand({doltCommit: 1, message: "feature-adds-5", author: "test <test@example.com>"})

// This clean rebase succeeds; try aborting a non-in-progress rebase.
db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, onto: "main"})
var abortResult = db.getSiblingDB("rebasedb__d_feature").runCommand({doltRebase: 1, abort: 1})
```

**Expected:**
- The clean rebase returns `ok: 1`
- The abort attempt (when no rebase is in progress) returns an error

---

## Scenario 6: Conflict during rebase — structured error response

Use a fresh database.

```js
var cdb = db.getSiblingDB("rebaseconflict")
cdb.dropDatabase()

cdb.items.insertOne({_id: 1, v: 1})
cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltBranch: 1, branch: "feature"})

cdb.getSiblingDB("rebaseconflict__d_feature").items.insertOne({_id: 2, v: 2})
cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})

cdb.items.insertOne({_id: 3, v: 3})
cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})

// Create conflict: both branches modify _id:1.
cdb.getSiblingDB("rebaseconflict__d_main").items.updateOne({_id: 1}, {$set: {v: 100}})
cdb.getSiblingDB("rebaseconflict__d_main").runCommand({doltCommit: 1, message: "main-changes-1", author: "test <test@example.com>"})

cdb.getSiblingDB("rebaseconflict__d_feature").items.updateOne({_id: 1}, {$set: {v: 200}})
cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltCommit: 1, message: "feature-changes-1", author: "test <test@example.com>"})

// Rebase — expect conflict.
var result = cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltRebase: 1, onto: "main"})
```

**Expected:**
- `ok: 0`
- `conflicts` is a non-empty array, each entry has `collection: "items"` and `count > 0`
- `conflictCommit` is a non-empty string (hash of the commit being replayed)
- `errmsg` contains "doltRebase"

```js
// Abort the conflicted rebase.
var abortResult = cdb.getSiblingDB("rebaseconflict__d_feature").runCommand({doltRebase: 1, abort: 1})
```

**Expected:** `ok: 1`, `newTip` is present (the pre-rebase branch HEAD)

---

## Scenario 7: Conflict during rebase — resolve and continue

Use another fresh database.

```js
var rdb = db.getSiblingDB("rebaseresolve")
rdb.dropDatabase()

rdb.items.insertOne({_id: 1, v: 1})
rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "initial", author: "test <test@example.com>"})
rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltBranch: 1, branch: "feature"})

rdb.getSiblingDB("rebaseresolve__d_feature").items.insertOne({_id: 2, v: 2})
rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltCommit: 1, message: "feature-adds-2", author: "test <test@example.com>"})

rdb.items.insertOne({_id: 3, v: 3})
rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "main-adds-3", author: "test <test@example.com>"})

// Create conflict.
rdb.getSiblingDB("rebaseresolve__d_main").items.updateOne({_id: 1}, {$set: {v: 100}})
rdb.getSiblingDB("rebaseresolve__d_main").runCommand({doltCommit: 1, message: "main-modifies-1", author: "test <test@example.com>"})

rdb.getSiblingDB("rebaseresolve__d_feature").items.updateOne({_id: 1}, {$set: {v: 200}})
rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltCommit: 1, message: "feature-modifies-1", author: "test <test@example.com>"})

// Start rebase — expect conflict.
var rebaseResult = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({doltRebase: 1, onto: "main"})
// rebaseResult.ok === 0, rebaseResult.conflicts is non-empty.

// Inspect conflicts.
var conflictsResult = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltConflicts: 1, collection: "items"
})
var conflictId = conflictsResult.conflicts[0].conflictId

// Resolve using "ours".
rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltResolveConflict: 1, collection: "items", conflictId: conflictId, resolution: "ours"
})

// Continue the rebase.
var continueResult = rdb.getSiblingDB("rebaseresolve__d_feature").runCommand({
    doltRebase: 1, continue: 1
})
```

**Expected:**
- `continueResult.ok: 1`
- `continueResult.commitsReplayed: 1`
- `continueResult.newTip` is a non-empty string
