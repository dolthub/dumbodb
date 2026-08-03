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

## Scenario 8: Divergent validators on both branches -- resolve the conflict

Both branches change the **same** collection's validator to different
definitions, then merge. A divergent metadata change is **never silently
dropped**: it surfaces as a **resolvable conflict on the owning collection**
(`items`), resolved through the same `doltConflicts` / `doltResolveConflict`
workflow that document, index, and view conflicts use. The internal catalog
collection is never named.

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

// The merge pauses with a conflict (ok:0).
db.getSiblingDB("valconflict@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0, ... }
```

Inspect the conflict -- it is a `type: "metadata"` entry on `items`, never
`__dumbo_catalog__`:

```js
var main = db.getSiblingDB("valconflict@main")
printjson(main.runCommand({ doltConflicts: 1 }).conflicts)
// Expected: one entry
//   { conflictId: "<hash>", type: "metadata", name: "items",
//     base:   { validator: { age: { $gte: 0  } }, validationLevel: "...", validationAction: "..." },
//     ours:   { validator: { age: { $gte: 21 } }, ..., diffType: "modified" },
//     theirs: { validator: { age: { $gte: 18 } }, ..., diffType: "modified" } }
```

Resolve to theirs (feature's `age >= 18`), then complete the merge:

```js
var cid = main.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
main.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "theirs" })
main.runCommand({ doltMerge: 1, continue: 1 })
// { ..., ok: 1 }

db.runCommand({ listCollections: 1, filter: { name: "items" } }).cursor.firstBatch[0].options.validator
// { age: { $gte: 18 } }   (theirs won)
```

For a **custom** resolution, supply a new validator instead:

```js
// (from a fresh conflict) resolve with your own definition:
main.runCommand({
  doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "custom",
  value: { validator: { age: { $gte: 5 } } }
})
main.runCommand({ doltMerge: 1, continue: 1 })
// items' validator is now { age: { $gte: 5 } }
```

Key checks:
- `doltConflicts` reports a single `type: "metadata"` entry on `items`; the
  string `__dumbo_catalog__` never appears anywhere in the output.
- The entry carries `base` / `ours` / `theirs` validator definitions.
- After resolving and continuing, `items`' validator is the chosen one, and the
  merge completes (`ok: 1`).
