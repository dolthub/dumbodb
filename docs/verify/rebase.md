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

## Scenario 5: Abort with no rebase in progress

```js
const rAbort = db.getSiblingDB("rebasedb@feature").runCommand({doltRebase: 1, abort: 1})
printjson(rAbort)
// Expected: { ok: 0, errmsg: "..." }  (no rebase in progress to abort)
```

Key check:
- Returns `ok: 0` with an error message

---

## Scenario 6: Three-commit rebase (clean)

Create a feature branch with three commits, advance main, then rebase all three.

```js
var tdb = db.getSiblingDB("rebase3c")
tdb.dropDatabase()

tdb.items.insertOne({_id: 1, v: 1})
tdb.runCommand({doltCommit: 1, message: "C1", author: "alice <alice@acme.com>"})

tdb.getSiblingDB("rebase3c@main").runCommand({doltBranch: 1, branch: "feature"})

var feat = tdb.getSiblingDB("rebase3c@feature")
feat.items.insertOne({_id: 10, v: 10})
feat.runCommand({doltCommit: 1, message: "F1", author: "bob <bob@widgets.io>"})
feat.items.insertOne({_id: 11, v: 11})
feat.runCommand({doltCommit: 1, message: "F2", author: "bob <bob@widgets.io>"})
feat.items.insertOne({_id: 12, v: 12})
feat.runCommand({doltCommit: 1, message: "F3", author: "bob <bob@widgets.io>"})

tdb.items.insertOne({_id: 2, v: 2})
tdb.runCommand({doltCommit: 1, message: "C2", author: "alice <alice@acme.com>"})

const r = feat.runCommand({doltRebase: 1, onto: "main"})
printjson(r)
// Expected: { commitsReplayed: 3, newTip: "<hash>", ok: 1 }
```

Key checks:
- `commitsReplayed` is 3
- Feature has all 5 documents (_id: 1, 2, 10, 11, 12)

---

## Scenario 7: Three-commit rebase, first commit conflicts

```js
var tdb = db.getSiblingDB("rebase3cf")
tdb.dropDatabase()

tdb.items.insertOne({_id: 1, v: 1})
tdb.runCommand({doltCommit: 1, message: "C1", author: "alice <alice@acme.com>"})

tdb.getSiblingDB("rebase3cf@main").runCommand({doltBranch: 1, branch: "feature"})

var feat = tdb.getSiblingDB("rebase3cf@feature")
// F1: modify _id:1 (will conflict)
feat.items.updateOne({_id: 1}, {$set: {v: 100}})
feat.runCommand({doltCommit: 1, message: "F1-conflict", author: "bob <bob@widgets.io>"})
// F2, F3: add new docs (clean)
feat.items.insertOne({_id: 10, v: 10})
feat.runCommand({doltCommit: 1, message: "F2-clean", author: "bob <bob@widgets.io>"})
feat.items.insertOne({_id: 11, v: 11})
feat.runCommand({doltCommit: 1, message: "F3-clean", author: "bob <bob@widgets.io>"})

// Advance main: modify _id:1 differently
tdb.items.updateOne({_id: 1}, {$set: {v: 200}})
tdb.runCommand({doltCommit: 1, message: "C2-conflict", author: "alice <alice@acme.com>"})

// Rebase -- F1 conflicts immediately
try { feat.runCommand({doltRebase: 1, onto: "main"}) } catch(e) { print(e) }

// Resolve with ours, then continue
const conflicts = feat.runCommand({doltConflicts: 1})
const cid = conflicts.collections[0].conflicts[0].conflictId
feat.runCommand({doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "ours"})

const r = feat.runCommand({doltRebase: 1, continue: 1})
printjson(r)
// Expected: { commitsReplayed: 3, newTip: "<hash>", ok: 1 }
```

Key checks:
- First commit (F1) triggers the conflict
- After resolve + continue, all 3 commits are replayed
- Feature has 3 docs (_id: 1, 10, 11)

---

## Scenario 8: Three-commit rebase, third commit conflicts

```js
var tdb = db.getSiblingDB("rebase3cl")
tdb.dropDatabase()

tdb.items.insertOne({_id: 1, v: 1})
tdb.runCommand({doltCommit: 1, message: "C1", author: "alice <alice@acme.com>"})

tdb.getSiblingDB("rebase3cl@main").runCommand({doltBranch: 1, branch: "feature"})

var feat = tdb.getSiblingDB("rebase3cl@feature")
// F1, F2: add new docs (clean)
feat.items.insertOne({_id: 10, v: 10})
feat.runCommand({doltCommit: 1, message: "F1-clean", author: "bob <bob@widgets.io>"})
feat.items.insertOne({_id: 11, v: 11})
feat.runCommand({doltCommit: 1, message: "F2-clean", author: "bob <bob@widgets.io>"})
// F3: modify _id:1 (will conflict)
feat.items.updateOne({_id: 1}, {$set: {v: 100}})
feat.runCommand({doltCommit: 1, message: "F3-conflict", author: "bob <bob@widgets.io>"})

// Advance main: modify _id:1 differently
tdb.items.updateOne({_id: 1}, {$set: {v: 200}})
tdb.runCommand({doltCommit: 1, message: "C2-conflict", author: "alice <alice@acme.com>"})

// Rebase -- F1 and F2 replay clean, F3 conflicts
try { feat.runCommand({doltRebase: 1, onto: "main"}) } catch(e) { print(e) }

// Resolve with theirs (accept feature's v:100)
const conflicts = feat.runCommand({doltConflicts: 1})
const cid = conflicts.collections[0].conflicts[0].conflictId
feat.runCommand({doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "theirs"})

const r = feat.runCommand({doltRebase: 1, continue: 1})
printjson(r)
// Expected: { commitsReplayed: 3, newTip: "<hash>", ok: 1 }

// Verify resolved value
feat.items.findOne({_id: 1})
// Expected: { _id: 1, v: 100 }  (theirs)
```

Key checks:
- F1 and F2 replay cleanly, F3 triggers the conflict
- After resolve + continue, all 3 commits are replayed
- _id:1 has v:100 (theirs resolution)
- Feature has 3 docs (_id: 1, 10, 11)

---

## Scenario 9: Conflict during rebase  -- structured error response

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

// Rebase  -- expect conflict. The server returns {ok: 0, conflicts: [...], ...}. Mongosh throws this as a MongoServerError.
try {
  cdb.getSiblingDB("rebaseconflict@feature").runCommand({doltRebase: 1, onto: "main"})
} catch (e) {
  print(e)
  // MongoServerError: doltRebase: unresolved conflicts in 1 collection(s)
}
// The rebase is now staged with conflicts. Use doltConflicts to inspect (or abort below).
```

Key checks:
- `runCommand` returns `ok:0` (mongosh throws this as `MongoServerError`)
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

## Scenario 10: Conflict during rebase  -- resolve and continue

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

// Start rebase  -- expect conflict. The server returns {ok: 0, conflicts: [...], ...}. Mongosh throws this as a MongoServerError.
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
//   dirty: true,
//   readonly: false,
//   collections: [...],
//   mergeState: "rebase",
//   conflicts: [ { collection: "items", count: 1 } ],
//   ok: 1
// }
// Key checks:
// - dirty is true (conflicts make the workspace dirty)
// - commitId is absent
// - mergeState equals "rebase"
// - conflicts lists per-collection conflict counts

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
//         { conflictId: "<base64-id>",
//           type: "documentEdit",
//           reason: { code: "bothModified",
//                     message: "the replayed commit (ours) and branch 'main' (theirs) both modified document 1" },
//           base:   { _id: 1, doc: { _id: 1, v: 1 } },
//           ours:   { _id: 1, doc: { _id: 1, v: 200 }, diffType: "modified" },
//           theirs: { _id: 1, doc: { _id: 1, v: 100 }, diffType: "modified" } }
//       ]
//     }
//   ],
//   ok: 1
// }
// For a rebase, "ours" is the replayed feature commit (v:200) and "theirs"
// is the onto branch (main, v:100).
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
- `doltConflicts` returns `collections` array with per-document conflicts grouped by collection, each with `conflictId`, a `type`, a `reason` (`code` + `message`), and `base` / `ours` / `theirs` sides of `{ _id, doc, diffType }` (`base` has no `diffType`; a deleted side is `null`)
- After `doltResolveConflict`, `ok` equals `1`
- After `doltRebase continue:1`, `ok` equals `1`, `commitsReplayed` equals `1`, `newTip` is present
