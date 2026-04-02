# dongoReset Verification

Manual verification guide for `dongoReset` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_reset_verify_test.go` (`TestResetVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestResetVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with two committed snapshots

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("resetdb")
db.dropDatabase()

db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ dongoCommit: 1, message: "initial", author: "alice" })
printjson(r1)
// Expected: { hash: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

db.items.insertOne({ _id: 2, v: 2 })
const r2 = db.runCommand({ dongoCommit: 1, message: "add-two", author: "alice" })
printjson(r2)
// Expected: { hash: "<hashC2>", branch: "main", message: "add-two", ok: 1 }
const hashC2 = r2.commitId

print("hashC1 =", hashC1)
print("hashC2 =", hashC2)
```

After setup:
- **C1** (`hashC1`): `items` = `[ { _id: 1, v: 1 } ]`
- **C2** (`hashC2`, HEAD): `items` = `[ { _id: 1, v: 1 }, { _id: 2, v: 2 } ]`
- Working set is clean (matches HEAD = C2)

---

## Scenario 1: Soft reset (default) — HEAD moves back, working set preserved

Insert an uncommitted document, then soft-reset HEAD to C1. The working-set insert
is preserved; the diff shows both staged changes relative to the new HEAD.

```js
// Add _id:3 to the working set (do NOT commit).
db.items.insertOne({ _id: 3, v: 3 })

// Soft reset to hashC1 (no `hard` parameter — defaults to false).
const rReset = db.runCommand({ dongoReset: 1, to: hashC1 })
printjson(rReset)
// Expected: { hash: "<hashC1>", ok: 1 }
```

Key checks:
- `hash` in the response equals `hashC1`

After the reset, HEAD is at C1 (which contains only `_id:1`). The working set is
unchanged and still contains `_id:1`, `_id:2`, and `_id:3`. Diffing HEAD vs the
working set shows two additions:

```js
db.runCommand({ dongoDiff: 1 })
```

Expected: `items.added` contains both `_id:2` and `_id:3`; `items.removed` and
`items.modified` are empty.

---

## Scenario 2: Hard reset — HEAD and working set both reset to target

Commit the working set from Scenario 1, add another document, then hard-reset back
to C1. Both HEAD and the working set return to the C1 state.

```js
// Commit the current working set (creates C3 with _id:1, 2, 3).
const r3 = db.runCommand({ dongoCommit: 1, message: "snapshot", author: "alice" })
const hashC3 = r3.commitId

// Add _id:4 to the working set (uncommitted).
db.items.insertOne({ _id: 4, v: 4 })

// Hard reset to hashC1.
const rHard = db.runCommand({ dongoReset: 1, to: hashC1, hard: true })
printjson(rHard)
// Expected: { hash: "<hashC1>", ok: 1 }
```

Key checks:
- `hash` in the response equals `hashC1`

After the hard reset, both HEAD and the working set reflect the C1 state:

```js
db.runCommand({ dongoDiff: 1 })
// Expected: { "collections": [], "ok": 1 }
```

The diff is empty — the working set matches HEAD exactly. Only `_id:1` is visible
in the `items` collection:

```js
db.items.find()
// Expected: exactly one document: { _id: 1, v: 1 }
```

---

## Scenario 3: Soft reset undoes a committed change — the reverted commit becomes uncommitted

A soft reset can "undo" a commit: HEAD moves back, but the working tree is untouched,
so the previously committed changes are now visible as uncommitted.

```js
// After Scenario 2: HEAD=C1, working set is clean.
// Insert _id:2 again and commit (creates C4).
db.items.insertOne({ _id: 2, v: 2 })
const r4 = db.runCommand({ dongoCommit: 1, message: "re-add-two", author: "alice" })
const hashC4 = r4.commitId

// Soft reset to C1 — this "undoes" the C4 commit.
db.runCommand({ dongoReset: 1, to: hashC1 })
// Expected: { hash: "<hashC1>", ok: 1 }
```

After this soft reset:
- HEAD is at C1 (only `_id:1` committed)
- Working set still has `_id:2` (it was committed in C4 but the working tree was not changed)
- `dongoDiff` shows `_id:2` as added (uncommitted):

```js
db.runCommand({ dongoDiff: 1 })
```

Expected: `items.added` contains exactly `_id:2`; `items.removed` and `items.modified`
are empty.

---

## Quick Reference

| Command | HEAD after | Working set after |
|---|---|---|
| `{ dongoReset: 1, to: "<hash>" }` | `<hash>` | unchanged |
| `{ dongoReset: 1, to: "<hash>", hard: true }` | `<hash>` | reset to `<hash>` state |

- Soft reset (default): moves HEAD, preserves working tree changes.
- Hard reset: moves HEAD **and** resets the working tree to the target state.
- Both forms return `{ hash: "<target_hash>", ok: 1 }`.
- `to` is required and must not be empty (`ErrBadValue` if missing or empty).
