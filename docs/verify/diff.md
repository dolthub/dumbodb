# dongoDiff Verification

Manual verification guide for `dongoDiff` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_diff_verify_test.go` (`TestDiffVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestDiffVerify -v
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
var db = db.getSiblingDB("diffdb")
db.dropDatabase()

// Baseline: two documents, committed
db.items.insertOne({ _id: 1, label: "alpha", score: 10 })
db.items.insertOne({ _id: 2, label: "beta",  score: 20 })
const r1 = db.runCommand({ dongoCommit: 1, message: "baseline" })
printjson(r1)
// Expected: { hash: "<hashBase>", branch: "main", message: "baseline", ok: 1 }
const hashBase = r1.hash

// Make three working-set changes (NOT committed):
//   _id:3 added
//   _id:1 modified (score 10 → 99)
//   _id:2 deleted
db.items.insertOne({ _id: 3, label: "gamma", score: 30 })
db.items.updateOne({ _id: 1 }, { $set: { score: 99 } })
db.items.deleteOne({ _id: 2 })

print("hashBase =", hashBase)
```

After setup, the working set differs from `hashBase` in three ways:
- `_id:3` added
- `_id:1` modified (`score`: 10 → 99)
- `_id:2` removed

---

## Scenario 1: Working set vs HEAD (default diff)

`dongoDiff` with no `from`/`to` compares HEAD (committed) to the current working set.

```js
db.runCommand({ dongoDiff: 1 })
```

Expected result structure:

```json
{
  "collections": [
    {
      "name": "items",
      "added": [
        { "_id": 3, "label": "gamma", "score": 30 }
      ],
      "removed": [
        { "_id": 2, "label": "beta", "score": 20 }
      ],
      "modified": [
        {
          "_id": 1,
          "diff": [
            { "type": "modified", "path": "$.score", "a": 10, "b": 99 }
          ]
        }
      ]
    }
  ],
  "ok": 1
}
```

Key checks:
- `added` contains exactly `_id:3`
- `removed` contains exactly `_id:2`
- `modified` contains exactly `_id:1` with a `score` field diff (`a: 10`, `b: 99`)
- `label` does not appear in `_id:1`'s diff (it was not changed)

---

## Scenario 2: Commit the working set, then diff two hashes

Commit the changes from the setup, then diff the two commits directly.

```js
const r2 = db.runCommand({ dongoCommit: 1, message: "three changes" })
printjson(r2)
// Expected: { hash: "<hashNew>", branch: "main", message: "three changes", ok: 1 }
const hashNew = r2.hash

db.runCommand({ dongoDiff: 1, from: hashBase, to: hashNew })
```

Expected: same structure as Scenario 1 — the same three changes appear, now between
two committed snapshots rather than HEAD vs working set.

---

## Scenario 3: No changes — diff returns an empty collections array

After committing, the working set matches HEAD. Diffing HEAD vs working set produces
no output.

```js
db.runCommand({ dongoDiff: 1 })
// Expected:
// { "collections": [], "ok": 1 }
```

---

## Scenario 4: `from` only — diff from a specific commit to current working set

Make one more change (do not commit), then diff from `hashBase` to the working set.

```js
db.items.insertOne({ _id: 4, label: "delta", score: 40 })

db.runCommand({ dongoDiff: 1, from: hashBase })
```

Expected: the diff includes all changes relative to `hashBase`:
- `_id:3` added (committed in Scenario 2)
- `_id:4` added (in working set, not committed)
- `_id:2` removed (committed in Scenario 2)
- `_id:1` modified with `score` 10 → 99 (committed in Scenario 2)

---

## Quick Reference

| Command | "a" side | "b" side |
|---|---|---|
| `{ dongoDiff: 1 }` | HEAD (last commit) | working set |
| `{ dongoDiff: 1, from: "<hash>" }` | `<hash>` | working set |
| `{ dongoDiff: 1, from: "<hash>", to: "<hash2>" }` | `<hash>` | `<hash2>` |

- Only collections with at least one change appear in the result.
- `added` and `removed` contain full documents.
- `modified` contains only the changed fields with `a` (old) and `b` (new) values.
- Unchanged fields do not appear in `modified[].diff`.
