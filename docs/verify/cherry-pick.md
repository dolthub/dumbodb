# doltCherryPick Verification

Manual verification guide for `doltCherryPick` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_cherry_pick_verify_test.go` (`TestCherryPickVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCherryPickVerify -v
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
var db = db.getSiblingDB("pickdb")
db.dropDatabase()

// Baseline: one document on main.
db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ doltCommit: 1, message: "initial", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch from main HEAD.
db.getSiblingDB("pickdb@main").runCommand({ doltBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

// Advance feature with a commit we will cherry-pick onto main.
db.getSiblingDB("pickdb@feature").items.insertOne({ _id: 2, v: 2 })
const r2 = db.getSiblingDB("pickdb@feature").runCommand({ doltCommit: 1, message: "add-two", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { commitId: "<hashC2>", branch: "feature", message: "add-two", ok: 1 }
const hashC2 = r2.commitId

print("hashC1 =", hashC1)
print("hashC2 =", hashC2)
```

After setup:
- **main** (HEAD = C1): `items` = `[ { _id: 1, v: 1 } ]`
- **feature** (HEAD = C2): `items` = `[ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]`

---

## Scenario 1: Clean cherry-pick  -- response shape

Cherry-pick the feature commit (adds `_id:2`) onto main. Since main doesn't have `_id:2`
and feature's parent (C1 = main HEAD) is the common base, this applies cleanly.

```js
const rPick1 = db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, commit: hashC2 })
printjson(rPick1)
// Expected: { commitId: "<hashC3>", message: "add-two\n\n(cherry picked from commit <hashC2>)",
//            committer: "<current user>", committerTimestamp: ISODate("..."), ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitId` is a non-empty string (different from `hashC2`)
- `message` contains the original message (`"add-two"`) and the cherry-pick annotation
- `committer` is the person performing the cherry-pick (may differ from the original commit's `author`)
- `committerTimestamp` records when the cherry-pick was applied

> **Note:** For cherry-pick commits, `author` is preserved from the original commit while
> `committer` identifies who applied the cherry-pick. This distinction is visible in `doltLog`.

Verify main now has both documents:

```js
db.items.find({}, { _id: 1, v: 1 }).sort({ _id: 1 }).toArray()
// Expected: [ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]
```

Verify the cherry-pick commit is a single-parent commit (not a merge commit):

```js
const log = db.getSiblingDB("pickdb@main").runCommand({ doltLog: 1, limit: 2 })
printjson(log.commits[0])
// Expected: no "parent2" field (single parent commit)
```

---

## Scenario 2: Custom message and committer

Cherry-pick C2 again (it was already applied but let's test message override with a fresh setup).
To re-test from a clean state without re-running setup, advance feature with another commit:

```js
// Add a third document on feature for this scenario.
db.getSiblingDB("pickdb@feature").items.insertOne({ _id: 3, v: 3 })
const r3 = db.getSiblingDB("pickdb@feature").runCommand({ doltCommit: 1, message: "add-three", author: "carol <carol@startup.dev>" })
const hashC3feat = r3.commitId

// Cherry-pick C3 onto main with a custom message and explicit committer.
const rPick2 = db.getSiblingDB("pickdb@main").runCommand({
  doltCherryPick: 1,
  commit: hashC3feat,
  message: "port: add item three",
  committer: "alice <alice@acme.com>"
})
printjson(rPick2)
// Expected: { commitId: "<hashC4>", message: "port: add item three",
//            author: "carol <carol@startup.dev>", committer: "alice <alice@acme.com>",
//            committerTimestamp: ISODate("..."), ok: 1 }
```

Key checks:
- `message` equals the custom override (`"port: add item three"`)
- `author` is preserved from the original commit (`"carol <carol@startup.dev>"`)
- `committer` is `"alice <alice@acme.com>"` (the explicit committer identity)
- `committer` differs from `author`
- `committerTimestamp` records when the cherry-pick was applied

---

## Scenario 3: Conflict during cherry-pick  -- structured error response

Create conflicting changes on both branches so cherry-pick cannot apply cleanly.

```js
// Modify _id:1 on feature (conflicting with main's version).
db.getSiblingDB("pickdb@feature").items.updateOne({ _id: 1 }, { $set: { v: 99 } })
const r4 = db.getSiblingDB("pickdb@feature").runCommand({ doltCommit: 1, message: "conflict-source", author: "bob <bob@widgets.io>" })
const hashC4feat = r4.commitId

// Modify _id:1 on main too (independently).
db.items.updateOne({ _id: 1 }, { $set: { v: 100 } })
db.runCommand({ doltCommit: 1, message: "conflict-target", author: "alice <alice@acme.com>" })

// Cherry-pick the conflicting feature commit onto main.
// In mongosh, runCommand throws a MongoServerError when ok:0  -- it does NOT return a document.
try {
  db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, commit: hashC4feat })
} catch (e) {
  print(e)
  // MongoServerError: doltCherryPick: unresolved conflicts in 1 collection(s)
}
// The cherry-pick is now staged with conflicts. Continue to Scenario 4 to inspect and resolve.
```

Key checks:
- `runCommand` throws a `MongoServerError` (mongosh surfaces `ok:0` as an exception)
- The error message contains `"doltCherryPick: unresolved conflicts in 1 collection(s)"`
- The cherry-pick state is preserved  -- use `doltConflicts` to inspect (Scenario 4)

Verify that `doltStatus` reflects the in-progress cherry-pick and its conflicts:

```js
const rStatus = db.getSiblingDB("pickdb@main").runCommand({ doltStatus: 1 })
printjson(rStatus)
// Expected:
// {
//   branch: "main",
//   collections: [...],
//   mergeState: "cherry-pick",
//   conflicts: [ { collection: "items", count: 1 } ],
//   ok: 1
// }
```

Key checks:
- `mergeState` equals `"cherry-pick"`
- `conflicts` lists per-collection conflict counts
- These fields are only present because a cherry-pick is in progress

---

## Scenario 4: Inspect and resolve conflicts, then continue

Continuing from Scenario 3 (cherry-pick with conflicts in progress).

> **Same interface as merge.** Cherry-pick conflicts are stored in the same
> `mergeState` struct as merge conflicts (with an internal `isCherryPick` flag).
> `doltConflicts` and `doltResolveConflict` work identically for both operations  --
> only the final continuation command differs (`doltCherryPick continue:1` vs
> `doltMerge continue:1`).

```js
// Step 1: Inspect conflicts  -- returns all conflicts grouped by collection.
const rConflicts = db.getSiblingDB("pickdb@main").runCommand({ doltConflicts: 1 })
printjson(rConflicts)
// Expected:
// {
//   collections: [
//     {
//       collection: "items",
//       conflicts: [
//         { conflictId: "<hex-hash>", _id: 1, base: {...},
//           ours: { v: 100 }, theirs: { v: 99 },
//           ourDiffType: "modified", theirDiffType: "modified" }
//       ]
//     }
//   ],
//   ok: 1
// }
// _id is promoted to top level; ours = main's version (v:100), theirs = cherry-picked version (v:99)

const conflictId = rConflicts.collections[0].conflicts[0].conflictId

// Step 2: Resolve  -- accept "theirs" (the cherry-picked value v:99).
const rResolve = db.getSiblingDB("pickdb@main").runCommand({
  doltResolveConflict: 1,
  collection: "items",
  conflictId: conflictId,
  resolution: "theirs"
})
printjson(rResolve)
// Expected: { ok: 1 }

// Step 3: After resolution, doltConflicts returns an empty collections array.
const rAfter = db.getSiblingDB("pickdb@main").runCommand({ doltConflicts: 1 })
printjson(rAfter)
// Expected: { collections: [], ok: 1 }

// Step 4: Continue the cherry-pick (equivalent to doltMerge continue:1 for merges).
const rContinue = db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, continue: 1 })
printjson(rContinue)
// Expected: { commitId: "<hash>", message: "conflict-source\n\n(cherry picked from commit <hashC4feat>)", committer: "<current user>", committerTimestamp: ISODate("..."), ok: 1 }
```

Key checks:
- `doltConflicts` returns `collections` array with per-document `conflicts` grouped by collection
- Each conflict entry has `conflictId`, `_id` (top-level), `base`, `ours`, `theirs`, `ourDiffType`, `theirDiffType`
- `_id` is promoted to the top level of each conflict entry; `base`/`ours`/`theirs` do not contain `_id`
- After `doltResolveConflict`, `doltConflicts` returns an empty `collections` array
- After `doltCherryPick continue:1`, `ok` equals `1` and `commitId` is present
- main HEAD reflects the resolved document state

---

## Scenario 5: Abort cherry-pick in progress

Start another conflicting cherry-pick and then abort it.

```js
// Create another conflicting commit on feature.
db.getSiblingDB("pickdb@feature").items.updateOne({ _id: 1 }, { $set: { v: 200 } })
const r5 = db.getSiblingDB("pickdb@feature").runCommand({ doltCommit: 1, message: "another-conflict", author: "bob <bob@widgets.io>" })
const hashConflict2 = r5.commitId

// Modify _id:1 on main to create conflict.
db.items.updateOne({ _id: 1 }, { $set: { v: 201 } })
db.runCommand({ doltCommit: 1, message: "another-conflict-target", author: "alice <alice@acme.com>" })

// Cherry-pick  -- expect conflict (throws in mongosh).
try {
  db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, commit: hashConflict2 })
} catch (e) {
  // MongoServerError: doltCherryPick: unresolved conflicts in 1 collection(s)
}

// Abort: restore pre-cherry-pick state.
const rAbort = db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, abort: 1 })
printjson(rAbort)
// Expected: { message: "cherry-pick aborted", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `message` equals `"cherry-pick aborted"`
- `doltConflicts` after abort returns an error (no operation in progress)
- main HEAD is unchanged from before the aborted cherry-pick

---

## Scenario 6: Error cases

**Missing commit parameter:**
```js
db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1 })
// Expected: ok: 0, errmsg mentions "commit" or "required"
```

**Abort when no cherry-pick in progress:**
```js
db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, abort: 1 })
// Expected: ok: 0, errmsg: "no cherry-pick in progress to abort"
```

**Continue when no cherry-pick in progress:**
```js
db.getSiblingDB("pickdb@main").runCommand({ doltCherryPick: 1, continue: 1 })
// Expected: ok: 0, errmsg mentions "no cherry-pick in progress"
```
