# docudoltMerge Verification

Manual verification guide for `docudoltMerge` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_merge_verify_test.go` (`TestMergeVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestMergeVerify -v
> ```

## Prerequisites

A running Docudolt instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your Docudolt address if different.

---

## Setup: Create a database with a baseline commit and a feature branch

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("mergedb")
db.dropDatabase()

// Baseline: one document, committed on main.
db.items.insertOne({ _id: 1, v: 1 })
const r1 = db.runCommand({ docudoltCommit: 1, message: "initial", author: "alice <alice@docudolt>" })
printjson(r1)
// Expected: { commitId: "<hashC1>", branch: "main", message: "initial", ok: 1 }
const hashC1 = r1.commitId

// Create "feature" branch from main HEAD.
db.getSiblingDB("mergedb__d_main").runCommand({ docudoltBranch: 1, branch: "feature" })
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
const r2 = db.runCommand({ docudoltCommit: 1, message: "add-two", author: "alice <alice@docudolt>" })
const hashC2 = r2.commitId

// Merge feature (at C1) into main (at C2).
const rMerge1 = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltMerge: 1, merge_in: "feature" })
printjson(rMerge1)
// Expected: { commitId: "<hashC2>", message: "already up-to-date", ok: 1 }
```

Key checks:
- `message` equals `"already up-to-date"`
- `commitId` equals `hashC2` (main's HEAD is unchanged)
- No new commit was created

---

## Scenario 2: Fast-forward merge — bring feature up to date with main

`feature` is behind `main` (still at C1, while main is at C2). Merging `main`
into `feature` advances the feature pointer to main's HEAD without creating a
merge commit — a fast-forward.

```js
// Merge main (at C2) into feature (at C1) — feature fast-forwards.
const rMerge2 = db.getSiblingDB("mergedb__d_feature").runCommand({ docudoltMerge: 1, merge_in: "main" })
printjson(rMerge2)
// Expected: { commitId: "<hashC2>", message: "fast-forward", ok: 1 }
```

Key checks:
- `message` equals `"fast-forward"`
- `commitId` equals `hashC2` (feature's HEAD advanced to main's commit — no new commit)

Verify that `feature` now contains both documents:

```js
db.getSiblingDB("mergedb__d_feature").items.countDocuments({})
// Expected: 2
```

---

## Scenario 3: Already up-to-date — branches are now equal

After the fast-forward in Scenario 2, `feature` and `main` point to the same commit.
Merging either direction produces "already up-to-date".

```js
// feature and main are now both at C2.
const rMerge3 = db.getSiblingDB("mergedb__d_feature").runCommand({ docudoltMerge: 1, merge_in: "main" })
printjson(rMerge3)
// Expected: { commitId: "<hashC2>", message: "already up-to-date", ok: 1 }
```

Key checks:
- `message` equals `"already up-to-date"`
- `commitId` equals `hashC2` (both branches are at C2; nothing to do)

---

## Scenario 4: True three-way merge — diverged branches produce a merge commit

After Scenario 3, both `main` and `feature` are at C2. Commit a new document on
each branch independently, then merge `feature` into `main`. Because neither
branch is an ancestor of the other, a real merge commit is created with two
parents.

```js
// Commit _id:3 on main → C3.
db.items.insertOne({ _id: 3, v: 3 })
const r3 = db.runCommand({ docudoltCommit: 1, message: "add-three", author: "alice <alice@docudolt>" })
const hashC3 = r3.commitId

// Commit _id:4 on feature independently → C4.
// (feature is still at C2; _id:4 is only on feature's side)
db.getSiblingDB("mergedb__d_feature").items.insertOne({ _id: 4, v: 4 })
const r4 = db.getSiblingDB("mergedb__d_feature").runCommand({ docudoltCommit: 1, message: "add-four", author: "alice <alice@docudolt>" })
const hashC4 = r4.commitId

// Merge feature (at C4) into main (at C3) — true three-way merge with custom message/author.
const rMerge4 = db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltMerge: 1,
    merge_in: "feature",
    message: "custom merge msg",
    author: "bob <bob@x>"
})
printjson(rMerge4)
// Expected: { commitId: "<hashM>", message: "custom merge msg", ok: 1 }
```

Key checks:
- `message` equals `"custom merge msg"` (the custom message passed to `docudoltMerge`)
- `commitId` is a new hash — different from both `hashC3` and `hashC4`

Verify the merge commit has two parents and the custom message/author via `docudoltLog`:

```js
const logResult = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltLog: 1, limit: 1 })
printjson(logResult)
// Expected: commits[0].commitId === hashM,
//           commits[0].parent1  === hashC3,
//           commits[0].parent2  === hashC4,
//           commits[0].message  === "custom merge msg",
//           commits[0].author   === "bob <bob@x>"
```

Verify `main` now contains all four documents:

```js
db.getSiblingDB("mergedb__d_main").items.countDocuments({})
// Expected: 4
```

---

---

## Scenario 5: Conflicting merge — both branches modify the same document

When both branches independently modify the same document, `docudoltMerge` cannot
auto-resolve the conflict. The response has `ok: 0` and includes a `conflicts`
array summarising which collections have unresolved conflicts. The branch HEAD
is **not** advanced; the staged working set contains "ours" (current branch)
values for conflicting documents.

```js
// After setup: main modifies _id:1 to v:10, feature modifies _id:1 to v:20.
db.items.updateOne({ _id: 1 }, { $set: { v: 10 } })
db.getSiblingDB("mergedb__d_main").runCommand({ docudoltCommit: 1, message: "main-v10", author: "alice" })

db.getSiblingDB("mergedb__d_feature").items.updateOne({ _id: 1 }, { $set: { v: 20 } })
db.getSiblingDB("mergedb__d_feature").runCommand({ docudoltCommit: 1, message: "feature-v20", author: "bob" })

const rConflict = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltMerge: 1, merge_in: "feature" })
printjson(rConflict)
// Expected: { conflicts: [ { collection: "items", count: 1 } ], ok: 0, code: 96, errmsg: "..." }
```

---

## Scenario 6: Inspect conflicts

```js
// Summary: list which collections have conflicts
const rSummary = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltConflicts: 1 })
printjson(rSummary)
// Expected: { collections: [ { name: "items", count: 1 } ], ok: 1 }

// Detail: list individual conflicts within a collection
const rDetail = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltConflicts: 1, collection: "items" })
printjson(rDetail)
// Expected: { conflicts: [ { conflictId: "c0", base: { _id: 1, v: 1 }, ours: { _id: 1, v: 10 },
//             theirs: { _id: 1, v: 20 }, ourDiffType: "modified", theirDiffType: "modified" } ], ok: 1 }
const conflictId = rDetail.conflicts[0].conflictId
```

Key checks:
- `collections` lists per-collection conflict counts.
- `conflicts` in the per-collection view lists individual document conflicts.
- `base` is the document at the common ancestor (null for new documents).
- `ours` / `theirs` are the two conflicting versions (null for deletions).
- `ourDiffType` / `theirDiffType` are one of `"added"`, `"modified"`, `"deleted"`.

---

## Scenario 7: docudoltCommit rejected while conflicts remain

While a merge is in progress, `docudoltCommit` is always rejected — even once all
conflicts are resolved. Use `docudoltMerge: 1, continue: 1` to finalize.

```js
// (Conflicts still unresolved from Scenario 5/6.)
const rBlockedCommit = db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltCommit: 1,
    message: "should not work",
    author: "alice <alice@docudolt>"
})
printjson(rBlockedCommit)
// Expected: { ok: 0, code: 96, errmsg: "unresolved merge conflicts remain" }
```

Key checks:
- `ok` equals `0`
- `errmsg` mentions unresolved conflicts

---

## Scenario 8: Resolve conflict — ours

```js
// Resolve using our version (v:10).
const rResolve = db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltResolveConflict: 1,
    collection: "items",
    conflictId: conflictId,
    resolution: "ours"
})
printjson(rResolve)
// Expected: { ok: 1 }
```

After resolution, `docudoltConflicts` returns an empty `collections` array.

---

## Scenario 9: Resolve conflict — theirs

```js
// (Re-create a conflict first as shown in Scenario 5.)
// Resolve using their version (v:20).
db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltResolveConflict: 1,
    collection: "items",
    conflictId: conflictId,
    resolution: "theirs"
})
// Expected: { ok: 1 }
```

---

## Scenario 10: Resolve conflict — custom value

```js
// (Re-create a conflict as in Scenario 5.)
// Resolve with a custom merged value.
db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltResolveConflict: 1,
    collection: "items",
    conflictId: conflictId,
    resolution: "custom",
    value: { _id: 1, v: 15 }   // custom resolved value
})
// Expected: { ok: 1 }
```

---

## Scenario 11: Continue after conflict resolution

Once all conflicts are resolved, `docudoltMerge: 1, continue: 1` creates the merge
commit with both branch HEADs as parents. `message` and `author` are optional;
if omitted, Docudolt generates the standard merge message and uses the default author.

`docudoltCommit` is rejected throughout an in-progress merge (whether conflicts
remain or not) — always use `continue` to finalize.

```js
// (All conflicts resolved in Scenario 8/9/10.)
const rContinue = db.getSiblingDB("mergedb__d_main").runCommand({
    docudoltMerge: 1,
    continue: 1,
    message: "Resolve merge conflicts",   // optional
    author: "alice <alice@docudolt>"          // optional
})
printjson(rContinue)
// Expected: { commitId: "<hashM>", branch: "main", message: "Resolve merge conflicts", ok: 1 }
```

`docudoltLog` shows a merge commit with two parents and the custom message/author:

```js
const log = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltLog: 1, limit: 1 })
printjson(log)
// Expected: commits[0].parent1  === <main pre-merge HEAD>,
//           commits[0].parent2  === <feature HEAD>,
//           commits[0].message  === "Resolve merge conflicts",
//           commits[0].author   === "alice <alice@docudolt>"
```

---

## Scenario 12: Abort an in-progress merge

```js
// (Re-create a conflict as in Scenario 5.)
const rAbort = db.getSiblingDB("mergedb__d_main").runCommand({ docudoltMerge: 1, abort: 1 })
printjson(rAbort)
// Expected: { message: "merge aborted", ok: 1 }
```

After abort the branch is back to its pre-merge state and `docudoltCommit` works normally.

---

## State Guards

| State | `docudoltCommit` | `docudoltMerge` (new) | `docudoltMerge continue` |
|---|---|---|---|
| No merge in progress | Normal commit | Normal merge | **Rejected**: "no merge in progress" |
| Merge in progress, conflicts remain | **Rejected**: "unresolved merge conflicts remain" | **Rejected**: "merge already in progress" | **Rejected**: "unresolved merge conflicts remain" |
| Merge in progress, all conflicts resolved | **Rejected**: "merge in progress: use docudoltMerge continue" | **Rejected**: "merge already in progress" | Creates merge commit (two parents) |

---

## Quick Reference

| Situation | `message` in response |
|---|---|
| `merge_in` branch is at or behind `into` branch | `"already up-to-date"` |
| `into` branch is strictly behind `merge_in` branch | `"fast-forward"` |
| Both branches have diverged, no conflicts | `"Merge branch '<merge_in>' into '<into>'"` |
| Both branches have diverged, conflicts exist | `ok: 0` with `conflicts` array |

- `docudoltMerge` always operates on named branches, not raw commit hashes.
- The target branch (`into`) is encoded in the database name: `dbname__d_branch`.
- The `merge_in` parameter names the source branch to merge from.
- Returns `{ commitId: "<result_commitId>", message: "<description>", ok: 1 }` for clean merges.
- For conflicting merges: `{ conflicts: [...], ok: 0, code: 96, errmsg: "..." }`.
- A fast-forward does not create a new commit; the `commitId` in the response is the
  `merge_in` branch's existing HEAD.
- Use `{ docudoltMerge: 1, noFF: true }` to force a merge commit even when fast-forward is possible.
- Use `{ docudoltMerge: 1, ffOnly: true }` to fail if fast-forward is not possible.
- `noFF` and `ffOnly` are mutually exclusive.
- Optional `message` (string) and `author` ('Name <email>') customize the merge commit.
- Use `docudoltConflicts`, `docudoltResolveConflict`, then `{ docudoltMerge: 1, continue: 1 }` to complete a
  conflicting merge.
- `docudoltCommit` is rejected throughout any in-progress merge; use `continue` to finalize.
- Use `{ docudoltMerge: 1, abort: 1 }` to discard an in-progress merge.
