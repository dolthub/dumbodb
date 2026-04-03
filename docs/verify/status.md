# dongoStatus Verification

Manual verification guide for `dongoStatus` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

> **Automated equivalent:** `tests/versioning_status_verify_test.go` (`TestStatusVerify`)
> covers every scenario in this document as sequential subtests using the same setup.
> Run it with:
> ```
> go test ./tests/... -run TestStatusVerify -v
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
var db = db.getSiblingDB("statusdb")
db.dropDatabase()

// Baseline: one document in "items", committed.
db.items.insertOne({ _id: 1, label: "alpha", score: 10 })
db.runCommand({ dongoCommit: 1, message: "baseline", author: "alice <alice@dongo>" })
```

After setup, the working set matches HEAD — no uncommitted changes.

---

## Scenario 1: Status on clean repo — empty tables

After committing, the working set matches HEAD. `dongoStatus` reports no changed
collections.

```js
db.runCommand({ dongoStatus: 1 })
```

Expected:

```json
{ "branch": "main", "tables": [], "ok": 1 }
```

Key checks:
- `tables` is an empty array
- No collection appears as changed

---

## Scenario 2: Status after insert — new collection appears as "added"

Inserting into a collection that has no HEAD state marks it as `"added"`.

```js
db.newcoll.insertOne({ _id: 1, v: "new" })

db.runCommand({ dongoStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "tables": [
    { "name": "newcoll", "status": "added" }
  ],
  "ok": 1
}
```

Key checks:
- `newcoll` appears with status `"added"`
- `items` does **not** appear (it was not changed)

---

## Scenario 3: Status after update — modified collection appears as "modified"

After committing the previous working set, modifying a committed collection
marks it as `"modified"`.

```js
// Commit the "newcoll" addition first.
db.runCommand({ dongoCommit: 1, message: "add newcoll", author: "alice <alice@dongo>" })

// Now modify an existing committed collection.
db.items.updateOne({ _id: 1 }, { $set: { score: 99 } })

db.runCommand({ dongoStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "tables": [
    { "name": "items", "status": "modified" }
  ],
  "ok": 1
}
```

Key checks:
- `items` appears with status `"modified"`
- `newcoll` does **not** appear (it was committed and unchanged)

---

## Scenario 4: Status after delete — removed collection appears as "deleted"

Deleting all documents from a collection (effectively removing it from the
working set) marks it as `"deleted"`.

```js
// Commit the items modification first.
db.runCommand({ dongoCommit: 1, message: "modify items", author: "alice <alice@dongo>" })

// Delete the entire "items" collection.
db.items.drop()

db.runCommand({ dongoStatus: 1 })
```

Expected:

```json
{
  "branch": "main",
  "tables": [
    { "name": "items", "status": "deleted" }
  ],
  "ok": 1
}
```

Key checks:
- `items` appears with status `"deleted"`
- `newcoll` does **not** appear (unchanged)

---

## Scenario 5: Status after commit — clean again

After committing the deletion, the working set matches HEAD and `dongoStatus`
reports no changes.

```js
db.runCommand({ dongoCommit: 1, message: "delete items", author: "alice <alice@dongo>" })

db.runCommand({ dongoStatus: 1 })
```

Expected:

```json
{ "branch": "main", "tables": [], "ok": 1 }
```

Key checks:
- `tables` is empty — all collections are in sync with HEAD

---

## Quick Reference

| Working-set change vs HEAD | `status` value |
|---|---|
| Collection exists in working set but not in HEAD | `"added"` |
| Collection exists in HEAD but not in working set | `"deleted"` |
| Collection exists in both but content differs | `"modified"` |
| Collection is identical in both | *(not reported)* |

- Only collections with changes appear in `tables`.
- The `branch` field reflects the connection's active branch.
- `tables` is always an array (empty when there are no changes).
