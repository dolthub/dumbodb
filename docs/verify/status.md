# doltStatus Verification

Manual verification guide for `doltStatus` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_status_verify_test.go` (`TestStatusVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestStatusVerify -v
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
var db = db.getSiblingDB("statusdb")
db.dropDatabase()

// Baseline: one document in "items", committed.
db.items.insertOne({ _id: 1, label: "alpha", score: 10 })
db.runCommand({ doltCommit: 1, message: "baseline", author: "alice <alice@dumbodb>" })
```

After setup, the working set matches HEAD — no uncommitted changes.

---

## Scenario 1: Status on clean repo — empty collections

After committing, the working set matches HEAD. `doltStatus` reports no changed
collections.

```js
db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{ "branch": "main", "commitId": "<hashBase>", "collections": [], "ok": 1 }
```

Key checks:
- `collections` is an empty array
- `commitId` is present and equals the HEAD commit hash (only shown when workspace is clean)
- No collection appears as changed

---

## Scenario 2: Status after insert — new collection appears as "added" with counts

Inserting into a collection that has no HEAD state marks it as `"added"`. The
per-collection counts report one added document.

```js
db.newcoll.insertOne({ _id: 1, v: "new" })

db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "collections": [
    { "name": "newcoll", "status": "added", "added": 1, "modified": 0, "deleted": 0 }
  ],
  "ok": 1
}
```

Key checks:
- `newcoll` appears with status `"added"` and `added: 1`
- `items` does **not** appear (it was not changed)

---

## Scenario 3: Status after update — modified collection appears as "modified"

After committing the previous working set, modifying a committed collection
marks it as `"modified"` with `modified: 1`.

```js
// Commit the "newcoll" addition first.
db.runCommand({ doltCommit: 1, message: "add newcoll", author: "alice <alice@dumbodb>" })

// Now modify an existing committed collection.
db.items.updateOne({ _id: 1 }, { $set: { score: 99 } })

db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "collections": [
    { "name": "items", "status": "modified", "added": 0, "modified": 1, "deleted": 0 }
  ],
  "ok": 1
}
```

Key checks:
- `items` appears with status `"modified"` and `modified: 1`
- `newcoll` does **not** appear (it was committed and unchanged)

---

## Scenario 4: Status after delete — removed collection appears as "deleted"

Deleting all documents from a collection (effectively removing it from the
working set) marks it as `"deleted"`. The count of removed documents is
reported under `deleted`.

```js
// Commit the items modification first.
db.runCommand({ doltCommit: 1, message: "modify items", author: "alice <alice@dumbodb>" })

// Delete the entire "items" collection.
db.items.drop()

db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "collections": [
    { "name": "items", "status": "deleted", "added": 0, "modified": 0, "deleted": 1 }
  ],
  "ok": 1
}
```

Key checks:
- `items` appears with status `"deleted"` and `deleted: 1` (the one baseline doc)
- `newcoll` does **not** appear (unchanged)

---

## Scenario 5: Status after commit — clean again

After committing the deletion, the working set matches HEAD and `doltStatus`
reports no changes.

```js
db.runCommand({ doltCommit: 1, message: "delete items", author: "alice <alice@dumbodb>" })

db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{ "branch": "main", "collections": [], "ok": 1 }
```

Key checks:
- `collections` is empty — all collections are in sync with HEAD

---

## Scenario 6: Multiple collections changed simultaneously

A single working set can span several collections. Each appears as its own entry
with its own `added`/`modified`/`deleted` counts.

```js
// Commit a baseline with three collections we will then mutate together.
db.orders.insertMany([
  { _id: 1, total: 10 },
  { _id: 2, total: 20 },
  { _id: 3, total: 30 }
])
db.archive.insertOne({ _id: 1, note: "old" })
db.runCommand({ doltCommit: 1, message: "baseline for multi-collection", author: "alice <alice@dumbodb>" })

// Now mutate all three in one working set:
//   orders:  insert 3 new, modify 1, delete 2
//   users:   brand-new collection with 5 inserts
//   archive: drop the entire collection
db.orders.insertMany([
  { _id: 4, total: 40 },
  { _id: 5, total: 50 },
  { _id: 6, total: 60 }
])
db.orders.updateOne({ _id: 1 }, { $set: { total: 999 } })
db.orders.deleteOne({ _id: 2 })
db.orders.deleteOne({ _id: 3 })

db.users.insertMany([
  { _id: 1, name: "user1" }, { _id: 2, name: "user2" }, { _id: 3, name: "user3" },
  { _id: 4, name: "user4" }, { _id: 5, name: "user5" }
])

db.archive.drop()

db.runCommand({ doltStatus: 1 })
```

Expected (entry order may vary):

```json
{
  "branch": "main",
  "collections": [
    { "name": "orders",  "status": "modified", "added": 3, "modified": 1, "deleted": 2 },
    { "name": "users",   "status": "added",    "added": 5, "modified": 0, "deleted": 0 },
    { "name": "archive", "status": "deleted",  "added": 0, "modified": 0, "deleted": 1 }
  ],
  "ok": 1
}
```

Key checks:
- All three collections appear in a single `collections` array
- Counts are independent per collection
- `added` entries report every doc in the working-set collection; `deleted` entries
  report every doc that was at HEAD

---

## Scenario 7: Multi-field modification — one modified doc, not many

Changing several fields in a single document — mixing added fields, modified
fields, and unset fields — still counts as exactly **one** modified document.
doltStatus counts docs, not fields; for field-level detail, use `doltDiff`.

```js
// Commit Scenario 6's state so we have a stable baseline.
db.runCommand({ doltCommit: 1, message: "checkpoint before multi-field", author: "alice <alice@dumbodb>" })

// Modify one "users" doc with changes across several fields.
db.users.updateOne(
  { _id: 1 },
  { $set: { name: "user1-renamed", age: 30 } }
)

db.runCommand({ doltStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "collections": [
    { "name": "users", "status": "modified", "added": 0, "modified": 1, "deleted": 0 }
  ],
  "ok": 1
}
```

Key checks:
- `modified: 1`, regardless of how many fields changed in the doc
- `added` stays 0 — a new field on an existing doc is not a new doc

> **Why fields don't show up here:** `doltStatus` answers "which documents changed?"
> A document with a new field, a renamed field, and a removed field is still the same
> `_id`, so it counts once. To see the individual field-level changes, use `doltDiff`.

---

## Scenario 8: Status on a read-only rootish — clear error

`doltStatus` compares the working set against HEAD, but a connection pinned to a
commit hash or ancestor expression (e.g. `mydb@<hash>` or `mydb@main~1`)
is a read-only snapshot with no working set. The command returns a clear error
rather than silent empty output.

```js
// Commit the previous working set so we have a hash to point at.
const r = db.runCommand({ doltCommit: 1, message: "commit for read-only status", author: "alice <alice@dumbodb>" })
const hash = r.commitId

// Attach to the commit snapshot (read-only rootish).
const snap = db.getSiblingDB("statusdb@" + hash)
snap.runCommand({ doltStatus: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltStatus: no working set
//   (connection is at a specific commit, not a named branch)

// Ancestor expressions are also read-only and produce the same error.
db.getSiblingDB("statusdb@main~1").runCommand({ doltStatus: 1 })
// Expected error (code 96):
//   MongoServerError[OperationFailed]: doltStatus: no working set
//   (connection is at a specific commit, not a named branch)
```

Key checks:
- Error code is **96** (`OperationFailed`)
- Message mentions "no working set" so the caller knows to connect via a branch name
- No server crash, no confusing empty response

> **Rationale:** A read-only snapshot has no working set to compare against, so
> "uncommitted changes" is not a meaningful question. Returning an explicit error
> mirrors how `doltCurrentBranch` behaves on the same rootish forms and keeps the
> contract consistent across versioning commands.

---

## Quick Reference

### Collection-level status

| Working-set change vs HEAD | `status` value |
|---|---|
| Collection exists in working set but not in HEAD | `"added"` |
| Collection exists in HEAD but not in working set | `"deleted"` |
| Collection exists in both but content differs | `"modified"` |
| Collection is identical in both | *(not reported)* |

### Per-collection doc counts

| Count field | Meaning |
|---|---|
| `added` | Docs in the working-set copy but not in the HEAD copy |
| `modified` | Docs present in both copies with different content (any number of field changes counts as 1) |
| `deleted` | Docs in the HEAD copy but not in the working-set copy |

- Only collections with changes appear in `collections`.
- The `branch` field reflects the connection's active branch.
- `collections` is always an array (empty when there are no changes).
- Counts are **document-level**, not field-level. Use `doltDiff` for field-level detail.

### Rootish compatibility

| Rootish form | Example | `doltStatus` |
|---|---|---|
| Branch name | `mydb@main`, `mydb@feature` | ✅ works |
| Commit hash | `mydb@<32-char hash>` | ❌ code 96 "no working set" |
| Ancestor expression | `mydb@main~1` | ❌ code 96 "no working set" |

`doltStatus` is a working-set concept — only writable rootish forms have one.
