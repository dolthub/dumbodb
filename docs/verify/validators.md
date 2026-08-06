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
> (`TestValidatorVerify`) covers Scenarios 1-4 and 8; the merge cross-validation
> cases (Scenarios 5-7: the data-violation case, the 6a-6i matrix, and the
> two-phase case) are covered by `tests/verify/validator_merge_xval_test.go`
> (`TestValidatorMergeCrossValidation`). Run them with:
> ```
> go test ./tests/verify/ -run 'TestValidatorVerify|TestValidatorMergeCrossValidation' -v
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

// >>> Restart the DumboDB server here (mongosh reconnects; db still points here). <<<

db.runCommand({ listCollections: 1, filter: { name: "items" } })
// firstBatch[0].options.validator is still { age: { $gte: 0 } }

db.items.insertOne({ _id: 3, age: -1 })   // still rejected (121)
db.items.insertOne({ _id: 4, age: 5 })    // ok
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
db.runCommand({ doltMerge: 1, mergeIn: "feature" })
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
db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0, ... }
```

Inspect the conflict -- it is a `type: "metadata"` entry on `items`, never
`__dumbo_catalog__`:

```js
printjson(db.runCommand({ doltConflicts: 1 }).conflicts)
// Expected: one entry
//   { conflictId: "<hash>", type: "metadata", name: "items",
//     reason: { code: "bothModified",
//               message: "branch 'main' (ours) and branch 'feature' (theirs) both changed the validator/options of \"items\"" },
//     base:   { validator: { age: { $gte: 0  } }, validationLevel: "strict", validationAction: "error" },
//     ours:   { validator: { age: { $gte: 21 } }, validationLevel: "strict", validationAction: "error", diffType: "modified" },
//     theirs: { validator: { age: { $gte: 18 } }, validationLevel: "strict", validationAction: "error", diffType: "modified" } }
// (a collection created with only a validator surfaces the effective defaults --
//  validationLevel "strict", validationAction "error" -- not empty strings)
```

Resolve to theirs (feature's `age >= 18`), then complete the merge:

```js
var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "theirs" })
db.runCommand({ doltMerge: 1, continue: 1 })
// { ..., ok: 1 }

db.runCommand({ listCollections: 1, filter: { name: "items" } }).cursor.firstBatch[0].options.validator
// { age: { $gte: 18 } }   (theirs won)
```

For a **custom** resolution, supply a new validator instead:

```js
// (from a fresh conflict) resolve with your own definition:
db.runCommand({
  doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "custom",
  value: { validator: { age: { $gte: 5 } } }
})
db.runCommand({ doltMerge: 1, continue: 1 })
// items' validator is now { age: { $gte: 5 } }
```

Key checks:
- `doltConflicts` reports a single `type: "metadata"` entry on `items`; the
  string `__dumbo_catalog__` never appears anywhere in the output.
- The entry carries a `reason` (`{ code, message }`) naming the divergence, and
  the `base` / `ours` / `theirs` validator definitions.
- After resolving and continuing, `items`' validator is the chosen one, and the
  merge completes (`ok: 1`).

If both branches make the **identical** validator change, there is no
divergence: it converges and the merge completes cleanly with no conflict (the
only case a metadata change is resolved without asking).

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

Scenario 4 covers divergence in the validator *definition*. This scenario covers
the orthogonal case: the validator definition does **not** conflict (only one
branch touches it), but the **documents** the other branch wrote violate it.

**Guiding rule (monotonicity):** a merge may never make data quality more
non-conformant than it was. Measured against the resulting validator, the set of
violating documents in the merged result must be a subset of the set that was
already violating at the merge base -- the merge may only ever *remove*
violations, never introduce one. Two triggers enforce it:

- **A clean auto-merge** that inserts or modifies a document into a violating
  state surfaces a **`type: "validation"` conflict** -- unless the validator's
  `validationAction` is `"warn"`. A pre-existing violator is tolerated only when
  the merge leaves it byte-for-byte unchanged (a validator is never retroactive);
  re-authoring a document to a violating value is a conflict even when the base
  already violated.
- **A divergent document (data) conflict** is validated at resolution time: the
  value you resolve *to* must conform, or the resolution is rejected (unless
  `validationAction` is `"warn"`). This holds even when the base already
  violated -- resolving a conflict is an authoring act.

A validation conflict has no ours/theirs divergence, so it resolves by
**replace-to-conform** (`resolution: "custom"` with a conforming document, which
is re-validated) or **`resolution: "drop"`** (remove the offender). `ours` and
`theirs` are not accepted -- keeping a known violator is the degradation the rule
forbids. `validationLevel` (strict/moderate) is irrelevant to the merge.

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
db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0, ... }
```

Inspect the conflict -- a `type: "validation"` entry carrying the offending
document and the validator it failed:

```js
printjson(db.runCommand({ doltConflicts: 1 }).conflicts)
// one entry:
//   { conflictId: "<hash>", type: "validation", name: "items", documentId: 1,
//     document: { _id: 1, age: -5 }, validator: { age: { $gte: 0 } },
//     reason: { code: "documentValidationFailure", message: "document 1 in ..." } }
```

Resolve by replacing the document with a conforming value, then complete the
merge (a still-violating replacement is rejected):

```js
var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid,
                resolution: "custom", value: { _id: 1, age: 0 } })   // conforms
db.runCommand({ doltMerge: 1, continue: 1 })
// { ..., ok: 1 };  items now has { _id: 1, age: 0 }
```

Or drop the offending document instead of fixing it:

```js
// db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "drop" })
```

Key checks:
- `doltConflicts` reports one `type: "validation"` entry on `items` with
  `documentId: 1`, the offending `document`, and the `validator`; the string
  `__dumbo_catalog__` never appears.
- `resolution: "custom"` with a still-violating value is rejected; a conforming
  value or `resolution: "drop"` completes the merge.

---

## Scenario 6: The full merge cross-validation matrix

Each cell below is a distinct base x ours x theirs case, keyed on the state of a
document versus the *resulting* validator (`A` absent, `C` present & conforms,
`X` present & violates). This table is the at-a-glance summary; each row is then
written out as a full, copy-paste walkthrough (6a-6i). Every cell also has an
automated subtest in `tests/verify/validator_merge_xval_test.go`
(`TestValidatorMergeCrossValidation`).

| cell | base | what the merge did | outcome |
|---|---|---|---|
| 6a | A | insert a violating doc | **validation conflict** (fix or drop) |
| 6b | A | insert a conforming doc | clean merge |
| 6c | C | modify conforming -> violating | **validation conflict** |
| 6d | X | doc left untouched | grandfathered -- no conflict |
| 6e | X | one-sided change, stays violating, action `error` | **validation conflict** |
| 6f | X | one-sided change, stays violating, action `warn` | allowed |
| 6g | A | insert a violating doc, action `warn` | allowed |
| 6h | C | divergent data conflict, resolve to violating vs conforming | **rejected** / completes |
| 6i | X | divergent data conflict, both sides violating | fix required to complete |

Guiding rule: grandfathering under action `error` is narrow -- a pre-existing
violator survives only if the merge never touches it. Any value the merge
*authors* (an insert, a modify, or a resolved data conflict) must conform, unless
the action is `warn`. A clean merge that only ever removes violations (a fix
`X -> C`, or a delete) never conflicts.

**Observing `warn` (cells 6f, 6g):** matching MongoDB, `validationAction: "warn"`
is **server-log-only** -- the write/merge succeeds and the client response
carries no warning of any kind, so there is nothing to observe in the result
(that is why 6f/6g just show the value landing). DumboDB logs one WARN-level
summary line per collection to the **server log**, e.g.
`documents allowed despite failing validation during merge (validationAction:warn) collection=items count=1`
(and the analogous `... (validationAction:warn) collection=... count=...` on
insert/update/findAndModify/bulkWrite). To see it, watch the dumbodb server log;
do not expect anything in the command response.

Every cell uses its own database, so you can run any one in isolation. The
validator is always `{ age: { $gte: 0 } }`.

### Cell 6a -- base absent: insert a violating document (validation conflict)

```js
var db = db.getSiblingDB("mx6a")
db.dropDatabase()
db.createCollection("items")                                                        // base: no validator, no docs
db.runCommand({ doltCommit: 1, message: "create items", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6a@feature")                                          // feature: add the validator
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

db.items.insertOne({ _id: 1, age: -5 })                                             // main: insert a violator (no validator here yet)
db.runCommand({ doltCommit: 1, message: "main: insert age -5", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0 }
```

Resolve by replacing the offending document with a conforming value:

```js
var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId               // type: "validation", documentId: 1
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "custom", value: { _id: 1, age: 5 } })
db.runCommand({ doltMerge: 1, continue: 1 })                                        // { ok: 1 }
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: 5 }
```

### Cell 6b -- base absent: insert a conforming document (clean merge)

Same as 6a, but the document main inserts conforms, so the merge is clean.

```js
var db = db.getSiblingDB("mx6b")
db.dropDatabase()
db.createCollection("items")
db.runCommand({ doltCommit: 1, message: "create items", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6b@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

db.items.insertOne({ _id: 1, age: 5 })                                              // conforming
db.runCommand({ doltCommit: 1, message: "main: insert age 5", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })                                // { ..., ok: 1 }  (no conflict)
```

### Cell 6c -- base conforming: modify it to violating (validation conflict, drop)

```js
var db = db.getSiblingDB("mx6c")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: 5 })                                             // base: conforming doc
db.runCommand({ doltCommit: 1, message: "create + doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6c@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

db.items.updateOne({ _id: 1 }, { $set: { age: -5 } })                              // main: turn it violating
db.runCommand({ doltCommit: 1, message: "main: age -> -5", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0 }
```

This time drop the offender instead of fixing it:

```js
var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "drop" })
db.runCommand({ doltMerge: 1, continue: 1 })                                        // { ok: 1 }
db.items.findOne({ _id: 1 })                                                        // null (removed)
```

### Cell 6d -- base violating & untouched: grandfathered (clean merge)

A document that already violated the incoming validator at the base, and that the
merge never touches, is grandfathered -- no conflict.

```js
var db = db.getSiblingDB("mx6d")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: -5 })                                            // base: already violating
db.runCommand({ doltCommit: 1, message: "create + violating doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6d@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

db.items.insertOne({ _id: 2, age: 9 })                                             // main advances but does NOT touch _id:1
db.runCommand({ doltCommit: 1, message: "main: add conforming doc", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })                                // { ..., ok: 1 }  (no conflict)
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: -5 }  (grandfathered, survives)
```

### Cell 6e -- base violating: one-sided re-author, action error (validation conflict)

Once the merge *authors* a change to that document, the grandfathering no longer
applies under `error`, even though the value was already violating.

```js
var db = db.getSiblingDB("mx6e")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: -5 })
db.runCommand({ doltCommit: 1, message: "create + violating doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6e@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })             // action defaults to "error"
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0", author: "bob <bob@widgets.io>" })

db.items.updateOne({ _id: 1 }, { $set: { age: -9 } })                              // main: re-author to another violating value
db.runCommand({ doltCommit: 1, message: "main: age -> -9", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0 }

var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "drop" })
db.runCommand({ doltMerge: 1, continue: 1 })                                        // { ok: 1 }
```

### Cell 6f -- base violating: one-sided re-author, action warn (allowed)

Identical to 6e, but the validator's action is `warn`, so the re-authored
violating value is allowed and the merge is clean.

```js
var db = db.getSiblingDB("mx6f")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: -5 })
db.runCommand({ doltCommit: 1, message: "create + violating doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6f@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } }, validationAction: "warn" })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0 (warn)", author: "bob <bob@widgets.io>" })

db.items.updateOne({ _id: 1 }, { $set: { age: -9 } })
db.runCommand({ doltCommit: 1, message: "main: age -> -9", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })                                // { ..., ok: 1 }  (warn allows it)
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: -9 }
```

### Cell 6g -- action warn suppresses a violating insert (allowed)

```js
var db = db.getSiblingDB("mx6g")
db.dropDatabase()
db.createCollection("items")
db.runCommand({ doltCommit: 1, message: "create items", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6g@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } }, validationAction: "warn" })
feat.runCommand({ doltCommit: 1, message: "feature: require age >= 0 (warn)", author: "bob <bob@widgets.io>" })

db.items.insertOne({ _id: 1, age: -5 })                                             // violating insert
db.runCommand({ doltCommit: 1, message: "main: insert age -5", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })                                // { ..., ok: 1 }  (warn allows it)
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: -5 }
```

### Cell 6h -- data conflict: a violating resolution is rejected

Here both branches modify the same document divergently (a real data conflict).
The value you resolve *to* is validated: resolving to the violating side is
rejected; resolving to the conforming side completes.

```js
var db = db.getSiblingDB("mx6h")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: 5 })
db.runCommand({ doltCommit: 1, message: "create + doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6h@feature")                                          // feature: add validator AND edit _id:1 conformingly
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.items.updateOne({ _id: 1 }, { $set: { age: 7, tag: "f" } })
feat.runCommand({ doltCommit: 1, message: "feature: validator + age 7", author: "bob <bob@widgets.io>" })

db.items.updateOne({ _id: 1 }, { $set: { age: -5, tag: "m" } })                    // main: edit _id:1 to a violating value
db.runCommand({ doltCommit: 1, message: "main: age -5", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0 }   -- a type "document" conflict on _id:1

var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "ours" })
// { ok: 0, errmsg: "... resolved document ... violates the collection validator ..." }   -- REJECTED

db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "theirs" })   // conforming side
db.runCommand({ doltMerge: 1, continue: 1 })                                        // { ok: 1 }
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: 7, tag: "f" }
```

### Cell 6i -- data conflict, base violating: both sides violate (fix required)

Both sides of the conflict are violating values, so neither `ours` nor `theirs`
is acceptable -- only a conforming replacement (or a drop) completes the merge.

```js
var db = db.getSiblingDB("mx6i")
db.dropDatabase()
db.createCollection("items")
db.items.insertOne({ _id: 1, age: -5 })                                            // base: already violating
db.runCommand({ doltCommit: 1, message: "create + violating doc", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("mx6i@feature")                                          // feature: edit (still violating) BEFORE adding validator
feat.items.updateOne({ _id: 1 }, { $set: { age: -3, tag: "f" } })
feat.runCommand({ collMod: "items", validator: { age: { $gte: 0 } } })
feat.runCommand({ doltCommit: 1, message: "feature: age -3 + validator", author: "bob <bob@widgets.io>" })

db.items.updateOne({ _id: 1 }, { $set: { age: -7, tag: "m" } })                    // main: another violating edit
db.runCommand({ doltCommit: 1, message: "main: age -7", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, mergeIn: "feature" })                                // ok: 0, a "document" conflict on _id:1

var cid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "ours" })     // REJECTED (age -7 violates)
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "theirs" })   // REJECTED (age -3 violates)
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: cid, resolution: "custom", value: { _id: 1, age: 2 } })
db.runCommand({ doltMerge: 1, continue: 1 })                                        // { ok: 1 }
db.items.findOne({ _id: 1 })                                                        // { _id: 1, age: 2 }
```

## Scenario 7: The validator definition and the data both conflict (two-phase)

When the same merge has BOTH a validator-definition conflict (Scenario 4) and a
document that violates the resulting validator, the merge resolves in two phases.
The document check is **deferred** until the validator is pinned -- until then the
resulting validator is unknown, so only the metadata conflict is shown. After you
resolve the metadata conflict and `continue`, cross-validation runs against the
now-pinned validator and **re-pauses** if a merged document violates it.

```js
var db = db.getSiblingDB("valtwophase")
db.dropDatabase()

// Base: validated items (age >= 0) with one conforming doc.
db.createCollection("items", { validator: { age: { $gte: 0 } } })
db.items.insertOne({ _id: 1, age: 5 })
db.runCommand({ doltCommit: 1, message: "create validated items + doc", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature tightens the validator to age >= 10.
var feat = db.getSiblingDB("valtwophase@feature")
feat.runCommand({ collMod: "items", validator: { age: { $gte: 10 } } })
feat.runCommand({ doltCommit: 1, message: "feature: age >= 10", author: "bob <bob@widgets.io>" })

// Main diverges the validator to age >= 3 AND inserts a doc that conforms to
// age >= 3 but will violate feature's age >= 10 once that side wins.
db.runCommand({ collMod: "items", validator: { age: { $gte: 3 } } })
db.items.insertOne({ _id: 2, age: 5 })
db.runCommand({ doltCommit: 1, message: "main: age >= 3 + doc age 5", author: "alice <alice@acme.com>" })

// Merge feature into main: the validator DEFINITION conflicts (age >= 3 vs age >= 10).
db.runCommand({ doltMerge: 1, merge_in: "feature" })
// { conflicts: [ { collection: "items", count: 1 } ], ok: 0 }

// Phase 1 -- only the metadata conflict is visible (the document check is deferred):
db.runCommand({ doltConflicts: 1 }).conflicts   // one type: "metadata" entry on items
var mid = db.runCommand({ doltConflicts: 1 }).conflicts[0].conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: mid, resolution: "theirs" })  // pin age >= 10

// Continue RE-PAUSES: _id:2 (age 5) now violates the pinned age >= 10.
db.runCommand({ doltMerge: 1, continue: 1 })    // { conflicts: [...], ok: 0 }

// Phase 2 -- resolve the surfaced validation conflict, then finish:
var vid = db.runCommand({ doltConflicts: 1 }).conflicts.find(c => c.type == "validation").conflictId
db.runCommand({ doltResolveConflict: 1, collection: "items", conflictId: vid,
                resolution: "custom", value: { _id: 2, age: 12 } })
db.runCommand({ doltMerge: 1, continue: 1 })    // { ..., ok: 1 }

db.items.find().sort({ _id: 1 })
// { _id: 1, age: 5 }   (grandfathered -- untouched by the merge, so not re-checked)
// { _id: 2, age: 12 }  (fixed to satisfy age >= 10)
```

Key checks:
- Before the metadata conflict is resolved, `doltConflicts` shows no
  `type: "validation"` entry -- the document check waits for the pinned validator.
- `continue` after resolving the metadata conflict returns `ok: 0` with the
  deferred validation conflict, which then resolves by replace-to-conform / drop.

---

## Scenario 8: A validator is visible in diff, status, and log

A collection's validator (and any change to it) surfaces under the `metadata`
field of the unified `changes` array in `doltDiff`, `doltStatus`, and `doltLog`
(`--stat` / `--patch`). A `collMod` that changes **only** the validator (no
document or index change) still surfaces -- it rewrites the internal catalog, not
the collection's own data.

```js
var db = db.getSiblingDB("valobserve")
db.dropDatabase()

// A newly-created validated collection: before any commit, diff and status show
// it as "added" with the validator under metadata.to (metadata.from is null).
db.createCollection("items", { validator: { age: { $gte: 0 } }, validationLevel: "strict" })

db.runCommand({ doltStatus: 1 })
// { ..., dirty: true, changes: [ { type: "collection", name: "items", status: "added",
//     documents: { added: 0, modified: 0, deleted: 0 }, indexes: { ... },
//     metadata: { from: null,
//                 to: { validator: { age: { $gte: 0 } }, validationLevel: "strict", validationAction: "error" } } } ] }

db.runCommand({ doltDiff: 1 }).changes[0].metadata   // same { from: null, to: {...} }

db.runCommand({ doltCommit: 1, message: "create validated items", author: "alice <alice@acme.com>" })

// A collMod that changes ONLY the validator still surfaces as a modified
// collection with metadata { from, to }, and makes the workspace dirty.
db.runCommand({ collMod: "items", validator: { age: { $gte: 10 } } })

db.runCommand({ doltStatus: 1 })
// { ..., dirty: true, changes: [ { ..., status: "modified",
//     metadata: { from: { validator: { age: { $gte: 0  } }, ... },
//                 to:   { validator: { age: { $gte: 10 } }, ... } } } ] }

db.runCommand({ doltDiff: 1 }).changes[0].metadata   // same { from, to }

// Commit it; doltLog --stat / --patch shows the same metadata change for the commit.
db.runCommand({ doltCommit: 1, message: "tighten validator to age >= 10", author: "alice <alice@acme.com>" })
db.runCommand({ doltLog: 1, limit: 1, stat: true }).commits[0].changes[0].metadata
// { from: { validator: { age: { $gte: 0 } }, ... }, to: { validator: { age: { $gte: 10 } }, ... } }
```

Key checks:
- A newly-added validated collection shows the validator under `metadata.to`
  (`metadata.from` is `null`) in `doltDiff` and `doltStatus`.
- A validator-only `collMod` surfaces as a `modified` collection carrying
  `metadata: { from, to }` in `doltDiff`, `doltStatus`, and `doltLog`
  (`--stat` / `--patch`), and makes `doltStatus` report `dirty: true`.
