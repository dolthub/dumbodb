# Document Validator Verification (version-control behavior)

Manual verification guide for the **version-control** behavior of collection
document validators (`validator`, `validationLevel`, `validationAction`):
durability across restart, carry across `doltBranch`, and behavior across
`doltMerge`. These have no MongoDB analogue, so they cannot be checked against
the oracle and are the scenarios worth verifying by hand.

**Enforcement is not covered here.** A validator's rejection of an invalid
insert / update / findAndModify / bulkWrite, `validationAction: "warn"`,
`bypassDocumentValidation`, and `validationLevel` grandfathering all match
MongoDB exactly and are pinned against the oracle in the parity harness
(`dumbodb-parity-testing/tests/validator_enforcement_test.go`). Do not
hand-verify enforcement -- if it regresses, the parity suite fails.

Validators are durable and branch-scoped: each collection's validator is stored
in the reserved internal catalog collection, so it survives restart, is carried
by `doltBranch`, and participates in `doltMerge`. That internal collection is
never shown to users -- validators appear only as a collection's `options` in
`listCollections`.

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

## Scenario 1: A validator survives a restart

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

## Scenario 2: Branching carries the validator

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

## Scenario 3: Merge carries a validator added on one branch

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

// Main advances independently so the merge is a real 3-way merge. The doc it
// adds CONFORMS to the incoming validator (age >= 0); the violating-doc case is
// Scenario 5.
db.items.insertOne({ _id: 100, age: 1 })
db.runCommand({ doltCommit: 1, message: "main: add a conforming doc", author: "alice <alice@acme.com>" })

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

## Scenario 4: Divergent validators on both branches -- resolve the conflict

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

The same workflow covers every divergence shape (each has an automated subtest):
- **`$jsonSchema` validators** (not just query-expression ones) -- the full
  schema document is carried through `base` / `ours` / `theirs`.
- **`validationLevel` and `validationAction`** divergence, alongside the
  validator -- the chosen side's action follows.
- **Both branches *create* the same collection** with different validators (an
  add/add conflict): the `base` side is `null`, and resolution picks a side.
- **One branch drops the collection while the other changes its metadata** (a
  modify/delete): the conflict's dropped side is `null`. Resolving *theirs* (the
  drop) removes the collection; resolving *ours* restores it with the chosen
  metadata and its data -- existence and metadata never disagree.

---

## Scenario 5: Data that violates a validator merged from the other branch

> **STATUS: not yet implemented.** This scenario documents intended behavior and
> is tracked by epic `workspace-h0w`. Today the merge silently accepts the
> violating document (see the epic). The scenario below is the target once the
> cross-validation merge conflict lands; do not treat a passing run as
> verification until the epic is closed.

Scenario 4 covers divergence in the validator *definition*. This scenario covers
the orthogonal case: the validator definition does **not** conflict (only one
branch touches it), but the **documents** the other branch wrote violate it.

The governing rule mirrors write-path validation, lifted onto the merge: a
document whose merged value differs from the merge base (an insert or a modify)
must satisfy the merged validator, or the merge surfaces a **validation
conflict** on that document. Documents unchanged since the base are grandfathered
(a validator is never retroactive), exactly as when a validator is added to a
collection of existing data. `validationLevel` and `validationAction` carry the
same meaning they have on the write path.

```js
var db = db.getSiblingDB("valdataconflict")
db.dropDatabase()
db.createCollection("items")   // no validator yet
db.runCommand({ doltCommit: 1, message: "create items", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature adds a validator; its own data conforms.
var feat = db.getSiblingDB("valdataconflict@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } }, validationAction: "error" })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

// Main inserts a document that VIOLATES the (not-yet-visible-here) validator.
db.items.insertOne({ _id: 1, age: -5 })
db.runCommand({ doltCommit: 1, message: "main: insert age -5", author: "alice <alice@acme.com>" })

// Merge feature into main: the validator arrives and _id:1 violates it.
db.getSiblingDB("valdataconflict@main").runCommand({ doltMerge: 1, merge_in: "feature" })
// TARGET: { conflicts: [ { collection: "items", count: 1 } ], ok: 0, ... }
```

Target conflict shape and resolution (final vocabulary pending the epic):

```js
var main = db.getSiblingDB("valdataconflict@main")
printjson(main.runCommand({ doltConflicts: 1 }).conflicts)
// TARGET: one entry
//   { conflictId: "<hash>", type: "validation", name: "items", documentId: 1,
//     document: { _id: 1, age: -5 }, validator: { age: { $gte: 0 } } }
```

---

## Coverage note: merge cross-validation edge cases

The cross-validation merge behavior (Scenario 5) has a large edge-case matrix
tracked in its epic. Once implemented, each case gets a subtest in
`tests/verify/validator_test.go` and, where practical, an entry here. The matrix
is enumerated in the epic and its child tasks.
