# doltLog Pagination and Filtering Verification

Manual verification guide for `doltLog` **frontier pagination** (`from` as a
seed array, `next` in the response, `all` to span every branch) and
**`collection:_id` filtering** (Part B). Work through each scenario top to
bottom in `mongosh`.

> **Automated equivalent:** `tests/verify/log_pagination_filtering_test.go`
> (`TestLogPaginationVerify`). The manual run below relies on wall-clock
> commit order (later commits get later timestamps), which produces the same
> height-primary ordering the automated test asserts.

## Prerequisites

A running DumboDB instance and `mongosh`:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

---

## Background: why the continuation token is a set

`doltLog` walks the commit DAG in reverse topological order, **height
first** (a commit's generation from the root), ties broken by newer
timestamp. After emitting N commits the traversal boundary is not a single
commit but the **set** of commits the walk has discovered (pushed) but not
yet visited -- the frontier. A merge pushes both parents; one is often
consumed many pages before the other.

`from` is overloaded to accept that set:

- `from` absent: start at the branch HEAD.
- `from: "<hash>"`: start at one commit (the existing single-hash form).
- `from: ["<hashA>", "<hashB>", ...]`: start the walk seeded with all
  listed commits.

Each response carries `next`: the frontier (an array of hashes) to pass as
the next call's `from`. When the walk is exhausted, `next` is **omitted**.

## Setup: a history with an old, low feature branch merged late

```js
var pg = db.getSiblingDB("logpage")
pg.dropDatabase()

// main: m1, m2
pg.coll.insertOne({ _id: 1 })
const m1 = pg.runCommand({ doltCommit: 1, message: "m1", author: "a <a@x.io>" }).commitId
pg.coll.insertOne({ _id: 2 })
const m2 = pg.runCommand({ doltCommit: 1, message: "m2", author: "a <a@x.io>" }).commitId

// branch "feat" from main HEAD (m2), then advance feat FIRST so its
// commits get older timestamps than the later main commits.
pg.getSiblingDB("logpage@main").runCommand({ doltBranch: 1, branch: "feat" })
const fdb = db.getSiblingDB("logpage@feat")
fdb.coll.insertOne({ _id: 101 })
const f1 = fdb.runCommand({ doltCommit: 1, message: "f1", author: "a <a@x.io>" }).commitId
fdb.coll.insertOne({ _id: 102 })
const f2 = fdb.runCommand({ doltCommit: 1, message: "f2", author: "a <a@x.io>" }).commitId

// advance main: m3, m4, m5
pg.coll.insertOne({ _id: 3 })
const m3 = pg.runCommand({ doltCommit: 1, message: "m3", author: "a <a@x.io>" }).commitId
pg.coll.insertOne({ _id: 4 })
const m4 = pg.runCommand({ doltCommit: 1, message: "m4", author: "a <a@x.io>" }).commitId
pg.coll.insertOne({ _id: 5 })
const m5 = pg.runCommand({ doltCommit: 1, message: "m5", author: "a <a@x.io>" }).commitId

// merge feat into main -> M
const M = pg.getSiblingDB("logpage@main").runCommand({ doltMerge: 1, merge_in: "feat" }).commitId

print("m1..m5 =", m1, m2, m3, m4, m5)
print("f1,f2  =", f1, f2)
print("M      =", M)
```

Commit graph (heights in parentheses; `init` is the auto root):

```
init(1) - m1(2) - m2(3) - m3(4) - m4(5) - m5(6) - M(7)
                     \                            /
                      f1(4) -------- f2(5) -------
```

The full reverse-topological walk from `M` (height first, newer-timestamp
tie-break, with `m*` newer than `f*`) is:

```
M  m5  m4  f2  m3  f1  m2  m1  init
```

Note `f2` (height 5) sorts **before** `m3` (height 4): height dominates
timestamp. The feature tip `f2` has a low height, so it lingers in the
frontier while the taller main commits drain first.

---

## Scenario A1: full walk has no pagination token

A single page large enough to hold the whole history returns every commit
and omits `next`.

```js
pg.runCommand({ doltLog: 1, limit: 100 })
```

Key checks:
- 9 commits, in the order `M, m5, m4, f2, m3, f1, m2, m1, init`.
- No top-level `next` field (the frontier is empty).

---

## Scenario A2: first page carries the dormant feature tip in `next`

This is the headline property. With `limit: 2`, page 1 emits `M` and `m5`.
The frontier afterward is **two** commits: `m4` (the main commit still to
come) and `f2` (the feature tip, discovered as `M`'s second parent but not
yet visited).

```js
pg.runCommand({ doltLog: 1, limit: 2 })
```

Expected:

```json
{
  "commits": [
    { "commitId": "<M>",  "parent1": "<m5>", "parent2": "<f2>", "refs": ["HEAD", "main"], "message": "Merge branch 'feat' into 'main'", "...": "<author/committer/timestamps as usual>" },
    { "commitId": "<m5>", "parent1": "<m4>", "message": "m5", "...": "<...>" }
  ],
  "next": [ "<f2>", "<m4>" ],
  "ok": 1
}
```

Key checks:
- 2 commits returned: `M` then `m5`.
- `next` is present and contains **exactly two** hashes: `f2` and `m4`
  (array order is not significant).
- `f2` -- the feature tip -- is in `next` even though no feature commit has
  been returned yet. A single-pointer cursor would have lost it.

---

## Scenario A3: paging to the end reassembles the full walk

Continue feeding each page's `next` back as `from`. The pages tile the
history exactly once, in the same order as the full walk.

```js
// page 1 (from Scenario A2): [M, m5],  next = [f2, m4]
var p = pg.runCommand({ doltLog: 1, limit: 2 })

// page 2
p = pg.runCommand({ doltLog: 1, limit: 2, from: p.next })
printjson(p)   // commits: [m4, f2],  next: [m3, f1]

// page 3
p = pg.runCommand({ doltLog: 1, limit: 2, from: p.next })
printjson(p)   // commits: [m3, f1],  next: [m2]

// page 4
p = pg.runCommand({ doltLog: 1, limit: 2, from: p.next })
printjson(p)   // commits: [m2, m1],  next: [init]

// page 5 (final)
p = pg.runCommand({ doltLog: 1, limit: 2, from: p.next })
printjson(p)   // commits: [init],  no next
```

Per-page expectation:

| Page | from (seeds)   | commits     | next         |
|------|----------------|-------------|--------------|
| 1    | (HEAD)         | M, m5       | [f2, m4]     |
| 2    | [f2, m4]       | m4, f2      | [m3, f1]     |
| 3    | [m3, f1]       | m3, f1      | [m2]         |
| 4    | [m2]           | m2, m1      | [init]       |
| 5    | [init]         | init        | (omitted)    |

Key checks:
- Concatenating the five pages' `commits` yields exactly the full-walk
  order from Scenario A1, with no commit repeated and none missing.
- `f2` is carried in `next` on page 1, then returned on page 2 -- never
  before it is actually reached.
- Only the final page omits `next`.

---

## Scenario A4: `from` accepts a seed array directly

Seeding `from` with page 2's frontier reproduces page 2 exactly, proving
`from` is the inverse of `next` and accepts an array.

```js
pg.runCommand({ doltLog: 1, limit: 2, from: [f2, m4] })
```

Key checks:
- 2 commits: `m4` then `f2` (the `m4`-before-`f2` order is the
  newer-timestamp tie-break at equal height 5).
- `next` contains `m3` and `f1`.
- Identical to Scenario A3 page 2.

---

## Scenario A5: single-hash `from` still works (back-compat)

The pre-existing single-string form is unchanged: it seeds a one-element
frontier and still walks both parents of any merge it reaches.

```js
pg.runCommand({ doltLog: 1, from: f2 })
```

Key checks:
- Walk is `f2, f1, m2, m1, init` (only commits reachable from `f2`).
- `m3`, `m4`, `m5`, and `M` do **not** appear.
- With no `limit`, the default of 20 applies; `next` is omitted here
  because the whole reachable set fits in one page.

---

## Scenario A6: `all` spans every branch

`all: true` seeds the walk with the HEAD of every branch, so commits that live
only on an un-merged branch appear. It is mutually exclusive with `from`.

```js
// Create a side branch off main with a commit that is NOT merged back.
pg.getSiblingDB("logpage@main").runCommand({ doltBranch: 1, branch: "side" })
var side = db.getSiblingDB("logpage@side")
side.coll.insertOne({ _id: 900 })
const s1 = side.runCommand({ doltCommit: 1, message: "s1", author: "a <a@x.io>" }).commitId

// Default walk from main HEAD does not reach s1:
pg.runCommand({ doltLog: 1 })            // no commit with message "s1"

// all: true seeds every branch HEAD, so s1 appears:
pg.runCommand({ doltLog: 1, all: true }) // includes "s1" plus all main/feature commits
```

Key checks:
- The default `doltLog` (main HEAD) does **not** include `s1`.
- `doltLog` with `all: true` **does** include `s1`, alongside the main and
  feature commits.
- `{ doltLog: 1, all: true, from: s1 }` is rejected (mutually exclusive).
- Paging works: pass page 1's `next` back as `from` (not `all`) for page 2.

---

# Part B: `collection:_id` filtering

`filters` is an array of `{collection: _id}` entries; the value is one `_id`,
or an array of `_id`s. A commit is returned only if it added, removed, or
modified (vs its first parent) one of those documents, in any listed entry
(OR). Because `_id` is immutable, every change to a document is a touch.

An `_id` may be any valid BSON `_id` type -- number, string, `ObjectId`, date,
decimal, or a document/subdocument (e.g. `{ events: { region: "us", seq: 5 } }`)
-- matched as `find({_id: ...})` would, with field-order-sensitive equality for
document `_id`s. Only an array is never a valid `_id`, so an array value is
always the id-list form.

## Setup: several documents per collection

```js
var ff = db.getSiblingDB("logfilter")
ff.dropDatabase()

ff.orders.insertMany([ { _id: 1, status: "pending" }, { _id: 2, status: "shipped" } ])
ff.runCommand({ doltCommit: 1, message: "c1 add orders 1,2", author: "a <a@x.io>" })

ff.users.insertOne({ _id: 1, name: "alice" })
ff.runCommand({ doltCommit: 1, message: "c2 add user 1", author: "a <a@x.io>" })

ff.orders.insertOne({ _id: 3, status: "pending" })
ff.runCommand({ doltCommit: 1, message: "c3 add order 3", author: "a <a@x.io>" })

// mixed: change order 1 (matched), order 2 (not matched), user 1 (other collection)
ff.orders.updateOne({ _id: 1 }, { $set: { note: "x" } })
ff.orders.updateOne({ _id: 2 }, { $set: { region: "eu" } })
ff.users.updateOne({ _id: 1 }, { $set: { name: "alicia" } })
ff.runCommand({ doltCommit: 1, message: "c4 mixed edit", author: "a <a@x.io>" })

ff.orders.deleteOne({ _id: 1 })
ff.runCommand({ doltCommit: 1, message: "c5 delete order 1", author: "a <a@x.io>" })
```

## Scenario B1: follow one document

```js
ff.runCommand({ doltLog: 1, filters: [ { orders: 1 } ] })
```

Key checks:
- Returns `c5` (delete), `c4` (modify), `c1` (add) -- every change to order 1.
- `c3` (added order 3) and the users commits are **excluded**.

## Scenario B2: id-list sugar and per-collection OR

```js
ff.runCommand({ doltLog: 1, filters: [ { orders: [1, 3] } ] })   // c5, c4, c3, c1
ff.runCommand({ doltLog: 1, filters: [ { orders: 3 }, { users: 1 } ] })  // c4, c3, c2
```

Key checks:
- `{ orders: [1, 3] }` returns commits touching order 1 or 3 (OR over ids).
- `[{orders:3},{users:1}]` ORs across collections; the second query returns
  `c4` (touched user 1), `c3` (order 3), `c2` (user 1).

## Scenario B2b: whole collection (collection-name string)

A bare collection-name string entry matches any `_id` -- every commit that
touched the collection.

```js
ff.runCommand({ doltLog: 1, filters: [ "orders" ] })   // c5, c4, c3, c1
```

Key checks:
- Returns every commit that touched `orders` (`c5`, `c4`, `c3`, `c1`); the
  users-only commits are excluded.
- A string entry is distinct from a `{collection: id}` document, so there is no
  ambiguity. (An empty `_id` array -- `[ { orders: [] } ]` -- is rejected; use
  the string form.)

## Scenario B3: scoped stat/patch

`c4` changed order 1 (matched), order 2 (not), and user 1 (other collection).

```js
ff.runCommand({ doltLog: 1, limit: 1, patch: true, filters: [ { orders: 1 } ] })
```

Key checks:
- The single returned commit (`c5` is HEAD-most match; use `from` to target
  `c4` if needed) reports `diff` for **only** the `orders` collection, and
  within it only the matched `_id` -- order 2 and the user change are absent.

## Scenario B4: errors

- `filters` not an array, an entry with more than one key, or an id-list
  element that is itself an array, is rejected. (A document value is a valid
  `_id`, not an error.)

## Scenario B5: non-integer `_id`s (ObjectId and document)

`_id` can be any valid BSON type. Build a collection whose documents use an
`ObjectId` `_id` and a subdocument `_id`, and filter by each.

```js
var nf = db.getSiblingDB("logfilter_ids")
nf.dropDatabase()

const oid = ObjectId()
nf.events.insertOne({ _id: oid, kind: "login" })
nf.runCommand({ doltCommit: 1, message: "e1 add oid event", author: "a <a@x.io>" })

nf.events.insertOne({ _id: { region: "us", seq: 5 }, kind: "order" })
nf.runCommand({ doltCommit: 1, message: "e2 add subdoc event", author: "a <a@x.io>" })

// modify the ObjectId-keyed doc -- a second touch of that _id
nf.events.updateOne({ _id: oid }, { $set: { kind: "logout" } })
nf.runCommand({ doltCommit: 1, message: "e3 modify oid event", author: "a <a@x.io>" })
```

Filter by the `ObjectId` `_id`:

```js
nf.runCommand({ doltLog: 1, filters: [ { events: oid } ] })   // e3, e1
```

Key checks:
- Returns `e3` (modify) and `e1` (add) -- both touches of the `ObjectId`-keyed
  document. The subdocument-keyed `e2` is excluded.

Filter by the document `_id` (field order is significant):

```js
nf.runCommand({ doltLog: 1, filters: [ { events: { region: "us", seq: 5 } } ] })   // e2
nf.runCommand({ doltLog: 1, filters: [ { events: { seq: 5, region: "us" } } ] })   // [] (different _id)
```

Key checks:
- `{ region: "us", seq: 5 }` returns `e2` (the subdocument-keyed insert).
- `{ seq: 5, region: "us" }` -- same fields, different order -- is a **different
  `_id`** and matches nothing, exactly as `find({_id: ...})` would behave.

## Scenario B6: `$match` query (per-commit, touched)

A list element can be a `{$match: <query>}` predicate. It is evaluated **per
commit**: a commit is included when it touched (added/removed/modified vs its
parent) a document satisfying the query. For a modification, either the pre- or
the post-image may satisfy it. `$match`es and explicit `_id`s OR.

```js
var mf = db.getSiblingDB("logfilter_match")
mf.dropDatabase()

mf.orders.insertMany([ { _id: 1, status: "pending" }, { _id: 2, status: "shipped" } ])
mf.runCommand({ doltCommit: 1, message: "m1 add 1,2", author: "a <a@x.io>" })
mf.orders.insertOne({ _id: 3, status: "pending" })
mf.runCommand({ doltCommit: 1, message: "m2 add 3", author: "a <a@x.io>" })
mf.orders.updateOne({ _id: 1 }, { $set: { status: "shipped" } })   // pending -> shipped
mf.runCommand({ doltCommit: 1, message: "m3 ship 1", author: "a <a@x.io>" })

mf.runCommand({ doltLog: 1, filters: [ { orders: [ { $match: { status: "pending" } } ] } ] })
```

Key checks:
- Returns `m3`, `m2`, `m1`:
  - `m1` added order 1 (pending),
  - `m2` added order 3 (pending),
  - `m3` shipped order 1 -- included because its **pre-image** was pending (the
    commit that moves a doc *out* of the matched set still matches).
- A commit that does not touch `orders` is never included, even if a pending
  order exists in its snapshot (presence alone is not a match).
- A `$`-operator other than `$match` is rejected.

## Scenario B7: `$match` with an operator (`$gt`)

`$match` accepts the full `find()` operator set -- range, regex, `$in`, etc.
This example uses `$gt`; try `$gte`/`$lt`/`$regex`/`$exists` the same way.

```js
var gf = db.getSiblingDB("logfilter_gt")
gf.dropDatabase()

gf.orders.insertOne({ _id: 1, amount: 50 })
gf.runCommand({ doltCommit: 1, message: "g1 add cheap order", author: "a <a@x.io>" })
gf.orders.insertOne({ _id: 2, amount: 300 })
gf.runCommand({ doltCommit: 1, message: "g2 add pricey order", author: "a <a@x.io>" })
gf.orders.updateOne({ _id: 1 }, { $set: { amount: 500 } })   // 50 -> 500, crosses 100
gf.runCommand({ doltCommit: 1, message: "g3 bump order 1", author: "a <a@x.io>" })

gf.runCommand({ doltLog: 1, filters: [ { orders: [ { $match: { amount: { $gt: 100 } } } ] } ] })
```

Key checks:
- Returns `g3`, `g2`:
  - `g2` added order 2 at 300 (`> 100`),
  - `g3` bumped order 1 from 50 to 500 -- included via its **post-image**
    (now `> 100`), the mirror of B6's pre-image case.
- `g1` is **excluded**: it added order 1 at 50, which is not `> 100`.

## Scenario B8: `$changed` -- a field that changed (any value)

`{ field: {$changed: true} }` inside a `$match` matches when that field differs
between the commit and its parent, regardless of values. It combines with value
conditions (implicit AND).

```js
var cf = db.getSiblingDB("logfilter_changed")
cf.dropDatabase()

cf.orders.insertMany([
  { _id: 1, customer: "4242", status: "pending" },
  { _id: 2, customer: "9999", status: "pending" }
])
cf.runCommand({ doltCommit: 1, message: "k1 add", author: "a <a@x.io>" })
cf.orders.updateOne({ _id: 1 }, { $set: { status: "shipped" } })   // status changed
cf.runCommand({ doltCommit: 1, message: "k2 ship o1", author: "a <a@x.io>" })
cf.orders.updateOne({ _id: 1 }, { $set: { note: "x" } })           // status NOT changed
cf.runCommand({ doltCommit: 1, message: "k3 note o1", author: "a <a@x.io>" })

// commits where status changed (k1 add counts; k2 ships; k3 only touched note):
cf.runCommand({ doltLog: 1, filters: [ { orders: [ { $match: { status: { $changed: true } } } ] } ] })

// status changed AND that order's customer is 4242:
cf.runCommand({ doltLog: 1, filters: [ { orders: [
  { $match: { status: { $changed: true }, customer: "4242" } }
] } ] })
```

Key checks:
- The first query returns `k2`, `k1` -- `k1` added the orders (presence counts),
  `k2` changed status; `k3` (only `note` changed) is **excluded**.
- The second returns `k2`, `k1` for order 1 only; an order-2 status change would
  be excluded by the `customer: "4242"` value condition.
- `$changed` combines with value conditions (AND) and `$and`/`$or`; it is a
  `$match` extension, not a real `find()` operator.

---

## Quick Reference

| Command | Result |
|---------|--------|
| `{ doltLog: 1, from: ["<h1>", "<h2>"] }` | Walk seeded with multiple frontier commits |
| `{ doltLog: 1, limit: N }` then `from: response.next` | Page through history; stop when `next` is absent |
| `{ doltLog: 1, all: true }` | Walk seeded with every branch HEAD (mutually exclusive with `from`) |
| `{ doltLog: 1, filters: [ { coll: id } ] }` | Commits that touched that document |
| `{ doltLog: 1, filters: [ { coll: [id1, id2] }, { other: id } ] }` | OR over ids and collections |
| `{ doltLog: 1, filters: [ "coll" ] }` | Commits that touched any document in `coll` (whole collection) |
| `{ doltLog: 1, filters: [ { coll: [ { $match: {<query>} } ] } ] }` | Commits that touched a doc matching `<query>` (per commit) |
| `{ ... $match: { field: { $changed: true } } ... }` | Commits where `field` changed vs parent (any value; combines with value conditions) |

Notes:
- `next` is an array of seed hashes; order within it is not significant.
  It is omitted when the walk is exhausted.
- The walk is height-first (commit generation), ties broken by newer
  timestamp. This is height-primary ordering, which can differ from git's
  default date ordering.
- `filters` selects commits that touched matching documents -- by `_id`
  (identity), whole collection, or a per-commit `$match` predicate. With a
  filter, `limit` counts matching commits and `stat`/`patch` are scoped to the
  matched documents.
