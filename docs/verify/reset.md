# doltReset Verification

Manual verification guide for `doltReset` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/reset_test.go` (`TestResetVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestResetVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with two committed snapshots

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("resetdb")
db.dropDatabase()

db.tasks.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ doltCommit: 1, message: "initial", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1, ... }
const hashC1 = r1.commitId

db.tasks.insertOne({ _id: 2, v: 2 })
const r2 = db.runCommand({ doltCommit: 1, message: "add-two", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { commitId: "<hashC2>", branch: "main", message: "add-two", ok: 1, ... }
const hashC2 = r2.commitId

print("hashC1 =", hashC1)
print("hashC2 =", hashC2)
```

After setup:
- **C1** (`hashC1`): `tasks` = `[ { _id: 1, v: 1 } ]`
- **C2** (`hashC2`, HEAD): `tasks` = `[ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]`
- Working set is clean (matches HEAD = C2)

---

## Scenario 1: Soft reset (default)  -- HEAD moves back, working set preserved

Insert an uncommitted document, then soft-reset HEAD to C1. The working-set insert
is preserved; the diff shows both staged changes relative to the new HEAD.

```js
// Add _id:3 to the working set (do NOT commit).
db.tasks.insertOne({ _id: 3, v: 3 })

// Soft reset to hashC1 (no `hard` parameter  -- defaults to false).
const rReset = db.runCommand({ doltReset: 1, to: hashC1 })
printjson(rReset)
// Expected: { commitId: "<hashC1>", ok: 1 }
```

Key checks:
- `commitId` in the response equals `hashC1`

After the reset, HEAD is at C1 (which contains only `_id:1`). The working set is
unchanged and still contains `_id:1`, `_id:2`, and `_id:3`. Diffing HEAD vs the
working set shows two additions:

```js
db.runCommand({ doltDiff: 1 })
```

Expected: `tasks.added` contains both `_id:2` and `_id:3`; `tasks.removed` and
`tasks.modified` are empty.

---

## Scenario 2: Hard reset  -- HEAD and working set both reset to target

Commit the working set from Scenario 1, add another document, then hard-reset back
to C1. Both HEAD and the working set return to the C1 state.

```js
// Commit the current working set (creates C3 with _id:1, 2, 3).
const r3 = db.runCommand({ doltCommit: 1, message: "snapshot", author: "alice <alice@acme.com>" })
const hashC3 = r3.commitId

// Add _id:4 to the working set (uncommitted).
db.tasks.insertOne({ _id: 4, v: 4 })

// Hard reset to hashC1.
const rHard = db.runCommand({ doltReset: 1, to: hashC1, hard: true })
printjson(rHard)
// Expected: { commitId: "<hashC1>", ok: 1 }
```

Key checks:
- `commitId` in the response equals `hashC1`

After the hard reset, both HEAD and the working set reflect the C1 state:

```js
db.runCommand({ doltDiff: 1 })
// Expected: { "collections": [], "ok": 1 }
```

The diff is empty  -- the working set matches HEAD exactly. Only `_id:1` is visible
in the `tasks` collection:

```js
db.tasks.find()
// Expected: exactly one document: { _id: 1, v: 1 }
```

---

## Scenario 3: Soft reset undoes a committed change  -- the reverted commit becomes uncommitted

A soft reset can "undo" a commit: HEAD moves back, but the working tree is untouched,
so the previously committed changes are now visible as uncommitted.

```js
// After Scenario 2: HEAD=C1, working set is clean.
// Insert _id:2 again and commit (creates C4).
db.tasks.insertOne({ _id: 2, v: 2 })
const r4 = db.runCommand({ doltCommit: 1, message: "re-add-two", author: "bob <bob@widgets.io>" })
const hashC4 = r4.commitId

// Soft reset to C1  -- this "undoes" the C4 commit.
db.runCommand({ doltReset: 1, to: hashC1 })
// Expected: { commitId: "<hashC1>", ok: 1 }
```

After this soft reset:
- HEAD is at C1 (only `_id:1` committed)
- Working set still has `_id:2` (it was committed in C4 but the working tree was not changed)
- `doltDiff` shows `_id:2` as added (uncommitted):

```js
db.runCommand({ doltDiff: 1 })
```

Expected: `tasks.added` contains exactly `_id:2`; `tasks.removed` and `tasks.modified`
are empty.

---

## Scenario 4: Reset to HEAD  -- discard all uncommitted changes

When `to` is omitted, `doltReset` defaults to the current HEAD. This is the
standard "discard all uncommitted changes" operation when combined with `hard: true`.

```js
// After Scenario 3: HEAD=C1, working set is clean.
// Insert _id:5 to the working set (do NOT commit).
db.tasks.insertOne({ _id: 5, v: 5 })

// Verify there is an uncommitted change.
db.runCommand({ doltDiff: 1 })
// Expected: tasks.added contains _id:5

// Hard reset to HEAD (no `to` parameter).
const rHead = db.runCommand({ doltReset: 1, hard: true })
printjson(rHead)
// Expected: { commitId: "<current HEAD hash>", ok: 1 }
```

Key checks:
- `commitId` in the response is the current HEAD hash (unchanged)
- The uncommitted insert of `_id:5` is discarded:

```js
db.runCommand({ doltDiff: 1 })
// Expected: { "collections": [], "ok": 1 }

db.tasks.find()
// Expected: only the documents present in the HEAD commit
```

Finally, a soft reset to HEAD  -- `{ doltReset: 1 }` with neither `to` nor `hard`  --
must have no effect on uncommitted edits: HEAD stays the same and the working tree is
preserved. Introduce an uncommitted insert, then soft-reset to HEAD:

```js
db.tasks.insertOne({ _id: 6, v: 6 })   // uncommitted edit

const rSoft = db.runCommand({ doltReset: 1 })
printjson(rSoft)
// Expected: { commitId: "<current HEAD hash>", ok: 1 }
```

Key checks:
- `commitId` equals the current HEAD hash (unchanged)
- The uncommitted edit survives  -- the working tree is untouched:

```js
db.runCommand({ doltDiff: 1 })
// Expected: tasks.added still contains _id:6

db.tasks.find()
// Expected: the HEAD documents plus the uncommitted _id:6
```

A hard reset to HEAD then discards that same edit, restoring the clean HEAD state:

```js
db.runCommand({ doltReset: 1, hard: true })
db.tasks.find()
// Expected: only the documents present in the HEAD commit (the _id:6 edit is gone)
```

---

## Scenario 5: Relative rootish (HEAD~N, branch~N)

`to` accepts any rootish expression, not just bare commit hashes. `HEAD` and
`HEAD~N` resolve relative to the connection's branch; `<branch>` and
`<branch>~N` resolve to the named branch's HEAD or its Nth ancestor.

```js
// Reset back one commit relative to current HEAD.
db.runCommand({ doltReset: 1, to: "HEAD~1", hard: true })
// Expected: { commitId: "<hash of HEAD's parent>", ok: 1 }

// Reset to the second-most-recent commit on main.
db.runCommand({ doltReset: 1, to: "main~1", hard: true })

// Reset to a branch tip by name.
db.runCommand({ doltReset: 1, to: "main" })
```

Accepted forms for `to`:
- 32-character commit hash (e.g. `f5gnvfbhdj4ipeqsog2pakkd8g53rbrc`)
- Branch name (e.g. `main`, `feature`)
- Tag name
- Relative ancestor expression (`<branch>~N`)
- `HEAD` (alias for the connection's branch HEAD)
- `HEAD~N` (Nth first-parent ancestor of the connection's branch HEAD)

---

## Branch setup (Scenarios 6 and 7)

`doltReset` acts on the branch encoded in the connection (`db@branch`), not on
`main`. Resetting a feature branch must move that branch's HEAD (and, when hard,
its working set); `main` must be left completely untouched.

Both branch scenarios use a fresh database so the two histories are unambiguous.
Run this setup once before each scenario (drop and rebuild for a clean state).

```js
var mdb = db.getSiblingDB("resetbranchdb")
mdb.dropDatabase()

// main: one committed document (M1).
mdb.tasks.insertOne({ _id: 1, v: 1 })
const hashM1 = mdb.runCommand({ doltCommit: 1, message: "main-base", author: "alice <alice@acme.com>" }).commitId

// Create a feature branch from main.
mdb.runCommand({ doltBranch: 1, branch: "feature" })

// Switch to the feature branch and add two commits (F1, F2).
var fdb = db.getSiblingDB("resetbranchdb@feature")
fdb.tasks.insertOne({ _id: 2, v: 2 })
const hashF1 = fdb.runCommand({ doltCommit: 1, message: "feature-one", author: "carol <carol@acme.com>" }).commitId

fdb.tasks.insertOne({ _id: 3, v: 3 })
const hashF2 = fdb.runCommand({ doltCommit: 1, message: "feature-two", author: "carol <carol@acme.com>" }).commitId
```

After setup:
- **main** (`hashM1`): `tasks` = `[ { _id: 1 } ]`
- **feature** (`hashF2`, HEAD): `tasks` = `[ { _id: 1 }, { _id: 2 }, { _id: 3 } ]`

---

## Scenario 6: Soft reset on a non-main branch  -- only the target branch moves

Rebuild the branch setup, then soft-reset the feature branch back to F1. HEAD moves
but the working tree is preserved, so `_id:3` becomes an uncommitted addition. `main`
must be untouched.

```js
const rSoft = fdb.runCommand({ doltReset: 1, to: hashF1 })
printjson(rSoft)
// Expected: { commitId: "<hashF1>", ok: 1 }
```

Key checks:
- `commitId` equals `hashF1`
- The **feature** branch moved to F1 but its working tree is preserved:

```js
fdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashF1
fdb.tasks.find()                                      // Expected: _id:1, _id:2, _id:3 (3 docs)
fdb.runCommand({ doltDiff: 1 })                       // Expected: tasks.added contains exactly _id:3
```

- **main** is untouched: its HEAD is still M1 and only `_id:1` is visible.

```js
mdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashM1 (unchanged)
mdb.tasks.find()                                      // Expected: _id:1 only (1 doc)
```

---

## Scenario 7: Hard reset on a non-main branch  -- only the target branch moves

Rebuild the branch setup, then hard-reset the feature branch back to F1. Both HEAD
and the working set return to the F1 state. `main` must be untouched.

```js
const rHard = fdb.runCommand({ doltReset: 1, to: hashF1, hard: true })
printjson(rHard)
// Expected: { commitId: "<hashF1>", ok: 1 }
```

Key checks:
- `commitId` equals `hashF1`
- The **feature** branch moved: its HEAD is F1 and only `_id:1`, `_id:2` are visible.

```js
fdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashF1
fdb.tasks.find()                                      // Expected: _id:1 and _id:2 only (2 docs)
```

- **main** is untouched: its HEAD is still M1 and only `_id:1` is visible.

```js
mdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashM1 (unchanged)
mdb.tasks.find()                                      // Expected: _id:1 only (1 doc)
```

---

## Scenario 8: Hard reset to a commit on another branch  -- content follows the target

`to` may name any commit in the repo, including one that lives on a different branch.
Resetting `main` to a feature-branch commit moves `main` to that commit; a hard reset
also makes `main`'s content follow the target. The feature branch is untouched.

Rebuild the branch setup, then, on the `main` connection, hard-reset to F1:

```js
const rHard = mdb.runCommand({ doltReset: 1, to: hashF1, hard: true })
printjson(rHard)
// Expected: { commitId: "<hashF1>", ok: 1 }
```

Key checks:
- `commitId` equals `hashF1`
- **main** now follows F1's content, including `_id:2` which was only ever committed
  on the feature branch; `_id:3` (only on F2) is absent, and the working set is clean:

```js
mdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashF1
mdb.tasks.find()                                      // Expected: _id:1 and _id:2 (2 docs)
mdb.runCommand({ doltDiff: 1 })                       // Expected: { collections: [], ok: 1 }
```

- **feature** is untouched: its HEAD is still F2 and all three docs are present.

```js
fdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashF2 (unchanged)
fdb.tasks.find()                                      // Expected: _id:1, _id:2, _id:3 (3 docs)
```

---

## Scenario 9: Soft reset to a commit on another branch  -- diff reflects the gap

A soft reset to a cross-branch commit moves HEAD but preserves `main`'s working tree.
The resulting diff therefore compares the new HEAD against the unchanged working tree.

Rebuild the branch setup, then, on the `main` connection, soft-reset to F1:

```js
const rSoft = mdb.runCommand({ doltReset: 1, to: hashF1 })
printjson(rSoft)
// Expected: { commitId: "<hashF1>", ok: 1 }
```

Key checks:
- `commitId` equals `hashF1`; `main` HEAD is now F1
- `main`'s working tree is preserved at M1 (only `_id:1`), so relative to the new HEAD
  (F1 = `{_id:1, _id:2}`) the working tree is missing `_id:2`  -- it shows as removed:

```js
mdb.tasks.find()               // Expected: _id:1 only (working tree unchanged)
mdb.runCommand({ doltDiff: 1 })
// Expected: tasks.removed contains exactly _id:2; added and modified are empty
```

- **feature** is untouched: its HEAD is still F2 and all three docs are present.

```js
fdb.runCommand({ doltLog: 1 }).commits[0].commitId   // Expected: hashF2 (unchanged)
fdb.tasks.find()                                      // Expected: _id:1, _id:2, _id:3 (3 docs)
```

---

## Quick Reference

| Command | HEAD after | Working set after |
|---|---|---|
| `{ doltReset: 1 }` | unchanged (HEAD) | unchanged |
| `{ doltReset: 1, hard: true }` | unchanged (HEAD) | reset to HEAD state |
| `{ doltReset: 1, to: "<rootish>" }` | `<rootish>` | unchanged |
| `{ doltReset: 1, to: "<rootish>", hard: true }` | `<rootish>` | reset to `<rootish>` state |

- Soft reset (default): moves HEAD, preserves working tree changes.
- Hard reset: moves HEAD **and** resets the working tree to the target state.
- All forms return `{ commitId: "<resolved_hash>", ok: 1 }`.
- `to` is optional; when omitted, the target defaults to the current HEAD.
- `<rootish>` is any commit hash, branch name, tag name, `HEAD`, `HEAD~N`, or `<branch>~N`.
