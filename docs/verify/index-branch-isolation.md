# Index Branch Isolation Verification

Manual verification guide for the contract that secondary index metadata
and data are branch-scoped: the indexes that exist, and the documents
they cover, depend on the branch the connection is pinned to. Work
through each scenario top to bottom. Each section builds on the
previous setup.

> **Automated equivalent:** `tests/index_branch_isolation_verify_test.go`
> (`TestIndexBranchIsolationVerify`) covers every scenario in this
> document as sequential subtests using the same setup. Run it with:
> ```
> go test ./tests/... -run TestIndexBranchIsolationVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your
instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Setup: One collection on main, two branches off it

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("idxisovdb")
db.dropDatabase()

// Seed: one doc on main with name "alpha".
db.items.insertOne({ _id: 1, name: "alpha" })
const r0 = db.runCommand({ doltCommit: 1, message: "seed alpha", author: "alice <alice@acme.com>" })
printjson(r0)

// Branch "am" and "nz" off main.
db.getSiblingDB("idxisovdb@main").runCommand({ doltBranch: 1, branch: "am" })
db.getSiblingDB("idxisovdb@main").runCommand({ doltBranch: 1, branch: "nz" })
```

After setup, `idxisovdb` has:
- **main** (HEAD: 1 doc, no secondary indexes)
- **am** (same commit as main)
- **nz** (same commit as main)

---

## Scenario 1: Create index on one branch, not visible on another

`createIndexes` on `idxisovdb@am` must not affect `listIndexes` results
on `idxisovdb@main` or `idxisovdb@nz`.

```js
// Create by_name index only on am.
var am = db.getSiblingDB("idxisovdb@am")
am.items.createIndex({ name: 1 }, { name: "by_name" })
am.runCommand({ doltCommit: 1, message: "am: create by_name", author: "alice <alice@acme.com>" })

// am sees both indexes.
am.items.getIndexes().map(i => i.name)
// Expected: [ "_id_", "by_name" ]

// main sees only the default index.
db.getSiblingDB("idxisovdb@main").items.getIndexes().map(i => i.name)
// Expected: [ "_id_" ]

// nz also sees only the default index.
db.getSiblingDB("idxisovdb@nz").items.getIndexes().map(i => i.name)
// Expected: [ "_id_" ]
```

Key checks:
- `am` lists `["_id_", "by_name"]`
- `main` lists `["_id_"]`
- `nz` lists `["_id_"]`

---

## Scenario 2: Different index names on different branches

A different index can exist on each branch under a different name; each
branch's `listIndexes` returns only its own.

```js
// Create by_id_name on nz only.
var nz = db.getSiblingDB("idxisovdb@nz")
nz.items.createIndex({ name: 1, _id: 1 }, { name: "by_id_name" })
nz.runCommand({ doltCommit: 1, message: "nz: create by_id_name", author: "alice <alice@acme.com>" })

// nz sees its own index, not am's.
nz.items.getIndexes().map(i => i.name).sort()
// Expected: [ "_id_", "by_id_name" ]

// am still sees by_name and not by_id_name.
db.getSiblingDB("idxisovdb@am").items.getIndexes().map(i => i.name).sort()
// Expected: [ "_id_", "by_name" ]
```

Key checks:
- `nz`: `["_id_", "by_id_name"]`
- `am`: `["_id_", "by_name"]`

---

## Scenario 3: Interleaved inserts, each branch sees only its own data through the index

The user-stated scenario: branch `am` writes strings `alpha..mike`,
branch `nz` writes `november..zulu`, indexed lookups on each branch
return only that branch's data.

```js
var am = db.getSiblingDB("idxisovdb@am")
var nz = db.getSiblingDB("idxisovdb@nz")

// am inserts strings starting with letters a..m (excluding "alpha", _id:1 already there).
const amWords = ["bravo","charlie","delta","echo","foxtrot","golf",
                 "hotel","india","juliet","kilo","lima","mike"]
amWords.forEach((w, i) => am.items.insertOne({ _id: 100 + i, name: w }))
am.runCommand({ doltCommit: 1, message: "am bulk insert", author: "alice <alice@acme.com>" })

// nz inserts strings starting with letters n..z.
const nzWords = ["november","oscar","papa","quebec","romeo","sierra",
                 "tango","uniform","victor","whiskey","xray","yankee","zulu"]
nzWords.forEach((w, i) => nz.items.insertOne({ _id: 200 + i, name: w }))
nz.runCommand({ doltCommit: 1, message: "nz bulk insert", author: "alice <alice@acme.com>" })

// am sees its own words plus the seed.
am.items.find({ name: "mike" }, { _id: 1 }).toArray()
// Expected: [ { _id: 111 } ]

am.items.find({ name: "zulu" }, { _id: 1 }).toArray()
// Expected: []  (nz wrote zulu, not am)

// nz sees its own words.
nz.items.find({ name: "zulu" }, { _id: 1 }).toArray()
// Expected: [ { _id: 212 } ]

nz.items.find({ name: "mike" }, { _id: 1 }).toArray()
// Expected: []  (am wrote mike, not nz)

// main sees only the seed.
db.getSiblingDB("idxisovdb@main").items.find({ name: "alpha" }, { _id: 1 }).toArray()
// Expected: [ { _id: 1 } ]

db.getSiblingDB("idxisovdb@main").items.find({ name: "mike" }, { _id: 1 }).toArray()
// Expected: []

db.getSiblingDB("idxisovdb@main").items.find({ name: "zulu" }, { _id: 1 }).toArray()
// Expected: []
```

Key checks:
- `am` returns its own writes for `name: "mike"`, empty for `name: "zulu"`
- `nz` returns its own writes for `name: "zulu"`, empty for `name: "mike"`
- `main` returns only the seed for `name: "alpha"`, empty for both branch writes

---

## Scenario 4: Update on one branch shifts the indexed lookup only on that branch

```js
var am = db.getSiblingDB("idxisovdb@am")

// Pre-state: on am, name "mike" -> _id 111.
am.items.find({ name: "mike" }, { _id: 1 }).toArray()
// Expected: [ { _id: 111 } ]

// On am, rename mike to "mary".
am.items.updateOne({ _id: 111 }, { $set: { name: "mary" } })
am.runCommand({ doltCommit: 1, message: "am: mike -> mary", author: "alice <alice@acme.com>" })

// am: mike disappears from the index, mary shows up.
am.items.find({ name: "mike" }, { _id: 1 }).toArray()
// Expected: []

am.items.find({ name: "mary" }, { _id: 1 }).toArray()
// Expected: [ { _id: 111 } ]

// nz: nothing changed about nz; its index is unaffected.
var nz = db.getSiblingDB("idxisovdb@nz")
nz.items.find({ name: "mary" }, { _id: 1 }).toArray()
// Expected: []
```

Key checks:
- `am` no longer finds `mike`, finds `mary` for `_id: 111`
- `nz` still finds nothing for `mary` (the update did not cross branches)

---

## Scenario 5: Delete on one branch removes from the index only on that branch

```js
var am = db.getSiblingDB("idxisovdb@am")

// am: delete _id 111 (name "mary" after Scenario 4).
am.items.deleteOne({ _id: 111 })
am.runCommand({ doltCommit: 1, message: "am: delete _id 111", author: "alice <alice@acme.com>" })

// am: mary lookup is now empty.
am.items.find({ name: "mary" }, { _id: 1 }).toArray()
// Expected: []

// nz: still has all its original docs; checking that one specific entry
// is unaffected by an unrelated branch's delete.
var nz = db.getSiblingDB("idxisovdb@nz")
nz.items.find({ name: "zulu" }, { _id: 1 }).toArray()
// Expected: [ { _id: 212 } ]
```

Key checks:
- `am` finds nothing for the deleted doc's name
- `nz` is unaffected

---

## Scenario 6: Drop an index on one branch, the index still exists on the other

```js
var am = db.getSiblingDB("idxisovdb@am")

// Drop by_name on am.
am.items.dropIndex("by_name")
am.runCommand({ doltCommit: 1, message: "am: drop by_name", author: "alice <alice@acme.com>" })

// am sees only _id_.
am.items.getIndexes().map(i => i.name)
// Expected: [ "_id_" ]

// nz still has its own index by_id_name.
var nz = db.getSiblingDB("idxisovdb@nz")
nz.items.getIndexes().map(i => i.name).sort()
// Expected: [ "_id_", "by_id_name" ]
```

Key checks:
- `am`: `["_id_"]`
- `nz`: `["_id_", "by_id_name"]`

---

## Scenario 7: Reopen the server, indexes still per-branch

This scenario requires restarting the DumboDB process. It validates that
indexes on non-default branches survive a reopen. Steps:

1. Disconnect `mongosh`.
2. Restart the DumboDB server pointed at the same `--data-dir`.
3. Reconnect with `mongosh` to the same URL.

```js
var db = db.getSiblingDB("idxisovdb")

// am still has _id_ only (we dropped by_name in Scenario 6).
db.getSiblingDB("idxisovdb@am").items.getIndexes().map(i => i.name)
// Expected: [ "_id_" ]

// nz still has its index.
db.getSiblingDB("idxisovdb@nz").items.getIndexes().map(i => i.name).sort()
// Expected: [ "_id_", "by_id_name" ]

// nz lookup through the index still returns the right doc.
db.getSiblingDB("idxisovdb@nz").items.find({ name: "zulu" }, { _id: 1 }).toArray()
// Expected: [ { _id: 212 } ]
```

Key checks:
- Per-branch index state survives a server restart
- Index lookups on non-default branches return correct results after
  restart, with no warmup step (no eager hydration is required)

---

## Quick Reference

| Operation | Pinned branch | Effect on other branches |
|---|---|---|
| `createIndex({...}, { name: "x" })` | `@am` | `@main` and `@nz` do not see `x` |
| `dropIndex("x")` on `@am` | `@am` | `x` still exists on branches that have it |
| `insertOne({ name: "..." })` on `@am` | `@am` | other branches' index lookups on that name return `[]` |
| `updateOne({ _id }, { $set: { name } })` on `@am` | `@am` | other branches' index views unchanged |
| `deleteOne({ _id })` on `@am` | `@am` | other branches still see the doc through the index |
| Server restart | n/a | per-branch index state and data preserved |

- Index existence is a property of the branch the connection is pinned
  to. Two branches with the same name `by_x` may exist independently
  (potentially with different key shapes); each `listIndexes` returns
  only its own.
- Reads on a pinned branch see only that branch's data through its
  indexes, regardless of what was written on other branches.
- Writes on a pinned branch update only that branch's indexes.
