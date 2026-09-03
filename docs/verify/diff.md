# doltDiff Verification

Manual verification guide for `doltDiff` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/verify/diff_test.go` (`TestDiffVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestDiffVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: Create a database with a baseline commit

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("diffdb")

// Baseline: two documents, committed
db.items.insertOne({ _id: 1, label: "alpha", score: 10 })
db.items.insertOne({ _id: 2, label: "beta",  score: 20 })
const r1 = db.runCommand({ doltCommit: 1, message: "baseline", author: "alice <alice@acme.com>" })
printjson(r1)
// Expected: { hash: "<hashBase>", branch: "main", message: "baseline", ok: 1 }
const hashBase = r1.commitId

// Make three working-set changes (NOT committed):
//   _id:3 added
//   _id:1 modified (score 10 -> 99)
//   _id:2 deleted
db.items.insertOne({ _id: 3, label: "gamma", score: 30 })
db.items.updateOne({ _id: 1 }, { $set: { score: 99 } })
db.items.deleteOne({ _id: 2 })

print("hashBase =", hashBase)
```

After setup, the working set differs from `hashBase` in three ways:
- `_id:3` added
- `_id:1` modified (`score`: 10 -> 99)
- `_id:2` removed

---

## Scenario 1: Working set vs HEAD (default diff)

`doltDiff` with no `from`/`to` compares HEAD (committed) to the current working set.

```js
db.runCommand({ doltDiff: 1 })
```

Expected result structure:

```json
{
  "changes": [
    {
      "type": "collection",
      "name": "items",
      "status": "modified",
      "documents": {
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
              { "type": "modified", "path": "$.score", "from": 10, "to": 99 }
            ]
          }
        ]
      },
      "indexes": { "added": [], "removed": [], "modified": [] },
      "metadata": {}
    }
  ],
  "ok": 1
}
```

The change set is a single `changes` array, one entry per changed namespace,
each tagged with its `type` (`collection` or `view`) and `status`. A collection
entry groups its detail under `documents` / `indexes` / `metadata` (metadata is
empty unless the collection's validator or validation options changed). A view
entry instead carries `from` / `to` definitions.

Key checks:
- the entry has `type: "collection"` and `status: "modified"` (existed in both sides)
- `documents.added` contains exactly `_id:3`
- `documents.removed` contains exactly `_id:2`
- `documents.modified` contains exactly `_id:1` with a `score` field diff (`from: 10`, `to: 99`)
- `label` does not appear in `_id:1`'s diff (it was not changed)

---

## Scenario 2: Commit the working set, then diff two hashes

Commit the changes from the setup, then diff the two commits directly.

```js
const r2 = db.runCommand({ doltCommit: 1, message: "three changes", author: "bob <bob@widgets.io>" })
printjson(r2)
// Expected: { hash: "<hashNew>", branch: "main", message: "three changes", ok: 1 }
const hashNew = r2.commitId

db.runCommand({ doltDiff: 1, from: hashBase, to: hashNew })
```

Expected: same structure as Scenario 1  -- the same three changes appear, now between
two committed snapshots rather than HEAD vs working set.

---

## Scenario 3: No changes  -- diff returns an empty changes array

After committing, the working set matches HEAD. Diffing HEAD vs working set produces
no output.

```js
db.runCommand({ doltDiff: 1 })
// Expected:
// { "changes": [], "ok": 1 }
```

---

## Scenario 4: `from` only  -- diff from a specific commit to current working set

Make one more change (do not commit), then diff from `hashBase` to the working set.

```js
db.items.insertOne({ _id: 4, label: "delta", score: 40 })

db.runCommand({ doltDiff: 1, from: hashBase })
```

Expected: the diff includes all changes relative to `hashBase`:
- `_id:3` added (committed in Scenario 2)
- `_id:4` added (in working set, not committed)
- `_id:2` removed (committed in Scenario 2)
- `_id:1` modified with `score` 10 -> 99 (committed in Scenario 2)

---

## Scenario 5: Multiple documents with mixed changes

Commit a three-document baseline, then apply different change types to different
documents  -- delete one, modify one, leave one unchanged, add a new one. Only
changed documents appear in the diff.

```js
// Fresh collection: commit 3 docs
db.multi.insertOne({ _id: 1, name: "alpha", v: 1 })
db.multi.insertOne({ _id: 2, name: "beta",  v: 2 })
db.multi.insertOne({ _id: 3, name: "gamma", v: 3 })
db.runCommand({ doltCommit: 1, message: "multi baseline", author: "alice <alice@acme.com>" })

// Working set: delete _id:1, modify _id:2 (v only), leave _id:3, add _id:4
db.multi.deleteOne({ _id: 1 })
db.multi.updateOne({ _id: 2 }, { $set: { v: 99 } })
db.multi.insertOne({ _id: 4, name: "delta", v: 4 })

db.runCommand({ doltDiff: 1 })
```

Expected (for the `multi` collection):
- `removed`: exactly `_id:1`
- `modified`: exactly `_id:2` with `$.v` modified (`a:2`, `b:99`); `$.name` absent
- `added`: exactly `_id:4`
- `_id:3` does **not** appear  -- it was not changed

---

## Scenario 6: Single document with simultaneous modify + add + remove field ops

One `replaceOne` can modify a field, remove a field, and add a new field in a
single operation. The diff must report all three as separate entries.

```js
// Fresh collection: commit a doc with fields x and y
db.mixedfields.insertOne({ _id: 1, x: 10, y: "remove-me" })
db.runCommand({ doltCommit: 1, message: "mixedfields baseline", author: "alice <alice@acme.com>" })

// Replace: x changed, y gone, z added
db.mixedfields.replaceOne({ _id: 1 }, { _id: 1, x: 99, z: "new-field" })

db.runCommand({ doltDiff: 1 })
```

Expected for `_id:1` in the `mixedfields` collection:
- `$.x`: `{ type: "modified", from: 10, to: 99 }`
- `$.y`: `{ type: "removed", from: "remove-me" }`  -- `to` absent
- `$.z`: `{ type: "added", to: "new-field" }`  -- `from` absent
- Exactly **3** diff entries total

---

## Scenario 7: Field type change

Changing a field's value type (e.g. number -> string) is reported as `modified`
with the old typed value in `a` and the new typed value in `b`.

```js
// Fresh collection: commit a doc where "val" is a number
db.typechg.insertOne({ _id: 1, val: 42, stable: "unchanged" })
db.runCommand({ doltCommit: 1, message: "typechg baseline", author: "alice <alice@acme.com>" })

// Replace: val changes from number to string
db.typechg.replaceOne({ _id: 1 }, { _id: 1, val: "forty-two", stable: "unchanged" })

db.runCommand({ doltDiff: 1 })
```

Expected for `_id:1` in the `typechg` collection:
- `$.val`: `{ type: "modified", from: 42, to: "forty-two" }`  -- note different types
- `$.stable` does **not** appear  -- it was not changed

---

## Scenario 8: Nested document field change

When a document contains a sub-document, only the changed leaf fields appear in
the diff. Unchanged nested fields and sibling top-level fields do not appear.
The path uses dot notation: `$.parent.child`.

```js
db.nested.insertOne({
  _id: 1,
  address: { city: "Seattle", zip: "98101" },
  name: "alice"
})
db.runCommand({ doltCommit: 1, message: "nested baseline", author: "alice <alice@acme.com>" })

// Only address.city changes; address.zip and name are unchanged
db.nested.updateOne({ _id: 1 }, { $set: { "address.city": "Portland" } })

db.runCommand({ doltDiff: 1 })
```

Expected for `_id:1` in the `nested` collection:
- `$.address.city`: `{ type: "modified", from: "Seattle", to: "Portland" }`
- `$.address.zip` does **not** appear  -- unchanged
- `$.name` does **not** appear  -- unchanged

---

## Scenario 9: Rootish expressions in `from`/`to`  -- HEAD, HEAD~N, branch name

`from` and `to` accept any valid rootish expression, not just raw commit hashes.
`HEAD` resolves to the committed tip of the connection's own branch (i.e. whatever
branch or snapshot is encoded in the database name). `HEAD~N` resolves to N ancestors
above that commit. Bare branch names are also accepted.

```js
// Create a feature branch from main, then connect to it.
db.getSiblingDB("diffdb@main").runCommand({ doltBranch: 1, branch: "feature" })
var featureDB = db.getSiblingDB("diffdb@feature")

// Make two commits on main.
var r3 = db.runCommand({ doltCommit: 1, message: "c3", author: "alice <alice@acme.com>" })
const hash3 = r3.commitId
db.items.insertOne({ _id: 5, label: "epsilon", score: 50 })
const hash4 = db.runCommand({ doltCommit: 1, message: "c4", author: "bob <bob@widgets.io>" }).commitId

// 1. from=HEAD~1, to=HEAD on a main connection  -- HEAD resolves to main tip (c4)
db.runCommand({ doltDiff: 1, from: "HEAD~1", to: "HEAD" })
```

Expected: diff between c3 and c4  -- only `_id:5` appears as added.

```js
// 2. from=hash3, to="HEAD" on a main connection  -- HEAD = main tip = hash4
db.runCommand({ doltDiff: 1, from: hash3, to: "HEAD" })
```

Expected: same result  -- `_id:5` added.

```js
// 3. from=hash3, to="HEAD" on the feature branch connection  --
//    HEAD resolves to feature tip (= c3, before the two main commits)
featureDB.runCommand({ doltDiff: 1, from: hash3, to: "HEAD" })
```

Expected: `{ "changes": [], "ok": 1 }`  -- feature HEAD equals hash3, no diff.

```js
// 4. REVERSE: from=HEAD, to=HEAD~1  -- swaps direction.
//    _id:5 was added going forward; going backward it appears as removed.
db.runCommand({ doltDiff: 1, from: "HEAD", to: "HEAD~1" })
```

Expected: `_id:5` in `removed` (not `added`)  -- the inverse of case 1.

```js
// 5. REVERSE via branch names: from="main", to="feature".
//    Going main->feature reverses the forward feature->main diff.
db.runCommand({ doltDiff: 1, from: "main", to: "feature" })
```

Expected: `_id:5` in `removed`  -- the inverse of a forward feature->main diff.

Key checks:
- `HEAD` on a main connection resolves to the latest main commit.
- `HEAD` on a feature connection resolves to feature's tip, **not** main.
- `HEAD~N` works as N ancestors above the connection's HEAD.
- Bare branch names (`"main"`, `"feature"`) resolve to that branch's HEAD.
- Reversing `from`/`to` inverts the diff: `added` and `removed` swap roles.

---

## Quick Reference

| Command | `from` side | `to` side |
|---|---|---|
| `{ doltDiff: 1 }` | HEAD (last commit) | working set |
| `{ doltDiff: 1, from: "<hash>" }` | `<hash>` | working set |
| `{ doltDiff: 1, from: "<hash>", to: "<hash2>" }` | `<hash>` | `<hash2>` |
| `{ doltDiff: 1, from: "HEAD~1", to: "HEAD" }` | connection HEAD~1 | connection HEAD |
| `{ doltDiff: 1, from: "HEAD", to: "HEAD~1" }` | connection HEAD | connection HEAD~1 (reverse) |
| `{ doltDiff: 1, from: "branch", to: "HEAD" }` | branch tip | connection HEAD |

- Only namespaces with at least one change appear in the `changes` array.
- Each entry carries a `type` (`collection` or `view`) and a `status` describing
  its lifecycle between the two sides:
  - `"added"`  -- exists in `to` but not in `from` (newly created).
  - `"deleted"`  -- exists in `from` but not in `to` (dropped).
  - `"modified"`  -- exists in both sides with at least one change.
- For a collection entry, `documents.added` / `documents.removed` contain full
  documents; `documents.modified` contains only the changed fields with `from`
  (old) and `to` (new) values; unchanged fields do not appear. `indexes` groups
  index changes the same way. `metadata` carries `{ diff: [...] }` when the
  validator or validation options changed, using the same `{type, path, from, to}`
  field-diff entries rooted at `$.validator`, `$.validationLevel`, and
  `$.validationAction`; a validator change reports the changed leaves inside the
  validator, so its paths continue on into the expression. It is `{}` otherwise.
- `HEAD` always resolves to the connection's own branch tip, not necessarily main.

---

## Scenario 10: Collection-level lifecycle (added / deleted / modified)

A diff result may span multiple collections that experienced different lifecycle
events. The `status` field on each collection entry says what happened to the
collection itself, independent of its document-level diff arrays.

```js
// Baseline commit:
//   db.staying  = [ { _id:1, v:1 } ]
//   db.going    = [ { _id:1 } ]

// Working-set changes (not committed):
db.staying.updateOne({ _id: 1 }, { $set: { v: 2 } })   // modified
db.going.drop()                                        // deleted
db.arrival.insertOne({ _id: 42, label: "new" })        // added

db.runCommand({ doltDiff: 1, from: hashBaseline })
```

Expected:

```json
{
  "changes": [
    {
      "type": "collection",
      "name": "arrival",
      "status": "added",
      "documents": { "added": [ { "_id": 42, "label": "new" } ], "removed": [], "modified": [] },
      "indexes": { "added": [], "removed": [], "modified": [] },
      "metadata": {}
    },
    {
      "type": "collection",
      "name": "going",
      "status": "deleted",
      "documents": { "added": [], "removed": [ { "_id": 1 } ], "modified": [] },
      "indexes": { "added": [], "removed": [], "modified": [] },
      "metadata": {}
    },
    {
      "type": "collection",
      "name": "staying",
      "status": "modified",
      "documents": {
        "added": [], "removed": [],
        "modified": [ { "_id": 1, "diff": [ { "type": "modified", "path": "$.v", "from": 1, "to": 2 } ] } ]
      },
      "indexes": { "added": [], "removed": [], "modified": [] },
      "metadata": {}
    }
  ],
  "ok": 1
}
```

Key checks:
- entries are sorted by `name` (`arrival`, `going`, `staying`).
- `arrival.status == "added"`  -- created since baseline; all docs appear in `documents.added`.
- `going.status == "deleted"`  -- dropped since baseline; all prior docs appear in `documents.removed`.
- `staying.status == "modified"`  -- present in both sides with at least one doc-level change.
