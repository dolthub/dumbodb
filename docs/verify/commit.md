# dongoCommit Verification

Manual verification guide for `dongoCommit` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_commit_verify_test.go` (`TestCommitVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestCommitVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with a baseline commit

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("commitdb")
db.dropDatabase()

// Baseline: two documents, committed
db.items.insertOne({ _id: 1, label: "alpha", v: 1 })
db.items.insertOne({ _id: 2, label: "beta",  v: 2 })
const r1 = db.runCommand({ dongoCommit: 1, message: "baseline" })
printjson(r1)
// Expected: { hash: "<hashBase>", branch: "main", message: "baseline", ok: 1 }
const hashBase = r1.hash

print("hashBase =", hashBase)
```

After setup, `commitdb` has one commit on `main` with two documents.

---

## Scenario 1: Response shape

`dongoCommit` returns `hash`, `branch`, `message`, and `ok`.

The response from setup already demonstrates the shape. Verify each field:

```js
const r = db.runCommand({ dongoCommit: 1, message: "shape check" })
printjson(r)
```

Expected result structure:

```json
{
  "hash":    "<non-empty hex string>",
  "branch":  "main",
  "message": "shape check",
  "ok":      1
}
```

Key checks:
- `hash` is a non-empty string (Dolt commit hash)
- `branch` reflects the connection's current branch (`"main"` for the plain db name)
- `message` echoes the message you provided
- `ok` is `1`

---

## Scenario 2: Commit on a named branch — data is committed to that branch

Create a branch, connect to it, insert a document, commit. Verify that the committed
data is visible on the branch but not on main (isolation check).

```js
// Create branch "feature" from main HEAD
db.getSiblingDB("commitdb__main").runCommand({ dongoBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

var feature = db.getSiblingDB("commitdb__feature")
feature.items.insertOne({ _id: 3, label: "gamma", v: 3 })

const r2 = feature.runCommand({ dongoCommit: 1, message: "feature commit" })
printjson(r2)
// Expected: { hash: "<hash>", branch: "<branch>", message: "feature commit", ok: 1 }

// Verify isolation: feature has the new doc, main does not
feature.items.countDocuments({})
// Expected: 3 (two from setup + _id:3)

db.getSiblingDB("commitdb__main").items.countDocuments({})
// Expected: 2 (feature commit must not affect main)
```

Key checks:
- `hash` is non-empty, `message` echoes `"feature commit"`, `ok` is `1`
- `feature` branch has 3 documents after the commit
- `main` branch still has 2 documents — the feature commit did not affect main

---

## Scenario 3: Successive commits have distinct hashes

Each `dongoCommit` call creates a new, unique commit hash. Two sequential commits
to the same database must return different hash values.

```js
// Make a change and commit
db.items.insertOne({ _id: 10, label: "ten", v: 10 })
const r3a = db.runCommand({ dongoCommit: 1, message: "commit A" })
print("hashA =", r3a.hash)

db.items.insertOne({ _id: 11, label: "eleven", v: 11 })
const r3b = db.runCommand({ dongoCommit: 1, message: "commit B" })
print("hashB =", r3b.hash)

print("hashes differ:", r3a.hash !== r3b.hash)
// Expected: true
```

Key check: `r3a.hash !== r3b.hash`.

---

## Scenario 4: Commit on empty working set

When no changes are pending since the last commit, `dongoCommit` still succeeds.

```js
// No changes since last commit
const r4 = db.runCommand({ dongoCommit: 1, message: "empty" })
printjson(r4)
```

Expected: `{ hash: "<hash>", branch: "main", message: "empty", ok: 1 }`

Key check: `ok` is `1`; `hash` is non-empty.

---

## Scenario 5: Committed hash is a valid diff reference

The hash returned by `dongoCommit` can immediately be used as a `from` or `to`
argument in `dongoDiff`.

```js
// Record state before a change
const hashBefore = db.runCommand({ dongoCommit: 1, message: "pre-change" }).hash

// Make a change and commit
db.items.insertOne({ _id: 99, label: "new", v: 99 })
const hashAfter = db.runCommand({ dongoCommit: 1, message: "post-change" }).hash

// Diff between the two commits — must show _id:99 as added
db.runCommand({ dongoDiff: 1, from: hashBefore, to: hashAfter })
```

Expected: `added` contains exactly `_id:99`; `collections` is non-empty.

---

## Quick Reference

| Scenario | Command | Key outcome |
|---|---|---|
| Commit on main | `{ dongoCommit: 1, message: "msg" }` | `{ hash, branch:"main", message:"msg", ok:1 }` |
| Commit on branch | `featureDB.runCommand({ dongoCommit: 1, ... })` | Data committed to branch; isolation verified via count |
| Two sequential commits | Call twice | Hashes are different |
| Empty working set | Commit with no pending changes | Succeeds with `ok:1` |
| Use hash in diff | `{ dongoDiff: 1, from: hash1, to: hash2 }` | Shows changes between commits |

- `hash` is a Dolt commit hash (non-empty string).
- `branch` reflects the connection's branch, not the database base name.
- `message` is echoed verbatim in the response.
