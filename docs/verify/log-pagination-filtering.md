# doltLog Pagination Verification

Manual verification guide for `doltLog` **frontier pagination**: `from` as a
seed array, `next` in the response, and `all` to span every branch. Work
through each scenario top to bottom in `mongosh`.

> **Automated equivalent:** `tests/versioning_log_pagination_verify_test.go`
> (`TestLogPaginationVerify`). The manual run below relies on wall-clock
> commit order (later commits get later timestamps), which produces the same
> height-primary ordering the automated test asserts.
>
> **Commit filtering** (restricting the log to commits that touched specific
> documents) is planned but not yet shipped; it is being redesigned as a
> simple `collection:_id` filter. Filtering scenarios will be added here when
> that feature lands.

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

## Quick Reference

| Command | Result |
|---------|--------|
| `{ doltLog: 1, from: ["<h1>", "<h2>"] }` | Walk seeded with multiple frontier commits |
| `{ doltLog: 1, limit: N }` then `from: response.next` | Page through history; stop when `next` is absent |
| `{ doltLog: 1, all: true }` | Walk seeded with every branch HEAD (mutually exclusive with `from`) |

Notes:
- `next` is an array of seed hashes; order within it is not significant.
  It is omitted when the walk is exhausted.
- The walk is height-first (commit generation), ties broken by newer
  timestamp. This is height-primary ordering, which can differ from git's
  default date ordering.
- Commit filtering (restricting the log to commits that touched specific
  documents) is planned but not yet available.
