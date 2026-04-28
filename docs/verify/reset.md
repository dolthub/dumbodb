# doltReset Verification

Manual verification guide for `doltReset` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_reset_verify_test.go` (`TestResetVerify`)
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

db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ doltCommit: 1, message: "initial", author: "alice <alice@dumbodb>" })
printjson(r1)
// Expected: { hash: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

db.items.insertOne({ _id: 2, v: 2 })
const r2 = db.runCommand({ doltCommit: 1, message: "add-two", author: "alice <alice@dumbodb>" })
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
const rReset = db.runCommand({ doltReset: 1, to: hashC1 })
printjson(rReset)
// Expected: { hash: "<hashC1>", ok: 1 }
```

Key checks:
- `hash` in the response equals `hashC1`

After the reset, HEAD is at C1 (which contains only `_id:1`). The working set is
unchanged and still contains `_id:1`, `_id:2`, and `_id:3`. Diffing HEAD vs the
working set shows two additions:

```js
db.runCommand({ doltDiff: 1 })
```

Expected: `items.added` contains both `_id:2` and `_id:3`; `items.removed` and
`items.modified` are empty.

---

## Scenario 2: Hard reset — HEAD and working set both reset to target

Commit the working set from Scenario 1, add another document, then hard-reset back
to C1. Both HEAD and the working set return to the C1 state.

```js
// Commit the current working set (creates C3 with _id:1, 2, 3).
const r3 = db.runCommand({ doltCommit: 1, message: "snapshot", author: "alice <alice@dumbodb>" })
const hashC3 = r3.commitId

// Add _id:4 to the working set (uncommitted).
db.items.insertOne({ _id: 4, v: 4 })

// Hard reset to hashC1.
const rHard = db.runCommand({ doltReset: 1, to: hashC1, hard: true })
printjson(rHard)
// Expected: { hash: "<hashC1>", ok: 1 }
```

Key checks:
- `hash` in the response equals `hashC1`

After the hard reset, both HEAD and the working set reflect the C1 state:

```js
db.runCommand({ doltDiff: 1 })
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
const r4 = db.runCommand({ doltCommit: 1, message: "re-add-two", author: "alice <alice@dumbodb>" })
const hashC4 = r4.commitId

// Soft reset to C1 — this "undoes" the C4 commit.
db.runCommand({ doltReset: 1, to: hashC1 })
// Expected: { hash: "<hashC1>", ok: 1 }
```

After this soft reset:
- HEAD is at C1 (only `_id:1` committed)
- Working set still has `_id:2` (it was committed in C4 but the working tree was not changed)
- `doltDiff` shows `_id:2` as added (uncommitted):

```js
db.runCommand({ doltDiff: 1 })
```

Expected: `items.added` contains exactly `_id:2`; `items.removed` and `items.modified`
are empty.

---

## Scenario 4: Reset to HEAD — discard all uncommitted changes

When `to` is omitted, `doltReset` defaults to the current HEAD. This is the
standard "discard all uncommitted changes" operation when combined with `hard: true`.

```js
// After Scenario 3: HEAD=C1, working set is clean.
// Insert _id:5 to the working set (do NOT commit).
db.items.insertOne({ _id: 5, v: 5 })

// Verify there is an uncommitted change.
db.runCommand({ doltDiff: 1 })
// Expected: items.added contains _id:5

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

db.items.find()
// Expected: only the documents present in the HEAD commit
```

A soft reset to HEAD is a no-op in effect (HEAD stays the same, working tree
stays the same), but is valid and returns the HEAD hash.

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
