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

A running DocuDolt instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DocuDolt address if different.

---

## Setup: Create a database with two branches and commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("pickdb")
db.dropDatabase()

// Baseline: one document on main.
db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ doltCommit: 1, message: "initial", author: "alice <alice@docudolt>" })
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch from main HEAD.
db.getSiblingDB("pickdb__d_main").runCommand({ doltBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

// Advance feature with a commit we will cherry-pick onto main.
db.getSiblingDB("pickdb__d_feature").items.insertOne({ _id: 2, v: 2 })
const r2 = db.getSiblingDB("pickdb__d_feature").runCommand({ doltCommit: 1, message: "add-two", author: "bob <bob@docudolt>" })
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

## Scenario 1: Clean cherry-pick — response shape

Cherry-pick the feature commit (adds `_id:2`) onto main. Since main doesn't have `_id:2`
and feature's parent (C1 = main HEAD) is the common base, this applies cleanly.

```js
const rPick1 = db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, commit: hashC2 })
printjson(rPick1)
// Expected: { commitId: "<hashC3>", message: "add-two\n\n(cherry picked from commit <hashC2>)", ok: 1 }
```

Key checks:
- `ok` equals `1`
- `commitId` is a non-empty string (different from `hashC2`)
- `message` contains the original message (`"add-two"`) and the cherry-pick annotation

Verify main now has both documents:

```js
db.items.find({}, { _id: 1, v: 1 }).sort({ _id: 1 }).toArray()
// Expected: [ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]
```

Verify the cherry-pick commit is a single-parent commit (not a merge commit):

```js
const log = db.getSiblingDB("pickdb__d_main").runCommand({ doltLog: 1, limit: 2 })
printjson(log.commits[0])
// Expected: no "parent2" field (single parent commit)
```

---

## Scenario 2: Custom message and author

Cherry-pick C2 again (it was already applied but let's test message override with a fresh setup).
To re-test from a clean state without re-running setup, advance feature with another commit:

```js
// Add a third document on feature for this scenario.
db.getSiblingDB("pickdb__d_feature").items.insertOne({ _id: 3, v: 3 })
const r3 = db.getSiblingDB("pickdb__d_feature").runCommand({ doltCommit: 1, message: "add-three", author: "carol <carol@docudolt>" })
const hashC3feat = r3.commitId

// Cherry-pick C3 onto main with a custom message and author.
const rPick2 = db.getSiblingDB("pickdb__d_main").runCommand({
  doltCherryPick: 1,
  commit: hashC3feat,
  message: "port: add item three",
  author: "alice <alice@docudolt>"
})
printjson(rPick2)
// Expected: { commitId: "<hashC4>", message: "port: add item three", ok: 1 }
```

Key checks:
- `message` equals the custom override (`"port: add item three"`) — no annotation appended
- `commitId` is a fresh hash

---

## Scenario 3: Conflict during cherry-pick — structured error response

Create conflicting changes on both branches so cherry-pick cannot apply cleanly.

```js
// Modify _id:1 on feature (conflicting with main's version).
db.getSiblingDB("pickdb__d_feature").items.updateOne({ _id: 1 }, { $set: { v: 99 } })
const r4 = db.getSiblingDB("pickdb__d_feature").runCommand({ doltCommit: 1, message: "conflict-source" })
const hashC4feat = r4.commitId

// Modify _id:1 on main too (independently).
db.items.updateOne({ _id: 1 }, { $set: { v: 100 } })
db.runCommand({ doltCommit: 1, message: "conflict-target" })

// Cherry-pick the conflicting feature commit onto main.
const rConflict = db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, commit: hashC4feat })
printjson(rConflict)
// Expected: { conflicts: [ { collection: "items", count: 1 } ], ok: 0, code: <N>, errmsg: "doltCherryPick: unresolved conflicts in 1 collection(s)" }
```

Key checks:
- `ok` equals `0`
- `conflicts` is an array with at least one entry
- Each entry has `collection` (string) and `count` (int > 0)
- `errmsg` contains `"doltCherryPick"`

---

## Scenario 4: Inspect and resolve conflicts, then continue

Continuing from Scenario 3 (cherry-pick with conflicts in progress).

```js
// Inspect conflicts.
const rConflicts = db.getSiblingDB("pickdb__d_main").runCommand({ doltConflicts: 1, collection: "items" })
printjson(rConflicts)
// Expected: { conflicts: [ { conflictId: "c0", base: {...}, ours: { _id: 1, v: 100 }, theirs: { _id: 1, v: 99 }, ... } ], ok: 1 }

const conflictId = rConflicts.conflicts[0].conflictId

// Resolve: accept "theirs" (the cherry-picked value).
const rResolve = db.getSiblingDB("pickdb__d_main").runCommand({
  doltResolveConflict: 1,
  collection: "items",
  conflictId: conflictId,
  resolution: "theirs"
})
printjson(rResolve)
// Expected: { ok: 1 }

// Continue the cherry-pick.
const rContinue = db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, continue: 1 })
printjson(rContinue)
// Expected: { commitId: "<hash>", message: "conflict-source\n\n(cherry picked from commit <hashC4feat>)", ok: 1 }
```

Key checks:
- After resolve, `doltConflicts` returns empty conflicts
- After continue, `ok` equals `1` and `commitId` is present
- main HEAD has the resolved document state

---

## Scenario 5: Abort cherry-pick in progress

Start another conflicting cherry-pick and then abort it.

```js
// Create another conflicting commit on feature.
db.getSiblingDB("pickdb__d_feature").items.updateOne({ _id: 1 }, { $set: { v: 200 } })
const r5 = db.getSiblingDB("pickdb__d_feature").runCommand({ doltCommit: 1, message: "another-conflict" })
const hashConflict2 = r5.commitId

// Modify _id:1 on main to create conflict.
db.items.updateOne({ _id: 1 }, { $set: { v: 201 } })
db.runCommand({ doltCommit: 1, message: "another-conflict-target" })

// Cherry-pick — expect conflict.
const rConflict2 = db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, commit: hashConflict2 })
// Expected: ok: 0, conflicts array non-empty

// Abort: restore pre-cherry-pick state.
const rAbort = db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, abort: 1 })
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
db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1 })
// Expected: ok: 0, errmsg mentions "commit" or "required"
```

**Abort when no cherry-pick in progress:**
```js
db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, abort: 1 })
// Expected: ok: 0, errmsg: "no cherry-pick in progress to abort"
```

**Continue when no cherry-pick in progress:**
```js
db.getSiblingDB("pickdb__d_main").runCommand({ doltCherryPick: 1, continue: 1 })
// Expected: ok: 0, errmsg mentions "no cherry-pick in progress"
```
