# dongoMerge Verification

Manual verification guide for `dongoMerge` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_merge_verify_test.go` (`TestMergeVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestMergeVerify -v
> ```

## Prerequisites

A running Dongo instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Dongo address if different.

---

## Setup: Create a database with a baseline commit and a feature branch

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("mergedb")
db.dropDatabase()

// Baseline: one document, committed on main.
db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ dongoCommit: 1, message: "initial", author: "alice <alice@dongo>" })
printjson(r1)
// Expected: { hash: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch from main HEAD.
db.getSiblingDB("mergedb__main").runCommand({ dongoBranch: 1, branch: "feature" })
// Expected: { branch: "feature", ok: 1 }

print("hashC1 =", hashC1)
```

After setup:
- **main** (HEAD = C1): `items` = `[ { _id: 1, v: 1 } ]`
- **feature**: branch pointing at C1 (same as main HEAD)

---

## Scenario 1: Already up-to-date — merge_in branch is behind into branch

Advance `main` past the feature branch point, then try to merge the now-behind
`feature` branch into `main`. Since `feature` is an ancestor of `main`, there is
nothing to merge — the result is "already up-to-date".

```js
// Commit _id:2 on main (feature stays at C1, behind main).
db.items.insertOne({ _id: 2, v: 2 })
const r2 = db.runCommand({ dongoCommit: 1, message: "add-two", author: "alice <alice@dongo>" })
const hashC2 = r2.commitId

// Merge feature (at C1) into main (at C2).
const rMerge1 = db.getSiblingDB("mergedb__main").runCommand({ dongoMerge: 1, merge_in: "feature" })
printjson(rMerge1)
// Expected: { hash: "<hashC2>", message: "already up-to-date", ok: 1 }
```

Key checks:
- `message` equals `"already up-to-date"`
- `hash` equals `hashC2` (main's HEAD is unchanged)
- No new commit was created

---

## Scenario 2: Fast-forward merge — bring feature up to date with main

`feature` is behind `main` (still at C1, while main is at C2). Merging `main`
into `feature` advances the feature pointer to main's HEAD without creating a
merge commit — a fast-forward.

```js
// Merge main (at C2) into feature (at C1) — feature fast-forwards.
const rMerge2 = db.getSiblingDB("mergedb__feature").runCommand({ dongoMerge: 1, merge_in: "main" })
printjson(rMerge2)
// Expected: { hash: "<hashC2>", message: "fast-forward", ok: 1 }
```

Key checks:
- `message` equals `"fast-forward"`
- `hash` equals `hashC2` (feature's HEAD advanced to main's commit — no new commit)

Verify that `feature` now contains both documents:

```js
db.getSiblingDB("mergedb__feature").items.countDocuments({})
// Expected: 2
```

---

## Scenario 3: Already up-to-date — branches are now equal

After the fast-forward in Scenario 2, `feature` and `main` point to the same commit.
Merging either direction produces "already up-to-date".

```js
// feature and main are now both at C2.
const rMerge3 = db.getSiblingDB("mergedb__feature").runCommand({ dongoMerge: 1, merge_in: "main" })
printjson(rMerge3)
// Expected: { hash: "<hashC2>", message: "already up-to-date", ok: 1 }
```

Key checks:
- `message` equals `"already up-to-date"`
- `hash` equals `hashC2` (both branches are at C2; nothing to do)

---

## Quick Reference

| Situation | `message` in response |
|---|---|
| `merge_in` branch is at or behind `into` branch | `"already up-to-date"` |
| `into` branch is strictly behind `merge_in` branch | `"fast-forward"` |
| Both branches have diverged (independent commits on each) | `"Merge branch '<merge_in>' into '<into>'"` |

- `dongoMerge` always operates on named branches, not raw commit hashes.
- The target branch (`into`) is encoded in the database name: `dbname__branch`.
- The `merge_in` parameter names the source branch to merge from.
- Returns `{ hash: "<result_hash>", message: "<description>", ok: 1 }`.
- The `merge_in` parameter is required and must not be empty.
- A fast-forward does not create a new commit; the `hash` in the response is the
  `merge_in` branch's existing HEAD.
