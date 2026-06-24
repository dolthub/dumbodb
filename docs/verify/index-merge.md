# Index Merge Verification

Manual verification guide for secondary indexes across branch merges
and conflict resolution: merged indexes cover both branches' writes,
an index created on one branch covers the other branch's documents
after merging, a drop wins over data changes, unique-key collisions
between branches become ordinary conflicts, and resolving a conflict
re-indexes the chosen document. Work through each scenario top to
bottom; each scenario uses its own database so they are independent.

These scenarios verify behaviors B2, B4, B5, B6, and C1-C5 from
`docs/design/secondary-index-structural-sharing.md`.

> **Automated equivalent:** `tests/index_merge_verify_test.go`
> (`TestIndexMergeVerify`) covers every scenario in this document as
> subtests using the same setup. Run it with:
> ```
> go test ./tests/... -run TestIndexMergeVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your
instance:

```js
mongosh mongodb://localhost:27017
```

## How to read the checks

Two probes appear throughout:

- `db.runCommand({ count: ... })` answers straight from the index when
  one covers the filter -- a stale merged index shows up as a wrong
  count.
- `db.items.find({...}).explain()` shows whether the plan uses the
  index (`IXSCAN` under `FETCH`) or scans (`COLLSCAN`). Use it when a
  scenario says "the index serves this query" -- a correct result via
  COLLSCAN would mask a missing index.

---

## Scenario 1: Merged index covers both branches' writes

Two branches insert disjoint documents under a shared index; after the
merge, index lookups on the merged branch find both sides' documents.

```js
var db = db.getSiblingDB("idxmrg1")
db.dropDatabase()

db.items.insertOne({ _id: 1, name: "base" })
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.runCommand({ doltCommit: 1, message: "seed + index", author: "alice <alice@acme.com>" })

db.getSiblingDB("idxmrg1@main").runCommand({ doltBranch: 1, branch: "feature" })

// Main writes the a-side.
db.items.insertMany([
  { _id: 10, name: "alpha" },
  { _id: 11, name: "bravo" }
])
db.runCommand({ doltCommit: 1, message: "main: a-side", author: "alice <alice@acme.com>" })

// Feature writes the n-side.
var feat = db.getSiblingDB("idxmrg1@feature")
feat.items.insertMany([
  { _id: 20, name: "november" },
  { _id: 21, name: "oscar" }
])
feat.runCommand({ doltCommit: 1, message: "feature: n-side", author: "bob <bob@widgets.io>" })

// Merge feature into main.
db.getSiblingDB("idxmrg1@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected: { commitId: "...", message: "...", ok: 1 }

// Both sides' docs are found through the index on the merged branch.
db.items.find({ name: "november" })
// Expected: one document: { _id: 20, name: "november" }
db.runCommand({ count: "items", query: { name: "november" } })
// Expected: { n: 1, ok: 1 }
db.runCommand({ count: "items", query: { name: "alpha" } })
// Expected: { n: 1, ok: 1 }

// The index (not a scan) serves these lookups.
db.items.find({ name: "november" }).explain().queryPlanner.winningPlan
// Expected: { stage: "FETCH", inputStage: { stage: "IXSCAN", indexName: "by_name", ... } }
```

Key checks:
- The count for a from-side value ("november") is 1. A count of 0
  with a correct `find` means the merge kept the into-branch's stale
  index and the find fell back to scanning.
- explain shows `IXSCAN` with `indexName: "by_name"`.

---

## Scenario 2: An index created on one branch covers the other's docs

Main creates the index after branching; feature inserts documents that
never saw it. After the merge the index covers feature's documents.

```js
var db = db.getSiblingDB("idxmrg2")
db.dropDatabase()

db.items.insertOne({ _id: 1, city: "base" })
db.runCommand({ doltCommit: 1, message: "seed, no index", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmrg2@main").runCommand({ doltBranch: 1, branch: "feature" })

// Index exists only on main.
db.items.createIndex({ city: 1 }, { name: "by_city" })
db.runCommand({ doltCommit: 1, message: "main: create by_city", author: "alice <alice@acme.com>" })

// Feature inserts without the index.
var feat = db.getSiblingDB("idxmrg2@feature")
feat.items.insertOne({ _id: 20, city: "november" })
feat.runCommand({ doltCommit: 1, message: "feature: november", author: "bob <bob@widgets.io>" })

db.getSiblingDB("idxmrg2@main").runCommand({ doltMerge: 1, merge_in: "feature" })

db.runCommand({ count: "items", query: { city: "november" } })
// Expected: { n: 1, ok: 1 }
db.items.find({ city: "november" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN with indexName "by_city" under FETCH
```

---

## Scenario 3: A drop wins over the other branch's writes

Main drops the index; feature keeps writing documents it would have
covered. After the merge the index stays dropped; queries fall back to
collection scans and remain correct.

```js
var db = db.getSiblingDB("idxmrg3")
db.dropDatabase()

db.items.insertOne({ _id: 1, name: "base" })
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.runCommand({ doltCommit: 1, message: "seed + index", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmrg3@main").runCommand({ doltBranch: 1, branch: "feature" })

db.items.dropIndex("by_name")
db.runCommand({ doltCommit: 1, message: "main: drop by_name", author: "alice <alice@acme.com>" })

var feat = db.getSiblingDB("idxmrg3@feature")
feat.items.insertOne({ _id: 20, name: "november" })
feat.runCommand({ doltCommit: 1, message: "feature: november", author: "bob <bob@widgets.io>" })

db.getSiblingDB("idxmrg3@main").runCommand({ doltMerge: 1, merge_in: "feature" })

db.items.getIndexes().map(i => i.name)
// Expected: [ "_id_" ]   (by_name stays dropped)

db.items.find({ name: "november" })
// Expected: one document -- correctness survives via collection scan
db.items.find({ name: "november" }).explain().queryPlanner.winningPlan.stage
// Expected: "COLLSCAN"
```

---

## Scenario 4: Unique-key collision across branches is a conflict

Each branch inserts a different document with the same unique key. The
merge stops with a conflict (standard conflict workflow); the merged
state keeps ours. Resolving with "theirs" is rejected because it would
recreate the collision; resolving with "ours" completes the merge.

```js
var db = db.getSiblingDB("idxmrg4")
db.dropDatabase()

db.items.insertOne({ _id: 1, sku: "SEED" })
db.items.createIndex({ sku: 1 }, { name: "by_sku", unique: true })
db.runCommand({ doltCommit: 1, message: "seed + unique index", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmrg4@main").runCommand({ doltBranch: 1, branch: "feature" })

// Same unique key, different documents, one per branch.
db.items.insertOne({ _id: 10, sku: "S-1" })
db.runCommand({ doltCommit: 1, message: "main: doc 10 sku S-1", author: "alice <alice@acme.com>" })

var feat = db.getSiblingDB("idxmrg4@feature")
feat.items.insertOne({ _id: 20, sku: "S-1" })
feat.runCommand({ doltCommit: 1, message: "feature: doc 20 sku S-1", author: "bob <bob@widgets.io>" })

// The merge surfaces a conflict (mongosh throws MongoServerError).
db.getSiblingDB("idxmrg4@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected throw:
//   MongoServerError: doltMerge: unresolved conflicts in 1 collection(s)

// Ours owns the key while the merge is in progress.
db.runCommand({ count: "items", query: { sku: "S-1" } })
// Expected: { n: 1, ok: 1 }
db.items.find({ sku: "S-1" })
// Expected: one document: { _id: 10, sku: "S-1" }

// Inspect: the conflict is self-describing. It names the index and the
// colliding key, and carries BOTH contenders with their own _ids.
const rConflicts = db.getSiblingDB("idxmrg4@main").runCommand({ doltConflicts: 1 })
printjson(rConflicts)
// Expected: collections[0].conflicts[0] has
//   type: "uniqueKeyCollision",
//   reason: { code: "uniqueKeyCollision",
//             message: 'unique index "by_sku": ours and theirs both have sku = "S-1"',
//             index: "by_sku", key: { sku: "S-1" } },
//   base:   null,
//   ours:   { _id: 10, doc: { _id: 10, sku: "S-1" }, diffType: "added" },
//   theirs: { _id: 20, doc: { _id: 20, sku: "S-1" }, diffType: "added" }
const conflictId = rConflicts.collections[0].conflicts[0].conflictId

// Resolving "theirs" would re-create the collision with doc 10:
// rejected, the conflict stays open.
db.getSiblingDB("idxmrg4@main").runCommand({
  doltResolveConflict: 1, collection: "items",
  conflictId: conflictId, resolution: "theirs"
})
// Expected throw: duplicate key error mentioning by_sku

// Resolving "ours" (keep the eviction) succeeds.
db.getSiblingDB("idxmrg4@main").runCommand({
  doltResolveConflict: 1, collection: "items",
  conflictId: conflictId, resolution: "ours"
})
// Expected: { ok: 1 }

db.getSiblingDB("idxmrg4@main").runCommand({ doltMerge: 1, continue: 1 })
// Expected: { commitId: "...", ok: 1 }

db.runCommand({ count: "items", query: { sku: "S-1" } })
// Expected: { n: 1, ok: 1 }   (doc 10 owns the key)
```

Key checks:
- The merge does not explode and does not silently keep both
  documents; it parks the loser as a conflict.
- The conflict names the offending index and colliding key, and shows
  both contending documents with their own _ids.
- The "theirs" resolution is rejected with a duplicate-key error and
  the conflict remains resolvable.
- After continue, exactly one document carries the key.

---

## Scenario 4b: Two unique indexes produce two independent collisions

Two unique indexes; one pair of documents collides on each. The merge
surfaces exactly two conflicts, one per index, each naming its own index
and resolvable independently.

```js
var db = db.getSiblingDB("idxmrg4b")
db.dropDatabase()

db.items.insertOne({ _id: 1, sku: "SEED", code: "SEED" })
db.items.createIndex({ sku: 1 }, { name: "by_sku", unique: true })
db.items.createIndex({ code: 1 }, { name: "by_code", unique: true })
db.runCommand({ doltCommit: 1, message: "seed + two unique indexes", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmrg4b@main").runCommand({ doltBranch: 1, branch: "feature" })

// One pair will collide on by_sku, a separate pair on by_code.
db.items.insertOne({ _id: 10, sku: "S-1", code: "K-10" })
db.items.insertOne({ _id: 11, sku: "S-11", code: "C-1" })
db.runCommand({ doltCommit: 1, message: "main: docs 10,11", author: "alice <alice@acme.com>" })

var feat = db.getSiblingDB("idxmrg4b@feature")
feat.items.insertOne({ _id: 20, sku: "S-1", code: "K-20" })
feat.items.insertOne({ _id: 21, sku: "S-21", code: "C-1" })
feat.runCommand({ doltCommit: 1, message: "feature: docs 20,21", author: "bob <bob@widgets.io>" })

db.getSiblingDB("idxmrg4b@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected throw: unresolved conflicts in 1 collection(s)

const r = db.getSiblingDB("idxmrg4b@main").runCommand({ doltConflicts: 1 })
printjson(r)
// Expected: collections[0].conflicts has length 2; one entry with
//   reason.index "by_sku" (ours._id 10, theirs._id 20) and one with
//   reason.index "by_code" (ours._id 11, theirs._id 21).

// Each collision resolves independently with "ours".
r.collections[0].conflicts.forEach(function (c) {
  db.getSiblingDB("idxmrg4b@main").runCommand({
    doltResolveConflict: 1, collection: "items",
    conflictId: c.conflictId, resolution: "ours"
  })
})
db.getSiblingDB("idxmrg4b@main").runCommand({ doltMerge: 1, continue: 1 })
// Expected: { commitId: "...", ok: 1 }

db.items.find({ sku: "S-1" })   // Expected: only { _id: 10, ... }
db.items.find({ code: "C-1" })  // Expected: only { _id: 11, ... }
```

Key checks:
- A document pair colliding on a different index from another pair
  yields one conflict per index, not one merged conflict.
- Each conflict carries its own conflictId and reason.index and is
  resolvable on its own.

---

## Scenario 5: Resolving a conflict re-indexes the chosen document

A divergent edit to an indexed field becomes a conflict. Whatever
resolution lands ("theirs" here, then a custom value in a second
conflict) is what index lookups must see after the merge commits.

```js
var db = db.getSiblingDB("idxmrg5")
db.dropDatabase()

db.items.insertMany([
  { _id: 1, name: "alpha" },
  { _id: 2, name: "bravo" }
])
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.runCommand({ doltCommit: 1, message: "seed + index", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmrg5@main").runCommand({ doltBranch: 1, branch: "feature" })

// Both sides edit the same field of the same docs.
db.items.updateOne({ _id: 1 }, { $set: { name: "ours-1" } })
db.items.updateOne({ _id: 2 }, { $set: { name: "ours-2" } })
db.runCommand({ doltCommit: 1, message: "main: ours", author: "alice <alice@acme.com>" })

var feat = db.getSiblingDB("idxmrg5@feature")
feat.items.updateOne({ _id: 1 }, { $set: { name: "theirs-1" } })
feat.items.updateOne({ _id: 2 }, { $set: { name: "theirs-2" } })
feat.runCommand({ doltCommit: 1, message: "feature: theirs", author: "bob <bob@widgets.io>" })

db.getSiblingDB("idxmrg5@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected throw: unresolved conflicts in 1 collection(s)

const rc = db.getSiblingDB("idxmrg5@main").runCommand({ doltConflicts: 1 })
const byDocID = {}
rc.collections[0].conflicts.forEach(c => { byDocID[c._id] = c.conflictId })

// Doc 1: take theirs. Doc 2: take a custom value.
db.getSiblingDB("idxmrg5@main").runCommand({
  doltResolveConflict: 1, collection: "items",
  conflictId: byDocID[1], resolution: "theirs"
})
db.getSiblingDB("idxmrg5@main").runCommand({
  doltResolveConflict: 1, collection: "items",
  conflictId: byDocID[2], resolution: "custom",
  value: { _id: 2, name: "custom-2" }
})
db.getSiblingDB("idxmrg5@main").runCommand({ doltMerge: 1, continue: 1 })

// Lookups see exactly the resolved values.
db.runCommand({ count: "items", query: { name: "theirs-1" } })
// Expected: { n: 1, ok: 1 }
db.runCommand({ count: "items", query: { name: "custom-2" } })
// Expected: { n: 1, ok: 1 }
for (const stale of ["alpha", "bravo", "ours-1", "ours-2", "theirs-2"]) {
  print(stale, db.runCommand({ count: "items", query: { name: stale } }).n)
}
// Expected: 0 for every stale value
```

Key checks:
- The custom resolution's value is findable through the index; every
  superseded value (base, ours, and the not-chosen theirs) counts 0.
- A non-zero count for a stale value means resolution updated the
  primary document but not the index.
