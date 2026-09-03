# Index Maintenance Verification

Manual verification guide for secondary-index maintenance on the write
path: updates and deletes keep indexes truthful, multikey (array)
indexes adjust per element, and sparse / partial indexes track
membership as documents change. Work through each scenario top to
bottom. Each section builds on the previous setup.

These scenarios verify behaviors W2, W3, W5, M1, and M2 from
`docs/design/secondary-index-structural-sharing.md`.

> **Automated equivalent:** `tests/verify/index_maintenance_test.go`
> (`TestIndexMaintenanceVerify`) covers every scenario in this
> document as sequential subtests using the same setup. Run it with:
> ```
> go test ./tests/... -run TestIndexMaintenanceVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your
instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

## How to read the checks

Three probes appear throughout:

- `db.runCommand({ count: ... })` is answered directly from the index
  when one covers the filter, with no per-document re-check. A stale
  index shows up as a wrong count even when `find` looks right (find
  re-validates each fetched document).
- `db.items.find({...})` returns the documents; compare the `_id`s.
- `db.items.find({...}).explain().queryPlanner.winningPlan` shows the
  plan the query planner chose. A plan of `FETCH -> IXSCAN` with the
  expected `indexName` proves the index is being used; a bare
  `COLLSCAN` means the planner walked the whole collection. `explain`
  answers "is the planner using the index", `count` answers "does the
  index return the right documents" -- a merge or update bug can break
  one without the other, so the scenarios check both.

---

## Setup

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("idxmntvrfy")

db.items.insertMany([
  { _id: 1, name: "alpha",   city: "NYC" },
  { _id: 2, name: "bravo",   city: "LA"  },
  { _id: 3, name: "charlie", city: "NYC" }
])
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.items.createIndex({ city: 1 }, { name: "by_city" })
```

---

## Scenario 1: Update re-indexes the changed field

After `$set` changes an indexed field from "alpha" to "zulu", lookups
by the new value find the document and lookups by the old value find
nothing -- in both `find` and `count`.

```js
db.items.updateOne({ _id: 1 }, { $set: { name: "zulu" } })

db.items.find({ name: "zulu" })
// Expected: one document: { _id: 1, name: "zulu", city: "NYC" }

db.items.find({ name: "alpha" })
// Expected: no documents

db.runCommand({ count: "items", query: { name: "zulu" } })
// Expected: { n: 1, ok: 1 }

db.runCommand({ count: "items", query: { name: "alpha" } })
// Expected: { n: 0, ok: 1 }

// The lookup is served by the by_name index, not a scan.
db.items.find({ name: "zulu" }).explain().queryPlanner.winningPlan
// Expected:
// {
//   stage: "FETCH",
//   inputStage: { stage: "IXSCAN", indexName: "by_name", keyPattern: { name: 1 } }
// }
```

Key checks:
- `find` by the new value returns `_id: 1`; by the old value returns nothing.
- Both counts agree with find. A count of 1 for "alpha" means the
  index still holds the pre-update entry.
- The plan is `FETCH -> IXSCAN(by_name)`. If it shows `COLLSCAN`, the
  re-indexed entry was found by scanning, not through the index.

---

## Scenario 2: updateMany re-indexes every touched document

```js
db.items.updateMany({ city: "NYC" }, { $set: { city: "SF" } })

db.runCommand({ count: "items", query: { city: "SF" } })
// Expected: { n: 2, ok: 1 }   (_id 1 and 3)

db.runCommand({ count: "items", query: { city: "NYC" } })
// Expected: { n: 0, ok: 1 }
```

---

## Scenario 3: Delete removes index entries

```js
db.items.deleteOne({ _id: 2 })

db.items.find({ name: "bravo" })
// Expected: no documents

db.runCommand({ count: "items", query: { name: "bravo" } })
// Expected: { n: 0, ok: 1 }

// Delete by an indexed-field filter (rather than by _id).
db.items.deleteMany({ city: "SF" })
db.runCommand({ count: "items", query: { city: "SF" } })
// Expected: { n: 0, ok: 1 }

db.runCommand({ count: "items", query: {} })
// Expected: { n: 0, ok: 1 }   (all three docs are gone now)
```

---

## Scenario 4: Multikey (array) updates adjust per element

An indexed array field gets one index entry per element. Replacing one
element must fix only that element's entry, and a range query must
return a multi-element document once, not once per matching element.

```js
db.items.insertMany([
  { _id: 10, tags: ["red", "green", "blue"] },
  { _id: 11, tags: ["red"] }
])
db.items.createIndex({ tags: 1 }, { name: "by_tags" })

// Replace one element: green -> yellow.
db.items.updateOne({ _id: 10 }, { $set: { "tags.1": "yellow" } })

db.runCommand({ count: "items", query: { tags: "yellow" } })
// Expected: { n: 1, ok: 1 }
db.runCommand({ count: "items", query: { tags: "green" } })
// Expected: { n: 0, ok: 1 }
db.runCommand({ count: "items", query: { tags: "red" } })
// Expected: { n: 2, ok: 1 }   (_id 10 and 11; kept elements intact)

// Range across elements: _id 10 matches via "red", "yellow", AND
// "blue" but must be returned exactly once.
db.items.find({ tags: { $gt: "a" } })
// Expected: two documents (_id 10 once, _id 11 once)
db.runCommand({ count: "items", query: { tags: { $gt: "a" } } })
// Expected: { n: 2, ok: 1 }

// Both the equality and the range lookup are served by by_tags.
db.items.find({ tags: "yellow" }).explain().queryPlanner.winningPlan
// Expected: { stage: "FETCH", inputStage: { stage: "IXSCAN", indexName: "by_tags", keyPattern: { tags: 1 } } }
db.items.find({ tags: { $gt: "a" } }).explain().queryPlanner.winningPlan
// Expected: { stage: "FETCH", inputStage: { stage: "IXSCAN", indexName: "by_tags", keyPattern: { tags: 1 } } }
```

Key checks:
- The replaced element ("green") stops matching; the new one
  ("yellow") starts; untouched elements still match.
- The range `find` returns `_id: 10` once. Duplicates mean the
  per-element index entries are leaking through.
- Both plans are `FETCH -> IXSCAN(by_tags)`. The range still rides the
  index; the dedup happens above the scan, not by avoiding it.

---

## Scenario 5: Sparse index tracks field presence across updates

A sparse index covers only documents that have the field. Updates that
add or unset the field move the document in and out of the index, and
queries stay truthful throughout.

```js
db.items.insertMany([
  { _id: 20, phone: "555-0100" },
  { _id: 21 }                       // no phone
])
db.items.createIndex({ phone: 1 }, { name: "by_phone", sparse: true })

db.runCommand({ count: "items", query: { phone: "555-0100" } })
// Expected: { n: 1, ok: 1 }

// The field appears on 21...
db.items.updateOne({ _id: 21 }, { $set: { phone: "555-0200" } })
db.runCommand({ count: "items", query: { phone: "555-0200" } })
// Expected: { n: 1, ok: 1 }

// ...and disappears from 20.
db.items.updateOne({ _id: 20 }, { $unset: { phone: "" } })
db.items.find({ phone: "555-0100" })
// Expected: no documents
db.runCommand({ count: "items", query: { phone: "555-0100" } })
// Expected: { n: 0, ok: 1 }

// A sparse index still serves equality lookups on the field.
db.items.find({ phone: "555-0200" }).explain().queryPlanner.winningPlan
// Expected: { stage: "FETCH", inputStage: { stage: "IXSCAN", indexName: "by_phone", keyPattern: { phone: 1 } } }
```

---

## Scenario 6: Partial index tracks its filter across updates

A partial index is keyed on one field but only contains documents
matching its filter expression. Here it is keyed on `sku` and contains
only `status:"active"` documents. Two facts drive the scenario:

- The planner uses the index only for a query that both filters `sku`
  (the key) AND includes `status:"active"` (the partial condition).
  With that condition present, every matching document is guaranteed to
  be in the index, so the scan is sound. Without it -- a `sku`-only
  query -- the planner must decline, because documents with that `sku`
  but `status:"inactive"` live outside the index and would be missed.
- An update that flips `status` moves a document in or out of the
  index, observable through the index-using query.

```js
db.items.insertMany([
  { _id: 30, sku: "A-1", status: "active"   },
  { _id: 31, sku: "B-2", status: "inactive" }
])
db.items.createIndex(
  { sku: 1 },
  { name: "by_sku_partial",
    partialFilterExpression: { status: "active" } }
)

// The covered query (filters sku AND includes the partial condition)
// uses the index and finds the active A-1 doc.
db.items.find({ sku: "A-1", status: "active" }).explain().queryPlanner.winningPlan
// Expected: { stage: "FETCH", inputStage: { stage: "IXSCAN", indexName: "by_sku_partial", keyPattern: { sku: 1 } } }
db.items.find({ sku: "A-1", status: "active" }).toArray().map(d => d._id)
// Expected: [ 30 ]

// The uncovered query (sku only) is DECLINED -- using the partial
// index would miss inactive A-1 docs -- so it scans.
db.items.find({ sku: "A-1" }).explain().queryPlanner.winningPlan
// Expected: { stage: "COLLSCAN" }

// Now flip membership: 30 leaves the filter, 31 enters it.
db.items.updateOne({ _id: 30 }, { $set: { status: "inactive" } })
db.items.updateOne({ _id: 31 }, { $set: { status: "active" } })

// The same index-using query now reflects the new membership: no
// active A-1 doc remains, and B-2 has entered the index.
db.items.find({ sku: "A-1", status: "active" }).toArray().map(d => d._id)
// Expected: [ ]
db.items.find({ sku: "B-2", status: "active" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN(by_sku_partial) under FETCH
db.items.find({ sku: "B-2", status: "active" }).toArray().map(d => d._id)
// Expected: [ 31 ]

// An uncovered query still scans and still returns everything,
// regardless of membership -- the document is not gone, just no longer
// in the partial index.
db.items.find({ sku: "A-1" }).toArray().map(d => d._id)
// Expected: [ 30 ]
```

Key checks:
- The covered query (`sku` + `status:"active"`) uses
  `IXSCAN(by_sku_partial)`; the `sku`-only query is correctly declined
  to `COLLSCAN`. An `IXSCAN` for the `sku`-only query would be a
  correctness bug -- it would silently miss the inactive docs the
  index omits. Both match MongoDB.
- The membership flip is visible through the index-using query: A-1
  drops out, B-2 appears. The document is not deleted -- the `sku`-only
  scan still finds A-1 -- it just left the partial index.

---

## Scenario 7: Cherry-pick an index build, then merge, covers all documents

Two branches. `main` creates documents and then an index over them (a
separate commit). `feature` -- branched before either -- creates its own
unrelated documents, then cherry-picks `main`'s index-creation commit. The
cherry-pick succeeds and builds the index over `feature`'s documents (not
`main`'s). A later merge of the branches is conflict-free and the index
covers every document from both sides.

```js
var db = db.getSiblingDB("idxmntcp")

// Baseline: a seed doc with no "name" field -- the common ancestor.
db.items.insertOne({ _id: 0, tag: "seed" })
db.runCommand({ doltCommit: 1, message: "base seed", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmntcp@main").runCommand({ doltBranch: 1, branch: "feature" })

// main: documents, then an index over them in a separate commit.
db.items.insertMany([{ _id: 1, name: "alpha" }, { _id: 2, name: "bravo" }])
db.runCommand({ doltCommit: 1, message: "main: docs", author: "alice <alice@acme.com>" })
db.items.createIndex({ name: 1 }, { name: "by_name" })
const idxCommit = db.runCommand({ doltCommit: 1, message: "main: create by_name", author: "alice <alice@acme.com>" }).commitId

// feature: different documents (no index yet).
var feat = db.getSiblingDB("idxmntcp@feature")
feat.items.insertMany([{ _id: 10, name: "november" }, { _id: 11, name: "oscar" }])
feat.runCommand({ doltCommit: 1, message: "feature: docs", author: "bob <bob@widgets.io>" })

// Cherry-pick main's index-creation commit onto feature -- it must succeed
// and build the index over feature's documents.
feat.runCommand({ doltCherryPick: 1, commit: idxCommit })
// Expected: { commitId: "...", ok: 1 }

// feature's index holds feature's documents only; main's are not on feature.
feat.items.find({ name: "november" }).toArray()   // Expected: [ { _id: 10, name: "november" } ]
feat.items.find({ name: "alpha" }).toArray()       // Expected: [] (main's docs are not on feature)
feat.items.find({ name: "november" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN with indexName "by_name"

// Merge feature into main: distinct docs, same index -> a clean 3-way merge.
db.getSiblingDB("idxmntcp@main").runCommand({ doltMerge: 1, mergeIn: "feature" })
// Expected: a merge commit, ok: 1 (no conflicts)

// Every document (seed + main + feature) is now in the index.
db.items.find({ name: "alpha" }).toArray()        // Expected: [ { _id: 1, name: "alpha" } ]
db.items.find({ name: "oscar" }).toArray()         // Expected: [ { _id: 11, name: "oscar" } ]
db.runCommand({ count: "items", query: {} })        // Expected: { n: 5, ok: 1 }
db.items.find({ name: "oscar" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN with indexName "by_name"
```

Key checks:
- Cherry-picking an index-creation commit succeeds and builds the index
  over the target branch's documents, not the source branch's.
- `feature`'s index serves `feature`'s documents; `main`'s documents
  (added after the branch) are absent on `feature`.
- Merging the branches is conflict-free; the merged index covers every
  document from both branches.

---

## Scenario 8: Distinct indexes per branch, merge unions both

Two branches over a shared base document. `main` adds documents and an
index on the first field (`name`); `feature` -- branched before either --
adds its own documents and an index on the second field (`city`). Merging
`feature` into `main` unions both the documents and the index definitions:
afterwards `main` carries both indexes, and each one covers every document
from both branches.

```js
var db = db.getSiblingDB("idxmnt2idx")

// Baseline: one document with both fields -- the common ancestor.
db.items.insertOne({ _id: 0, name: "seed", city: "Origin" })
db.runCommand({ doltCommit: 1, message: "base seed", author: "alice <alice@acme.com>" })
db.getSiblingDB("idxmnt2idx@main").runCommand({ doltBranch: 1, branch: "feature" })

// main: documents, then an index on the first field (name).
db.items.insertMany([
  { _id: 1, name: "alpha", city: "NYC" },
  { _id: 2, name: "bravo", city: "LA"  }
])
db.runCommand({ doltCommit: 1, message: "main: docs", author: "alice <alice@acme.com>" })
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.runCommand({ doltCommit: 1, message: "main: create by_name", author: "alice <alice@acme.com>" })

// feature: different documents, then an index on the second field (city).
var feat = db.getSiblingDB("idxmnt2idx@feature")
feat.items.insertMany([
  { _id: 10, name: "november", city: "Boston" },
  { _id: 11, name: "oscar",    city: "Denver" }
])
feat.runCommand({ doltCommit: 1, message: "feature: docs", author: "bob <bob@widgets.io>" })
feat.items.createIndex({ city: 1 }, { name: "by_city" })
feat.runCommand({ doltCommit: 1, message: "feature: create by_city", author: "bob <bob@widgets.io>" })

// Merge feature into main: distinct docs and distinct indexes -> clean merge.
db.getSiblingDB("idxmnt2idx@main").runCommand({ doltMerge: 1, mergeIn: "feature" })
// Expected: a merge commit, ok: 1 (no conflicts)

db.runCommand({ count: "items", query: {} })   // Expected: { n: 5, ok: 1 }

// by_name (created on main) now covers every document, including feature's.
db.items.find({ name: "november" }).toArray()   // Expected: [ { _id: 10, ... } ]
db.items.find({ name: "oscar" }).toArray()       // Expected: [ { _id: 11, ... } ]
db.items.find({ name: "november" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN with indexName "by_name"

// by_city (created on feature) now covers every document, including main's.
db.items.find({ city: "NYC" }).toArray()        // Expected: [ { _id: 1, ... } ]
db.items.find({ city: "LA" }).toArray()          // Expected: [ { _id: 2, ... } ]
db.items.find({ city: "NYC" }).explain().queryPlanner.winningPlan
// Expected: IXSCAN with indexName "by_city"
```

Key checks:
- The merge is conflict-free even though each branch defined its own
  index; both index definitions survive on `main`.
- `by_name`, created on `main`, serves `feature`'s documents after the
  merge; `by_city`, created on `feature`, serves `main`'s documents.
- Each index covers all five documents (seed + main's two + feature's
  two), so both old and new entries are present in each index.

---

## Not verifiable from mongosh

Two contracts from the same design doc are storage-level and have no
wire-visible signal: a no-op update leaving the index root hash
untouched (W4), and chunk-level structural sharing across writes (P2).
They are covered by `internal/backends/dolt/index_write_maintenance_test.go`.
