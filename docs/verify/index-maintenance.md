# Index Maintenance Verification

Manual verification guide for secondary-index maintenance on the write
path: updates and deletes keep indexes truthful, multikey (array)
indexes adjust per element, and sparse / partial indexes track
membership as documents change. Work through each scenario top to
bottom. Each section builds on the previous setup.

These scenarios verify behaviors W2, W3, W5, M1, and M2 from
`docs/design/secondary-index-structural-sharing.md`.

> **Automated equivalent:** `tests/index_maintenance_verify_test.go`
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
db.dropDatabase()

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

## Not verifiable from mongosh

Two contracts from the same design doc are storage-level and have no
wire-visible signal: a no-op update leaving the index root hash
untouched (W4), and chunk-level structural sharing across writes (P2).
They are covered by `internal/backends/dolt/index_write_maintenance_test.go`.
