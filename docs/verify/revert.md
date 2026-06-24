# doltRevert Verification

Manual verification guide for `doltRevert` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_revert_verify_test.go` (`TestRevertVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestRevertVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with two commits on main

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("revertdb")
db.dropDatabase()

// C1: insert {_id:1, v:1} on main.
db.records.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ doltCommit: 1, message: "initial", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// C2: add {_id:2, v:2}  -- this is the commit we will revert.
db.records.insertOne({ _id: 2, v: 2 })
const r2 = db.runCommand({ doltCommit: 1, message: "add-two", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { commitId: "<hashC2>", branch: "main", message: "add-two", ok: 1 }
const hashC2 = r2.commitId

print("hashC1 =", hashC1)
print("hashC2 =", hashC2)
```

After setup:
- **main** (HEAD = C2): `records` = `[ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]`

---

## Scenario 1: Clean revert  -- response shape

Revert C2 (which added `_id:2`) onto main. Since C2's parent is C1 (current base),
this applies cleanly by removing `_id:2`.

```js
const rRevert1 = db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, commit: hashC2 })
printjson(rRevert1)
// Expected: {
//   commitId: "<hashC3>",
//   message: "Revert \"add-two\"\n\nThis reverts commit <hashC2>.",
//   committer: "same as author",
//   committerTimestamp: ISODate("..."),
//   ok: 1
// }
```

Key checks:
- `ok` equals `1`
- `commitId` is a non-empty string (different from `hashC2`)
- `message` contains the original message (`"add-two"`) and the revert annotation
- `message` contains the reverted commit hash
- `committer` equals the author of the revert commit
- `committerTimestamp` records when the revert was applied

Verify main now has only `_id:1` (the revert undid the addition of `_id:2`):

```js
db.records.find({}).toArray()
// Expected: [ { _id: 1, v: 1 } ]
```

Verify the revert commit is a single-parent commit (not a merge commit):

```js
const log = db.getSiblingDB("revertdb@main").runCommand({ doltLog: 1, limit: 1 })
printjson(log.commits[0])
// Expected: no "parent2" field (single parent commit)
```

---

## Scenario 2: Custom message and author

Revert another commit with a custom message and author override.

```js
// Add another commit to revert.
db.records.insertOne({ _id: 3, v: 3 })
const r3 = db.runCommand({ doltCommit: 1, message: "add-three", author: "carol <carol@startup.dev>" })
const hashC3 = r3.commitId

// Revert with custom message and author.
const rRevert2 = db.getSiblingDB("revertdb@main").runCommand({
  doltRevert: 1,
  commit: hashC3,
  message: "undo: add item three",
  author: "alice <alice@acme.com>"
})
printjson(rRevert2)
// Expected: { commitId: "<hash>", message: "undo: add item three", committer: "alice <alice@acme.com>", committerTimestamp: ISODate("..."), ok: 1 }
```

Key checks:
- `message` equals the custom override (`"undo: add item three"`)  -- no annotation appended
- `commitId` is a fresh hash
- `committer` equals `"alice <alice@acme.com>"` (same as author for revert commits)
- `committerTimestamp` records when the revert was applied

---

## Scenario 3: Conflict during revert  -- structured error response

Create a scenario where reverting a commit causes a conflict: the commit added a document
that was subsequently modified on the current branch, so revert (which would delete it)
conflicts with the modification.

```js
// Add a document that will be the conflict target.
db.records.insertOne({ _id: 10, v: 10 })
const rAdd = db.runCommand({ doltCommit: 1, message: "add-ten", author: "alice <alice@acme.com>" })
const hashAddTen = rAdd.commitId

// Now modify _id:10 on main  -- this creates a conflict if we revert hashAddTen
// (revert would delete _id:10, but main has since modified it).
db.records.updateOne({ _id: 10 }, { $set: { v: 99 } })
db.runCommand({ doltCommit: 1, message: "modify-ten", author: "bob <bob@widgets.io>" })

// Revert hashAddTen  -- expect conflict.
// The server returns {ok: 0, conflicts: [...], ...}. Mongosh throws this as a MongoServerError.
try {
  db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, commit: hashAddTen })
} catch (e) {
  print(e)
  // MongoServerError: doltRevert: unresolved conflicts in 1 collection(s)
}
// The revert is now staged with conflicts. Continue to Scenario 4 to inspect and resolve.
```

Key checks:
- `runCommand` returns `ok:0` (mongosh throws this as `MongoServerError`)
- The error message contains `"doltRevert: unresolved conflicts in 1 collection(s)"`
- The full response document is available via `e.errorResponse` in the catch block
- The revert state is preserved  -- use `doltConflicts` to inspect (Scenario 4)

Verify that `doltStatus` reflects the in-progress revert and its conflicts:

```js
const rStatus = db.getSiblingDB("revertdb@main").runCommand({ doltStatus: 1 })
printjson(rStatus)
// Expected:
// {
//   branch: "main",
//   dirty: true,
//   readonly: false,
//   collections: [...],
//   mergeState: "revert",
//   conflicts: [ { collection: "records", count: 1 } ],
//   ok: 1
// }
```

Key checks:
- `dirty` is `true` (conflicts make the workspace dirty)
- `commitId` is absent
- `mergeState` equals `"revert"`
- `conflicts` lists per-collection conflict counts

---

## Scenario 4: Inspect and resolve conflicts, then continue

Continuing from Scenario 3 (revert with conflicts in progress).

> **Same interface as merge.** Revert conflicts are stored in the same `mergeState`
> struct as merge/cherry-pick conflicts (with an internal `isRevert` flag).
> `doltConflicts` and `doltResolveConflict` work identically for all operations  --
> only the final continuation command differs (`doltRevert continue:1`).

```js
// Step 1: Inspect conflicts  -- returns all conflicts grouped by collection.
const rConflicts = db.getSiblingDB("revertdb@main").runCommand({ doltConflicts: 1 })
printjson(rConflicts)
// Expected:
// {
//   collections: [
//     {
//       collection: "records",
//       conflicts: [
//         { conflictId: "<base64-id>",
//           type: "documentEdit",
//           reason: { code: "modifyDelete",
//                     message: "branch 'main' (ours) modified document 10; the reverted change (theirs) deleted it" },
//           base:   { _id: 10, doc: {...} },
//           ours:   { _id: 10, doc: { _id: 10, v: 99 }, diffType: "modified" },
//           theirs: null }
//       ]
//     }
//   ],
//   ok: 1
// }
// ours = main's current version (v:99); theirs is null (revert target deleted it),
// so reason.code names the modify/delete clash.

const conflictId = rConflicts.collections[0].conflicts[0].conflictId

// Step 2: Resolve  -- accept "ours" (keep main's modified version of _id:10).
const rResolve = db.getSiblingDB("revertdb@main").runCommand({
  doltResolveConflict: 1,
  collection: "records",
  conflictId: conflictId,
  resolution: "ours"
})
printjson(rResolve)
// Expected: { ok: 1 }

// Step 3: After resolution, doltConflicts returns an empty collections array.
const rAfter = db.getSiblingDB("revertdb@main").runCommand({ doltConflicts: 1 })
printjson(rAfter)
// Expected: { collections: [], ok: 1 }

// Step 4: Continue the revert.
const rContinue = db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, continue: 1 })
printjson(rContinue)
// Expected: { commitId: "<hash>", message: "Revert \"add-ten\"\n\nThis reverts commit <hashAddTen>.", committer: "same as author", committerTimestamp: ISODate("..."), ok: 1 }
```

Key checks:
- `doltConflicts` returns `collections` array with per-document `conflicts` grouped by collection
- Each conflict entry has `conflictId`, a `type` (`"documentEdit"`), a `reason` (`code` + `message`), and `base` / `ours` / `theirs` sides
- Each non-null side is `{ _id, doc, diffType }`; `_id` lives on the side (no top-level `_id`), `doc` is the full document, and `base` carries no `diffType`. A deleted side is `null`, and `reason.code` (here `"modifyDelete"`) names the clash
- After `doltResolveConflict`, `doltConflicts` returns an empty `collections` array
- After `doltRevert continue:1`, `ok` equals `1` and `commitId` is present

---

## Scenario 5: Abort revert in progress

Start another conflicting revert and then abort it.

```js
// Add a document and then modify it to set up a conflict.
db.records.insertOne({ _id: 20, v: 20 })
const rAdd20 = db.runCommand({ doltCommit: 1, message: "add-twenty", author: "alice <alice@acme.com>" })
const hashAdd20 = rAdd20.commitId

db.records.updateOne({ _id: 20 }, { $set: { v: 201 } })
db.runCommand({ doltCommit: 1, message: "modify-twenty", author: "bob <bob@widgets.io>" })

// Revert  -- expect conflict. The server returns {ok: 0, conflicts: [...], ...}. Mongosh throws this as a MongoServerError.
try {
  db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, commit: hashAdd20 })
} catch (e) {
  // MongoServerError: doltRevert: unresolved conflicts in 1 collection(s)
}

// Abort: restore pre-revert state.
const rAbort = db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, abort: 1 })
printjson(rAbort)
// Expected: { message: "revert aborted", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `message` equals `"revert aborted"`
- `doltConflicts` after abort returns an error (no operation in progress)
- main HEAD is unchanged from before the aborted revert

---

## Scenario 6: Error cases

**Missing commit parameter:**
```js
db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1 })
// Expected: ok: 0, errmsg mentions "commit" or "required"
```

**Abort when no revert in progress:**
```js
db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, abort: 1 })
// Expected: ok: 0, errmsg: "no revert in progress to abort"
```

**Continue when no revert in progress:**
```js
db.getSiblingDB("revertdb@main").runCommand({ doltRevert: 1, continue: 1 })
// Expected: ok: 0, errmsg mentions "no revert in progress"
```
