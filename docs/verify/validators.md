# Document Validator Verification

Manual verification guide for collection document validators (`validator`,
`validationLevel`, `validationAction`) -- their enforcement across every write
path and, in particular, their behavior across branch and merge.

Validators are durable and branch-scoped: each collection's validator is stored
in the reserved internal catalog collection, so it survives restart, is carried
by `doltBranch`, and participates in `doltMerge`. That internal collection is
never shown to users -- validators appear only as a collection's `options` in
`listCollections`.

Enforcement matches MongoDB and is covered against the oracle in the parity
harness; the scenarios here focus on the DumboDB-only version-control behavior
(durability, branch, merge), which has no MongoDB analogue.

> **Automated equivalent:** `tests/verify/validator_test.go`
> (`TestValidatorVerify`) covers the runnable scenarios in this document as
> subtests. Run it with:
> ```
> go test ./tests/verify/ -run TestValidatorVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Each scenario uses its own database so they are independent.

## The validator used throughout

Every scenario uses the same simple query-expression validator requiring a
non-negative `age`:

```js
{ age: { $gte: 0 } }
```

A document with `age >= 0` is valid; `age < 0` is invalid. (A `$jsonSchema`
validator behaves identically -- it runs through the same engine.)

---

## Scenario 1: Enforcement on every write path

A validator rejects an invalid document on insert, update, findAndModify, and
bulkWrite -- with `DocumentValidationFailure` (code 121).

```js
var db = db.getSiblingDB("valenf")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } } })

// Valid insert passes; invalid insert is rejected.
db.items.insertOne({ _id: 1, age: 5 })                 // ok
db.items.insertOne({ _id: 2, age: -1 })                // MongoServerError: Document failed validation (121)

// Update that turns the doc invalid is rejected...
db.items.updateOne({ _id: 1 }, { $set: { age: -5 } })  // WriteError 121
// ...a valid update succeeds.
db.items.updateOne({ _id: 1 }, { $set: { age: 9 } })   // ok

// findAndModify to an invalid state is rejected.
db.runCommand({ findAndModify: "items", query: { _id: 1 }, update: { $set: { age: -3 } } })
// { ok: 0, code: 121, ... }
```

Key checks:
- The invalid insert/update/findAndModify are all rejected with code `121`.
- The rejected update did **not** apply (`_id:1` still has `age:9`).

---

## Scenario 2: validationAction "warn" allows the write

With `validationAction: "warn"`, an invalid write is allowed (MongoDB logs a
warning server-side).

```js
var db = db.getSiblingDB("valwarn")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } }, validationAction: "warn" })

db.items.insertOne({ _id: 1, age: 5 })
db.items.updateOne({ _id: 1 }, { $set: { age: -5 } })  // allowed
db.items.findOne({ _id: 1 })                            // { _id: 1, age: -5 }  (write applied)
```

---

## Scenario 3: bypassDocumentValidation skips the validator

`bypassDocumentValidation: true` on a write skips validation for that write only.

```js
var db = db.getSiblingDB("valbypass")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } } })

// Invalid insert allowed with bypass.
db.runCommand({ insert: "items", documents: [ { _id: 1, age: -9 } ], bypassDocumentValidation: true })
// { ok: 1, n: 1 }
```

---

## Scenario 4: validationLevel "moderate" grandfathers invalid documents

`moderate` validates inserts and updates to documents that already satisfied the
validator, but skips updates to documents that were already invalid (e.g. left
behind when a validator is added to an existing collection).

```js
var db = db.getSiblingDB("valmod")
db.dropDatabase()

// Insert BEFORE the validator exists: one soon-to-be-invalid, one valid.
db.items.insertOne({ _id: 1, age: -5 })
db.items.insertOne({ _id: 2, age: 10 })

// Add the validator at moderate level.
db.runCommand({ collMod: "items", validator: { age: { $gte: 0 } }, validationLevel: "moderate", validationAction: "error" })

// Updating the already-invalid doc is ALLOWED (its pre-image was already invalid).
db.items.updateOne({ _id: 1 }, { $set: { note: "x" } })   // ok
// Turning the valid doc invalid is REJECTED.
db.items.updateOne({ _id: 2 }, { $set: { age: -1 } })     // WriteError 121
```

---

## Scenario 5: A validator survives a restart

The validator is durable: after restarting the server, it is still reported by
`listCollections` and still enforced.

```js
var db = db.getSiblingDB("valrestart")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } }, validationLevel: "strict" })
db.items.insertOne({ _id: 2, age: -1 })   // rejected (121)  -- validator active

// >>> Restart the DumboDB server here, then reconnect. <<<

db.getSiblingDB("valrestart").runCommand({ listCollections: 1, filter: { name: "items" } })
// firstBatch[0].options.validator is still { age: { $gte: 0 } }

db.getSiblingDB("valrestart").items.insertOne({ _id: 3, age: -1 })   // still rejected (121)
db.getSiblingDB("valrestart").items.insertOne({ _id: 4, age: 5 })    // ok
```

---

## Scenario 6: Branching carries the validator

A branch inherits the validator; enforcement on the branch is independent of
`main`'s working set.

```js
var db = db.getSiblingDB("valbranch")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } } })
db.runCommand({ doltCommit: 1, message: "create validated items", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// The validator is present on the branch and enforces there.
var feat = db.getSiblingDB("valbranch@feature")
feat.runCommand({ listCollections: 1, filter: { name: "items" } })
// options.validator == { age: { $gte: 0 } }
feat.items.insertOne({ _id: 1, age: -1 })   // rejected (121) on the branch too
```

---

## Scenario 7: Merge carries a validator added on one branch

A validator added (or changed) on one branch, with no competing change on the
other, merges in cleanly.

```js
var db = db.getSiblingDB("valmerge")
db.dropDatabase()
db.createCollection("items")   // no validator yet
db.runCommand({ doltCommit: 1, message: "create items", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature adds the validator.
var feat = db.getSiblingDB("valmerge@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } }, validationLevel: "strict", validationAction: "error" })
feat.runCommand({ doltCommit: 1, message: "feature: add validator", author: "bob <bob@widgets.io>" })

// Main advances independently so the merge is a real 3-way merge.
db.items.insertOne({ _id: 100, age: 1 })
db.runCommand({ doltCommit: 1, message: "main: add a doc", author: "alice <alice@acme.com>" })

// Merge feature into main.
db.runCommand({ doltMerge: 1, merge_in: "feature" })
// { ..., ok: 1 }

// The validator is now active on main and enforces.
db.runCommand({ listCollections: 1, filter: { name: "items" } })
// options.validator == { age: { $gte: 0 } }
db.items.insertOne({ _id: 2, age: -1 })   // rejected (121)
```

Key checks:
- The merge completes (`ok: 1`) and is a real merge commit (both sides diverged).
- `main` now has the validator and rejects an invalid insert.

---

## Scenario 8: Divergent validators on both branches

Both branches change the **same** collection's validator to different
definitions, then merge.

A divergent metadata change is **never silently dropped**. Today the merge is
**refused** loudly -- it fails with an error naming the owning collection
(`items`), and nothing is lost: each branch keeps its own validator. The
internal catalog collection is never named in the error.

> **Pending workspace-xhm.** The hard refusal below is an interim: it guarantees
> no silent data loss, but it is blunt. workspace-xhm upgrades it to a
> **resolvable conflict on the owning collection**, resolved through the same
> `doltConflicts` / `doltResolveConflict` workflow that document, index, and view
> conflicts use -- still never naming the internal catalog.

```js
var db = db.getSiblingDB("valconflict")
db.dropDatabase()
db.createCollection("items", { validator: { age: { $gte: 0 } } })
db.runCommand({ doltCommit: 1, message: "create validated items", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature: require age >= 18.
var feat = db.getSiblingDB("valconflict@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 18 } } })
feat.runCommand({ doltCommit: 1, message: "feature: age >= 18", author: "bob <bob@widgets.io>" })

// Main: require age >= 21.
db.runCommand({ collMod: "items", validator: { age: { $gte: 21 } } })
db.runCommand({ doltCommit: 1, message: "main: age >= 21", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, merge_in: "feature" })
// MongoServerError: cannot merge: collection metadata (validator/options)
//   changed on both branches for items; automatic metadata conflict resolution
//   is not yet supported -- align the metadata on both branches before merging
```

**Today (interim):**
- The merge is refused: the command errors, naming the collection `items` (never
  `__dumbo_catalog__`).
- Nothing is dropped: `main` still has `age >= 21` and `feature` still has
  `age >= 18`. The merge simply did not apply.

**Target (workspace-xhm):**
- The merge stops with a conflict: `doltConflicts` reports a metadata conflict on
  the collection `items`, carrying the base / ours / theirs validator
  definitions -- never the internal catalog name.
- `doltResolveConflict` chooses `ours` / `theirs` / `custom`, then
  `doltMerge continue:1` completes the merge with the chosen validator.

Key checks:
- The merge **fails** (it does not silently succeed).
- The error names `items`, not `__dumbo_catalog__`.
- Both branches' validators are intact afterward -- no side was dropped.
