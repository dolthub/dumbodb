# View Merge Verification

Manual verification guide for merging view definitions across branches and
resolving view-definition conflicts. Views live in the branch working set (a
blob entry in the collections address map), so branching carries them, a view
added or dropped on one side merges cleanly, and a view redefined divergently on
both branches is a conflict resolved through the same
`doltConflicts` / `doltResolveConflict` workflow that documents and indexes use.

This is a DumboDB-only capability: MongoDB has no versioning analogue, so there
is no parity comparison. Work through each scenario top to bottom in `mongosh`;
each uses its own database so they are independent.

These scenarios verify the design in `docs/design/live-views.md` section 4.7.

> **Automated equivalent:** `tests/verify/view_merge_test.go`
> (`TestViewMergeVerify`) covers every scenario in this document as subtests
> using the same setup. Run it with:
> ```
> go test ./tests/verify/ -run TestViewMergeVerify -v
> ```

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

## How to read the checks

A read against a view runs its pipeline over the source collection. Every
scenario seeds the same three base documents and defines the view as a single
`$match` on `status`, so the set of `_id`s a `find` on the view returns tells you
which definition is in effect:

- `[{_id:1, status:"active"}, {_id:2, status:"inactive"}, {_id:3, status:"pending"}]`
- a view of `$match {status:"active"}` returns `[1]`, `inactive` returns `[2]`,
  `pending` returns `[3]`.

---

## Scenario 1: A view added on a branch merges cleanly

A branch that adds a view merges into a branch that advanced independently; the
view then resolves on the target branch.

Note the extra commit on `main` after branching: without it, `main` and
`feature` share a single line of history and `doltMerge` just fast-forwards the
`main` pointer to `feature`'s commit -- no 3-way merge runs and the view-add is
never actually merged. Advancing `main` with its own commit forces a real merge
commit, which is what exercises the view-merge path.

```js
var db = db.getSiblingDB("vwmrg1")
db.dropDatabase()

db.items.insertMany([
  { _id: 1, status: "active" },
  { _id: 2, status: "inactive" },
  { _id: 3, status: "pending" }
])
db.runCommand({ doltCommit: 1, message: "seed items", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature adds the view.
var feat = db.getSiblingDB("vwmrg1@feature")
feat.runCommand({ create: "cv", viewOn: "items", pipeline: [ { $match: { status: "active" } } ] })
feat.runCommand({ doltCommit: 1, message: "feature: add view cv", author: "bob <bob@widgets.io>" })

// Main advances independently so the merge is a real 3-way merge (not a
// fast-forward).
db.items.insertOne({ _id: 4, status: "active" })
db.runCommand({ doltCommit: 1, message: "main: add item 4", author: "alice <alice@acme.com>" })

// Merge feature into main.
db.runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected: { commitId: "<hash>", message: "<merge message>", ok: 1 }
// The message is NOT "fast-forward" -- a merge commit was created.

// The view now resolves on main, over main's own data too.
db.cv.find().sort({ _id: 1 })
// Expected: two documents: { _id: 1, status: "active" }, { _id: 4, status: "active" }
```

---

## Scenario 2: Divergent redefine -- resolve "theirs"

The same view is redefined differently on each branch. The merge stops with a
conflict; `doltConflicts` reports it under `views`; resolving "theirs" applies
the other branch's definition.

```js
var db = db.getSiblingDB("vwmrg2")
db.dropDatabase()

db.items.insertMany([
  { _id: 1, status: "active" }, { _id: 2, status: "inactive" }, { _id: 3, status: "pending" }
])
db.runCommand({ create: "cv", viewOn: "items", pipeline: [ { $match: { status: "active" } } ] })
db.runCommand({ doltCommit: 1, message: "seed items + view cv", author: "alice <alice@acme.com>" })

db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature: cv -> inactive.
var feat = db.getSiblingDB("vwmrg2@feature")
feat.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "inactive" } } ] })
feat.runCommand({ doltCommit: 1, message: "feature: cv -> inactive", author: "bob <bob@widgets.io>" })

// Main: cv -> pending.
db.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "pending" } } ] })
db.runCommand({ doltCommit: 1, message: "main: cv -> pending", author: "alice <alice@acme.com>" })

// Merge surfaces a conflict (mongosh throws / ok:0).
db.runCommand({ doltMerge: 1, merge_in: "feature" })
// Expected: unresolved conflicts

// The conflict is self-describing: it names the view and carries ours/theirs.
var rc = db.runCommand({ doltConflicts: 1 })
printjson(rc.views)
// Expected: views has length 1 with
//   { conflictId: "view:cv", view: "cv",
//     base:   { viewOn: "items", pipeline: [ { $match: { status: "active"  } } ] },
//     ours:   { viewOn: "items", pipeline: [ { $match: { status: "pending"  } } ], diffType: "modified" },
//     theirs: { viewOn: "items", pipeline: [ { $match: { status: "inactive" } } ], diffType: "modified" } }

// Resolve to theirs (feature's inactive definition), then complete the merge.
db.runCommand({ doltResolveConflict: 1, collection: "cv", conflictId: "view:cv", resolution: "theirs" })
db.runCommand({ doltMerge: 1, continue: 1 })
// Expected: { ..., ok: 1 }

db.cv.find().sort({ _id: 1 })
// Expected: one document: { _id: 2, status: "inactive" }  (theirs won)
```

Key checks:
- `doltConflicts` returns a `views` array with one entry, not a document
  conflict under `collections`.
- The entry names the view and carries `base`, `ours`, and `theirs` definitions.
- After resolving "theirs" and continuing, the view resolves the inactive doc.

---

## Scenario 3: Divergent redefine -- resolve "ours"

Same setup as Scenario 2; resolving "ours" keeps this branch's definition.

```js
var db = db.getSiblingDB("vwmrg3")
db.dropDatabase()
db.items.insertMany([
  { _id: 1, status: "active" }, { _id: 2, status: "inactive" }, { _id: 3, status: "pending" }
])
db.runCommand({ create: "cv", viewOn: "items", pipeline: [ { $match: { status: "active" } } ] })
db.runCommand({ doltCommit: 1, message: "seed items + view cv", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("vwmrg3@feature")
feat.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "inactive" } } ] })
feat.runCommand({ doltCommit: 1, message: "feature: cv -> inactive", author: "bob <bob@widgets.io>" })

db.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "pending" } } ] })
db.runCommand({ doltCommit: 1, message: "main: cv -> pending", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, merge_in: "feature" })
db.runCommand({ doltResolveConflict: 1, collection: "cv", conflictId: "view:cv", resolution: "ours" })
db.runCommand({ doltMerge: 1, continue: 1 })

db.cv.find().sort({ _id: 1 })
// Expected: one document: { _id: 3, status: "pending" }  (ours won)
```

---

## Scenario 4: Divergent redefine -- resolve "custom"

A custom resolution replaces the view with a supplied `{viewOn, pipeline}`
definition, which need match neither side.

```js
var db = db.getSiblingDB("vwmrg4")
db.dropDatabase()
db.items.insertMany([
  { _id: 1, status: "active" }, { _id: 2, status: "inactive" }, { _id: 3, status: "pending" }
])
db.runCommand({ create: "cv", viewOn: "items", pipeline: [ { $match: { status: "active" } } ] })
db.runCommand({ doltCommit: 1, message: "seed items + view cv", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

var feat = db.getSiblingDB("vwmrg4@feature")
feat.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "inactive" } } ] })
feat.runCommand({ doltCommit: 1, message: "feature: cv -> inactive", author: "bob <bob@widgets.io>" })

db.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "pending" } } ] })
db.runCommand({ doltCommit: 1, message: "main: cv -> pending", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, merge_in: "feature" })
db.runCommand({
  doltResolveConflict: 1, collection: "cv", conflictId: "view:cv", resolution: "custom",
  value: { viewOn: "items", pipeline: [ { $match: { status: "active" } } ] }
})
db.runCommand({ doltMerge: 1, continue: 1 })

db.cv.find().sort({ _id: 1 })
// Expected: one document: { _id: 1, status: "active" }  (the custom definition)
```

---

## Scenario 5: Redefine on one side, drop on the other

Main redefines the view; feature drops it. The conflict's `theirs` side is null
(the deletion). Resolving "theirs" applies the deletion, so the view is gone
after the merge.

```js
var db = db.getSiblingDB("vwmrg5")
db.dropDatabase()
db.items.insertMany([
  { _id: 1, status: "active" }, { _id: 2, status: "inactive" }, { _id: 3, status: "pending" }
])
db.runCommand({ create: "cv", viewOn: "items", pipeline: [ { $match: { status: "active" } } ] })
db.runCommand({ doltCommit: 1, message: "seed items + view cv", author: "alice <alice@acme.com>" })
db.runCommand({ doltBranch: 1, branch: "feature" })

// Feature drops the view.
var feat = db.getSiblingDB("vwmrg5@feature")
feat.cv.drop()
feat.runCommand({ doltCommit: 1, message: "feature: drop cv", author: "bob <bob@widgets.io>" })

// Main redefines it.
db.runCommand({ collMod: "cv", viewOn: "items", pipeline: [ { $match: { status: "pending" } } ] })
db.runCommand({ doltCommit: 1, message: "main: cv -> pending", author: "alice <alice@acme.com>" })

db.runCommand({ doltMerge: 1, merge_in: "feature" })

var rc = db.runCommand({ doltConflicts: 1 })
printjson(rc.views)
// Expected: one entry with ours.diffType == "modified" and theirs == null.

// Resolve to theirs: apply the deletion.
db.runCommand({ doltResolveConflict: 1, collection: "cv", conflictId: "view:cv", resolution: "theirs" })
db.runCommand({ doltMerge: 1, continue: 1 })

db.runCommand({ listCollections: 1, filter: { name: "cv" } }).cursor.firstBatch
// Expected: []  (the view was deleted by the resolution)
```

Key checks:
- The conflict reports `ours.diffType == "modified"` and a null `theirs`.
- Resolving "theirs" removes the view; `listCollections` no longer lists it.
