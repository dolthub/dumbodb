# dumboLog: DAG Pagination and Commit Filtering

**Status:** Design draft.
**Date:** 2026-06-16

**Implementation status:**

- **Shipped:** frontier pagination -- `from` as a seed array, `next` in the
  response, and `all` to seed the walk with every branch HEAD (Sections 3.1,
  4, 5). This is the live behaviour; see `docs/verify/log-pagination-filtering.md`.
- **Removed / being redesigned:** the `find()`-style **commit filtering**
  (`filters`, touched semantics, `limit`-counts-matches, `ScanCap`) and the
  **content increment** (`full`, projection, scoped `stat`/`patch`) described
  in Sections 3.2-3.4, 6, and 7.11-7.21 were implemented and tested, then
  **removed before release.** `git log`-style value-predicate filtering over
  document history proved to have confusing semantics (touched-vs-presence,
  which-image-matches); it is being reconsidered in favour of a simpler
  `collection:_id` filter. Those sections are retained below as design
  reference for that redesign, **not** as a description of current behaviour.

---

## 1. Problem

`dumboLog` returns at most `limit` commits (default 20) in reverse
topological order from a single starting commit (`from`, or the branch
HEAD). Two capabilities are missing.

**Pagination.** There is no correct way to ask for "the next page." A
linear cursor (`skip`, or "resume from the last commit's parent") is wrong
for a commit DAG: after emitting N commits, the traversal boundary is not
a single pointer but a **set** of commits -- the pending contents of the
topological iterator's priority queue. A merge commit pushes two parents;
one is consumed immediately while the other can sit unvisited for many
pages. Resuming from "the last parent" silently drops every other commit
still waiting in that queue.

**Filtering.** There is no way to restrict the log to commits that changed
documents of interest. We want to pass `find()`-style filters and return
only commits that *touched* a matching document, mirroring `git log`
pathspec behaviour. The wrinkle: `find()` is collection-scoped, while
`dumboLog` spans collections.

This document specifies (a) pagination whose continuation token is the
frontier set itself, and (b) per-collection filtering with git-pathspec
"touched" semantics, and how the two compose.

## 2. Background: current implementation

Handler `MsgDumboDBLog` (`internal/handler/msg_dumbodb_versioning.go:1058`):

- Parses `$db` into `dbName` + `branch`.
- Reads `limit int32`, `from string`, `stat bool`, `patch bool`.
- `limit: 0` short-circuits to an empty `commits` array
  (`msg_dumbodb_versioning.go:1103`) before touching the backend.
- Emits `commits` + `ok: 1`.

Backend `DumboDBLog` (`internal/backends/dolt/backend.go:2158`):

- Resolves the start commit: `from` (a validated hash) or the branch HEAD
  via `resolveRootishToCommitHash`.
- Default limit is 20 when `limit <= 0` (`backend.go:2172`).
- Walks with `commitgraph.GetTopologicalOrderIterator(ctx, resolver,
  []hash.Hash{startHash}, nil)` (`backend.go:2231`) -- note this already
  accepts a **slice** of seed hashes.
- Orders by `(Height, timestamp)`, newest first. `Height` comes from
  `commit.Height()` and is **intrinsic** to a commit
  (`backend.go:3502`, `doltHashResolver.ResolveCommitHash`).
- For `stat`/`patch`, diffs each emitted commit against parent1
  (`backend.go:2276-2363`), producing per-collection added/modified/deleted
  document lists. This is the machinery the filter reuses (Section 6).

`LogParams` / `LogResult` / `CommitInfo` live in
`internal/backends/versioning.go:169-197`.

The full `find()` matcher `common.FilterDocument`
(`internal/handler/common/filter.go`) is reachable from the backend
through the `RegisterPartialFilterMatcher` bridge
(`internal/backends/partial_filter.go`), installed so partial indexes can
match without an import cycle. The filter feature reuses that same
registered predicate, so operator coverage equals normal `find()`.

The intrinsic-`Height` property is the foundation of the pagination
design: the global `(Height, timestamp)` order over any seed set is fixed,
regardless of which seeds the iterator is given. Re-seeding from a saved
frontier reproduces the exact order the single uninterrupted walk would
have produced.

## 3. The model

### 3.1 Pagination

- **Input.** `from` is overloaded to accept a string (one seed, today's
  behaviour) **or** an array of hash strings (the seed frontier). Absent
  means default to `[branch HEAD]`.
- **Input (`all`).** A new `all` bool seeds the frontier with the HEAD of
  **every branch** (`refs/heads/*`; tags excluded), so the walk spans the
  whole commit DAG (`git log --all`, branches only). It owns the seed set and
  is therefore **mutually exclusive with `from`** (both set -> `BadValue`).
  It only sets page 1's seeds; subsequent pages use the returned `next` as
  `from` like any other walk. If no branch refs exist it falls back to the
  connection branch HEAD.
- **Output.** A new response field `next`: an array of commit-hash
  strings, the seed set for the following page. **Omitted** when the
  frontier is empty (history exhausted), matching the existing
  omit-on-empty style (`parent1`, `refs`).

A client paginates the whole history by passing each page's `next` back as
the next call's `from`, stopping when `next` is absent:

```js
let from = undefined;
do {
  const page = db.runCommand({ dumboLog: 1, limit: 50, ...(from && { from }) });
  consume(page.commits);
  from = page.next;            // array of hashes, or undefined at the end
} while (from);
```

### 3.2 Filtering

- **Input.** A new `filters` field: a map of collection name -> filter
  specification. The specification mirrors the arguments to `find()`:
  - a **filter document** -- the `find()` filter alone; or
  - a **two-element array** `[filter, projection]` -- the filter plus a
    projection applied to that collection's emitted document bodies
    (Section 6.7), exactly as `find(filter, projection)`.
  A commit is included if it touched (added / removed / modified vs parent1)
  at least one document matching the filter in **any** listed collection (OR
  across collections, mirroring git multi-pathspec `git log -- a b`).
  Semantics are defined in Section 6.
- Absent `filters` means no filtering (today's behaviour).

```js
// filter only:
db.runCommand({ dumboLog: 1, limit: 10,
  filters: { orders: { status: "pending" }, audit: { actor: "alice" } } })

// filter + projection (find(filter, projection) form), per collection:
db.runCommand({ dumboLog: 1, full: true,
  filters: { orders: [ { status: "pending" }, { foo: 1, bar: 1 } ] } })
```

### 3.3 limit semantics

- **No filter:** `limit` counts emitted commits; default 20; `limit: 0`
  short-circuits to empty.
- **With filter:** `limit` counts *matching* commits returned, mirroring
  `git log -n`. The walk continues past non-matching commits until `limit`
  matches are collected or history is exhausted. An optional per-page
  **scan cap** bounds commits examined per call as an operational guard;
  when the cap is hit before `limit` matches, the page returns the matches
  found so far with a non-empty `next`, and the client continues. With the
  scan cap a page may legitimately return fewer than `limit` matches -- or
  zero -- while `next` is still present.

### 3.4 Content options (content increment)

The per-commit content emitted on top of the bare commit metadata:

- **`stat`** (existing): per-collection change counts.
- **`patch`** (existing): document-level field diffs.
- **`full`** (new): for each emitted document, the `patch` diff **plus** a
  parallel `content` field holding the document's full state at that commit.
  `full` is a superset of `patch` for the emitted documents; setting both is
  redundant. Defined in Section 6.7.
- **projection** (new): not a top-level option. A projection is supplied as
  the second element of a per-collection `[filter, projection]` array in
  `filters` (Section 3.2), `find(filter, projection)`-style, and shapes the
  document bodies emitted for that collection. Defined in Section 6.7.

```js
// Follow one document and show its content over time, trimmed to two fields:
db.runCommand({ dumboLog: 1, full: true,
  filters: { orders: [ { _id: 42 }, { status: 1, total: 1 } ] } })
```

When `filters` is set, the output of `stat` / `patch` / `full` is **scoped to
the matched documents** (Section 6.3). `full` requires `filters`, and a
projection only shapes content that `full` (or `patch`) emits.

**Assumption to confirm:** `full` remains the switch that requests document
content; the per-collection projection only *shapes* that content (whole doc
when no projection is given). Filtering alone still returns commit metadata
(plus `stat`/`patch` if asked), not document bodies. If instead the intent is
that filtering -- or supplying a projection -- should by itself return matched
document content without `full`, that is a small change to this section.

## 4. Frontier algorithm

The frontier we must return is exactly the topological iterator's pending
priority-queue contents at the stop point. We reconstruct it in the walk
loop using only data we already have -- the seeds and `ci.Parents` on each
popped commit -- without reaching into iterator internals. Filtering adds a
distinction between commits *examined* (popped) and commits *returned*
(matched):

```
seeds      = resolved input seed hashes (or [branch HEAD])
discovered = set(seeds)            // everything ever pushed onto the queue
examined   = set()                 // every commit popped (matched or not)
returned   = []                    // the page we return, in order

for ci in topoIterator(seeds):
    if ci.IsGhost: continue
    examined.add(ci.Hash)
    for p in ci.Parents:
        if not isGhost(p): discovered.add(p)

    if matchesFilter(ci):          // always true when no filter is set
        returned.append(ci)

    if len(returned) == limit: break
    if scanCapHit(len(examined)):  break    // optional operational guard

next = sort(discovered - examined)          // empty -> omit
```

Three subtleties:

1. **`next` subtracts `examined`, not `returned`.** A commit that was
   examined but filtered out has been fully processed -- its parents were
   pushed -- so it must not reappear in `next`. Subtracting only `returned`
   would cause skipped commits to be re-examined on the next page,
   producing duplicates or a loop.

2. **`discovered` is initialised with the seeds**, not empty. With a
   multi-seed frontier, a seed not reached within the stop condition
   (because higher commits keep getting popped) must still be carried into
   `next`. Initialising with the seed set is what makes Section 7's
   dormant-branch cases work.

3. **Ghost parents are excluded from `next`.** A ghost seed would be
   skipped on the next page (producing no commit), so it must not appear in
   the continuation set.

`sort` (by hash string, or by `(Height desc, timestamp desc, hash)`) is
not required for correctness -- the iterator re-derives its own order from
any seed set -- but a deterministic `next` keeps tests and client logs
stable.

## 5. Why it is correct

Let the **global order** be the sequence the single uninterrupted walk
from the original HEAD would emit. Because `Height` and timestamp are
intrinsic, this order is a fixed total order on the reachable commits. The
*examined* set is what tiles the DAG; the filter merely selects which
examined commits surface in `returned`.

- **No gaps, no overlaps (the examined set tiles the DAG exactly once).**
  At the stop point, `discovered - examined` is precisely the iterator's
  live queue: every commit pushed but not yet popped. Re-seeding a fresh
  iterator with that queue resumes from the same position; the next page's
  first examined commit is the global successor of this page's last
  examined commit.

- **No commit is examined twice across pages.** Suppose page P examines X
  and a later page re-examines X. The later page only walks ancestors of
  its seeds (the frontier from P). For X to be re-examined, X must be an
  ancestor of some frontier seed F. But in newest-first topological order
  an ancestor is always emitted *after* its descendant; F was not examined
  on page P (it is in the frontier), so X -- an ancestor of F -- cannot
  have been examined on page P either. Contradiction.

- **Within a page, the iterator dedupes** commits reachable by multiple
  paths (its existing diamond-merge behaviour). This also covers a frontier
  that contains a commit and one of its own ancestors simultaneously
  (Section 7.6).

Because the examined set tiles the DAG once and the filter is a pure
function of a commit and its parent1, the filtered output is identical
whether produced in one walk or paginated: concatenating `returned` across
pages equals the single full filtered walk (the master invariant,
Section 7.1).

## 6. Commit filtering semantics

### 6.1 Touched, not presence

Empirically verified against git (a five-commit repo touching `orders.txt`
and an unrelated file): all of git's content filters operate on what a
commit *changed*, never on the snapshot's full contents. A commit that
does not touch the path is excluded even though the path still contains the
matching content at that commit. `dumboLog` follows suit: presence-based
filtering (run `find()` against the full snapshot at each commit) is **not**
implemented. The decisive reasons: presence floods results (a long-lived
matching doc marks every commit from its insertion to HEAD) and costs a
full collection scan per commit, whereas touched semantics reuse the cheap
per-commit parent diff.

### 6.2 Pathspec, not pickaxe (the c3 decision)

Git itself has two "touched" definitions that differ on one case -- an edit
to an already-matching item that leaves it still matching:

```
c1 add, becomes pending     git log -- path: shown   git log -S: shown
c3 edit, stays pending       git log -- path: SHOWN   git log -S: HIDDEN
c4 pending -> shipped        git log -- path: shown   git log -S: shown
```

We mirror **pathspec** (`git log -- path`), git's default content filter:
include a commit if any document it changed matches the filter, even if the
document was matching before and stays matching. Mapped to documents and
evaluated against parent1:

- **added** doc: matches if the post-image matches the filter.
- **removed** doc: matches if the pre-image matches the filter.
- **modified** doc: matches if *either* the pre- or post-image matches.

Worked cases for filter `{status: "pending"}`:

| change                         | included | why                         |
|--------------------------------|:--:|-----------------------------------|
| add a pending doc              | yes | post matches                     |
| remove a pending doc           | yes | pre matches                      |
| edit pending -> pending (c3)   | yes | both match                       |
| edit pending -> shipped (c4)   | yes | pre matches                      |
| edit shipped -> pending        | yes | post matches                     |
| edit shipped -> shipped        | no  | neither image matches            |
| add/remove/edit a non-matching | no  | no matching changed doc          |

Pickaxe (`-S`, "match-set count changed", which would exclude c3) is
explicitly deferred (Section 9).

### 6.3 Per-collection map, OR across collections

`filters` is `{collName: filterDoc, ...}`. A commit is included if it
touched a matching document (per 6.2) in **any** named collection. This
mirrors git multi-pathspec (`git log -- a b` is OR over `a`, `b`). A
collection named in `filters` that does not exist at a given commit
contributes no matches there.

**Scope (content increment).** When `filters` is set, the filter does more
than gate inclusion: it also **scopes the content** of `stat` / `patch` /
`full` to the matched documents. A returned commit reports only the
collections named in `filters`, and within them only the documents that
matched (per 6.2) and changed in that commit -- not every collection the
commit happened to touch. This mirrors `git log -p -- pathspec`, where the
patch is restricted to the pathspec.

> This **revises** the originally shipped behaviour, in which a filtered
> commit's `stat` / `patch` reported all changed collections. With no
> `filters`, `stat` / `patch` remain whole-commit as before.

### 6.4 Merges evaluated against parent1

"Touched" is evaluated against parent1 only, consistent with how `stat` /
`patch` already diff (`backend.go:2282`). We deliberately do **not**
replicate git's merge history simplification (TREESAME pruning across
parents): it is confusing and fights the frontier model. A merge commit is
included iff it changed a matching document relative to its first parent.
The root commit is compared against the empty tree (all documents are
"added"), as git does.

### 6.5 Query coverage and cost

The filter document is matched with the registered `common.FilterDocument`
predicate (Section 2), so operator coverage equals normal `find()`. Only
the filter document is accepted -- no projection, sort, or skip, since we
need only a yes/no "did a matching doc change." Cost is proportional to the
number of changed documents in each examined commit (the parent1 diff we
already compute), not to collection size or history length.

### 6.6 Filtering by `_id` (the common case)

The most common filter is by `_id` -- "show me the history of this one
document," the document-level analog of `git log -- <file>`. It is special
enough to call out:

- **`_id` is immutable.** MongoDB forbids changing a document's `_id`, so a
  document's membership in an `_id` match set changes only by **insert**
  (add) or **delete** (remove), never by modification. There is no
  "modified into / out of the match set" case for `_id` predicates.
- **Every change to a matched `_id` is a touch.** Because the pre- and
  post-images of a modified document share the same (matching) `_id`, the
  pathspec rule (6.2) includes every insert, every field modification, and
  every delete of that `_id`. The c3 "edit-stays-matching" case is total
  and automatic for `_id` filters -- which is exactly the desired
  "follow one document" behaviour.
- **Delete-then-reinsert** of the same `_id` yields two touching commits (a
  remove and a later add).
- **The diff is already keyed by `_id`.** The per-commit parent1 diff groups
  changes by document `_id`. An `_id`-equality or `_id: {$in: [...]}`
  predicate therefore maps directly onto the set of changed `_id`s; an
  implementation MAY short-circuit such predicates against the diff keys
  rather than running `FilterDocument` per changed document, but MUST
  preserve `find()` semantics -- in particular numeric `_id` type coercion
  (int32 / int64 / double compare equal, per `scalarMatch`) and ObjectId
  equality.

### 6.7 The `full` snapshot and `project` (content increment)

`stat` and `patch` are *change* views. `full` adds a *snapshot* view for the
matched documents: per emitted document it carries the `patch` diff **and** a
parallel `content` field with the document's full state at that commit.

Per matched, changed document, by status:

| status   | diff (as `patch`) | `content` field                          |
|----------|-------------------|------------------------------------------|
| added    | shows it added    | the document (its post-image)            |
| modified | shows field diffs | the post-image (state at this commit)    |
| deleted  | shows it removed; entry marked deleted | **absent** (no document exists at this commit) |

Notes:

- `full` is a superset of `patch` for the emitted (scoped) documents. The
  genuinely new data is on **modified** documents -- for added the post-image
  equals the added doc and for deleted there is no content, both already
  implied by the `patch` diff. Setting `patch` alongside `full` is redundant.
- The `content` of a modified document is always the **post-image**, even when
  the document matched only via its pre-image (e.g. `pending -> shipped` under
  `{status:"pending"}`): the commit is included because it changed a matching
  document, and `content` reports that document's state at the commit
  (`shipped`). "Content at the commit" has no pre-image meaning for a snapshot.
- Output is **scoped** (6.3): only matched documents in the filtered
  collections appear. `full` therefore requires `filters`.

**Projection.** A projection is supplied per collection as the second element
of a `[filter, projection]` array in `filters` (Section 3.2), mirroring
`find(filter, projection)`. It shapes the document bodies emitted for **that
collection** -- the `full` `content` field and the full added/removed
documents in `patch`/`full`. It follows `find()` projection rules (inclusion
`{f:1}` vs exclusion `{f:0}`, `_id` retained unless excluded) and reuses the
existing projection engine
(`internal/handler/common/aggregations/stages/projection`), applied in the
handler as it builds the response. It does not alter the field-level diff of a
modified document (the diff is a delta, not a body). The filter-only form
(plain document value) means no projection -- the whole document is emitted. A
projection only takes effect when document bodies are emitted (`full`, or
`patch`'s add/remove bodies); supplied without them it has nothing to shape.

## 7. Testing

All scenarios are Go tests. Backend cases use the helpers in
`internal/backends/dolt/diff_test.go`: `newTestBackend`, `insertDoc`,
`commitDB`, plus `DumboDBBranch` / `DumboDBMerge` for shaping the DAG.
Handler cases drive `MsgDumboDBLog` through `wire.OpMsg`.

**Deterministic ordering is mandatory.** The iterator breaks `Height` ties
by timestamp, so any test with height-tied commits MUST set
`CommitParams.Timestamp` explicitly (`versioning.go:35`) rather than
relying on wall-clock commit times. The convention below: mainline commits
carry newer timestamps than feature commits, so on a height tie the
mainline drains first. Tests assert on commit *messages* (stable, chosen
by the test) rather than hashes where possible, translating `next` and
filter results back to messages for readability.

`tests/bats/` is owner-managed and off-limits (`AGENTS.md`). No bats files
are added or changed by this work.

### 7.1 Master invariant: pagination equals the full walk

The umbrella property test, run as a table over every DAG shape in this
section, over `limit` in `{1, 2, 3, 5, 7, 50}`, and over
{no filter, a filter that matches every commit, a sparse filter}:

1. Single full walk: `DumboDBLog` with `limit` >= total matching count, no
   `from`. Record the ordered list `full`.
2. Paginated walk: start with no `from`; repeatedly call with the chosen
   `limit`, feeding `next` back as `from`, until `next` is absent.
   Concatenate pages into `paged`.
3. Assert `paged == full` (same length, same order, element-wise).
4. Assert every commit appears exactly once in `paged`.
5. Assert the final page omits `next` and no earlier page does (modulo the
   scan-cap case in 7.13, which is exercised separately).

This single property subsumes most per-scenario assertions; the explicit
scenarios pin down per-page frontier contents and filter membership.

### 7.2 Linear history -- single-commit frontier

100 commits on `main`, no branches, `limit = 10`:

- Page 1 emits c100..c91, `next = [c90]`.
- Page 2 seeds `[c90]`, emits c90..c81, `next = [c80]`.
- ... 10 pages; the frontier is always exactly one commit.
- Final page omits `next`.

Assert each page size and that `next` is a single-element array until the
last page.

### 7.3 Diamond

```
      M           M  = merge of L and R
     / \          L  = left child of base
    L   R         R  = right child of base
     \ /          B  = "Initialize database" base
      B
```

`limit = 2`, timestamps chosen so L precedes R:

- Page 1: emit `M`, `L`; `next = sort({R, B})`.
- Page 2 seeds `[R, B]`: emit `R` then `B` (reached via two paths, emitted
  once); `next` omitted.

Assert `B` appears exactly once across the two pages.

### 7.4 Long mainline with an old feature merge -- one dormant seed across pages

This is the scenario the linear-cursor approach gets wrong, and the primary
reason the continuation token is a set. A feature branched off early, was
merged late, and its tip has a low `Height`; the mainline drains for many
pages while the feature tip sits in the frontier untouched.

Worked example, heights in parentheses, `I` is the init commit:

```
I(1) - a1(2) - a2(3) - a3(4) - a4(5) - a5(6) - a6(7) - a7(8) - a8(9) - M(10)
                  \                                                   /
                   b1(4) ------------- b2(5) ------------------------
```

`M` merges `feature` (tip `b2`) into `main` after `a8`; parents `a8(9)`,
`b2(5)`. Feature commits carry older timestamps than `a3..a8`, so height
ties resolve mainline-first.

`limit = 3`:

| Page | seeds (in)   | emitted     | next (out)   |
|------|--------------|-------------|--------------|
| 1    | [M] (HEAD)   | M, a8, a7   | {a6, **b2**} |
| 2    | [a6, b2]     | a6, a5, a4  | {a3, **b2**} |
| 3    | [a3, b2]     | b2, a3, b1  | {a2}         |
| 4    | [a2]         | a2, a1, I   | -- (omitted) |

`b2` (height 5) is carried in `next` on pages 1 and 2, never emitted, then
emitted on page 3 once the mainline frontier descends to height 4 (`a3`)
and `b2` becomes the queue maximum. Concatenation
`M, a8, a7, a6, a5, a4, b2, a3, b1, a2, a1, I` -- 12 commits, each once,
identical to the unbroken walk.

**Scaled variant (the real test).** Mainline of 60 commits, feature of 2
commits branched at `a2` and merged at the tip, `limit = 5`. `b2` stays
dormant for roughly the first ten pages. Assertions:

- `b2`'s hash appears in `next` of every page from page 1 until the page
  immediately before it is emitted.
- `b2` does NOT appear in any page's `commits` before that page.
- `b2` appears exactly once in `commits`, on the page where the mainline
  frontier height drops to `Height(b2)`.
- The merge base `a2` is emitted exactly once, after both `a3` and `b1`.
- Master invariant (7.1) holds across all listed limits.

A parameterised version sweeps the branch point (early vs late) and the
mainline length so the number of dormant pages ranges from 1 to ~20,
guarding against an off-by-one where the dormant seed is emitted one page
too early or too late.

### 7.5 Two dormant seeds simultaneously

Two independent features merged at different points, both with low-height
tips:

```
I - a1 - a2 - a3 - a4 - a5 - a6 - a7 - a8 - M1 - a9 - a10 - M2
           \                          /            \        /
            f1 - f2 ------------------              g1 - g2
                 (merged at M1)                     (merged at M2)
```

With a small `limit`, after the first page both `f2` and `g2` can be live
in the frontier at once. Assert `next` can contain **two** dormant feature
tips simultaneously, each carried across the expected number of pages, each
emitted exactly once at its correct height. This proves the frontier is a
genuine set, not a single carried-over pointer with special-casing.

### 7.6 Overlapping seeds -- a commit and its own ancestor

A frontier can legitimately contain a commit and one of its ancestors at
once (both pushed, neither popped). Construct a page boundary that produces
`next = [X, P]` where `P` is an ancestor of `X`. Re-seed the next page with
`[X, P]` and assert:

- `P` is emitted exactly once (not once per path that reaches it).
- The order is still the global order; no commit between `X` and `P` is
  skipped.

This pins down that `GetTopologicalOrderIterator` dedupes overlapping
seeds. If it does not dedupe natively, the backend must dedupe the resolved
seed set before constructing it.

### 7.7 Exhaustion and boundary conditions

- **Exact fit.** `k * limit` commits paginate into `k` pages; page `k`
  omits `next`; page `k-1` does not.
- **limit >= history.** One page returns everything, `next` omitted.
- **limit = 1.** Degenerate paging; master invariant still holds; `next` is
  a single seed each page (or a set immediately after a merge).
- **Single root only.** A repo with just the init commit: one page, one
  commit, no `next`.

### 7.8 Pagination input handling (backend + handler)

- **`from` as string (back-compat).** Existing single-hash form resolves to
  a one-element seed set and still walks merge parents. The documented
  `from` example in `docs/COMMANDS.md` continues to pass.
- **`from` as array.** A two-element array seeds the iterator with both;
  result equals seeding them in one walk.
- **`from` absent.** Defaults to `[branch HEAD]`.
- **Invalid hash in `from` (string or any array element).** Returns the
  existing `invalid from hash` / `commit not found` errors (`backend.go:2182`,
  `:2188`); a bad element is not silently dropped.
- **`from` array element of wrong BSON type.** Handler rejects with a
  parameter type error before reaching the backend.
- **`limit: 0`.** Handler short-circuit still returns empty `commits` and no
  `next`.
- **Wire round-trip.** A handler test pages a multi-commit history end to
  end by feeding `next` back into the next `runCommand`, asserting
  concatenation and final `next` omission (the loop in Section 3.1).

### 7.9 Interaction with stat / patch

With `stat: true` (and separately `patch: true`) over a paginated multi-page
walk:

- Each commit's `stat` / `diff` is identical to its entry in the
  unpaginated walk (still diffed against parent1).
- A merge commit emitted on a later page still diffs against parent1.
- `next` is independent of `stat` / `patch`.

### 7.10 Determinism

Re-running the same paginated walk (same DAG, seeds, limit, filter) yields
byte-identical pages, including `next` ordering. A fixed DAG is run twice
and asserted equal, guarding against map-iteration order leaking into
`next`.

### 7.11 Filter semantics -- the per-document touched table

Reproduce the Section 6.2 table directly. One collection `orders`; build
commits, each setting `CommitParams.Timestamp`, that exercise every row:
add-pending, edit-pending-to-pending (c3), edit-pending-to-shipped,
edit-shipped-to-pending, edit-shipped-to-shipped, remove-pending, plus
commits that touch only an unrelated collection. With
`filters: {orders: {status: "pending"}}` assert exactly the included rows
are returned and the excluded rows (edit shipped->shipped, unrelated-only)
are not.

The **presence guard** (the decisive git behaviour, Section 6.1): a pending
doc inserted early and never touched again; subsequent commits modify only
other documents/collections. Assert those subsequent commits are **not**
returned even though the pending doc is present in their snapshots.

### 7.12 Filter scoping -- per-collection OR

`filters: {orders: {status:"pending"}, audit: {actor:"alice"}}` over a
history where some commits touch a matching `orders` doc, some touch a
matching `audit` doc, some touch both, and some touch neither (or touch
non-matching docs in those collections). Assert:

- Commit touching a matching doc in either collection is included (OR).
- Commit touching both is included once.
- Commit touching neither (or only non-matching docs) is excluded.
- A commit that creates collection `audit` for the first time with a
  matching doc is included; commits before `audit` exists are unaffected by
  the `audit` filter.

### 7.13 Filter + limit counts matches (git -n)

A history with many non-matching commits interleaved between sparse
matching commits. With `limit = 3` and a filter:

- The page returns exactly 3 matching commits, having walked past the
  intervening non-matching commits.
- `next` is the frontier (examined-set continuation) after the 3rd match,
  positioned so the next page resumes at the global successor of the last
  *examined* commit -- not the last matched commit.
- Concatenating filtered pages equals the single filtered full walk
  (master invariant with the sparse filter).

**Scan cap** (if adopted): set the cap low enough that a page hits it before
collecting `limit` matches. Assert the page returns the matches found so far
(possibly zero) with a **non-empty** `next`, and that continuing from `next`
eventually yields the full filtered result with no duplicates or gaps. This
is the one case where an intermediate page may carry `next` while returning
fewer than `limit` matches.

### 7.14 Filter + dormant branch

Combine 7.4 with a filter: place a matching commit on the dormant feature
branch (low height) and matching commits on the mainline. Assert the
feature-branch match is surfaced only on the page where the feature tip is
finally examined (not earlier), that the dormant seed is carried in `next`
meanwhile (carry is independent of the filter -- it tracks examined, not
matched), and that the filtered concatenation equals the filtered full
walk.

### 7.15 Filter + merge against parent1

A merge commit whose merged result changes a matching document relative to
parent1 is included; a merge that is identical to parent1 for the filtered
collections is excluded even if parent2 differs. Assert the parent1 rule
(Section 6.4) explicitly, and that the root commit is included when it adds
a matching document (diff vs empty tree).

### 7.16 Filter + empty results and invalid filters

- A filter that matches nothing walks to EOF, returns empty `commits`, and
  omits `next`.
- An invalid filter document (bad operator) surfaces the same error
  `find()` would, before or during the walk, rather than silently matching
  nothing.

### 7.17 Filtering by `_id` -- follow one document

The canonical case, given dedicated coverage. Build a history in which doc
`_id = K` in collection `orders` is inserted, modified twice, then deleted,
interleaved with many commits that touch only other `_id`s or other
collections. With `filters: {orders: {_id: K}}` assert:

- Exactly the insert, both modifications, and the delete of `_id = K` are
  returned, newest-first.
- Every modification of `_id = K` is included even when only unrelated
  fields change (the immutable-identity consequence of 6.6); no
  modification is ever dropped.
- Commits that touch only other documents/collections are excluded, even
  though `_id = K` is present in their snapshots (presence guard).
- The delete commit is included (pre-image identity match).
- A later re-insert of the same `_id = K` is also included; the remove and
  the re-add are distinct touching commits.
- Results are identical whether produced in one walk or paged (master
  invariant, 7.1); with a small `limit` the page boundaries skip examined
  non-matching commits exactly as in 7.13, and `next` carries the
  examined-set frontier, not the matched commits.

### 7.18 `_id` filter type and operator coverage

`_id` takes many BSON types; the touched predicate must key on `_id` the
same way the diff does. Table-driven cases:

- **ObjectId `_id`** (MongoDB default): track a document by its ObjectId;
  `filters: {coll: {_id: ObjectId("...")}}` matches its insert / modify /
  delete commits.
- **Numeric `_id` with cross-type coercion:** a document stored with
  `_id: NumberLong(5)` is matched by `filters: {coll: {_id: 5}}` (a double
  off the wire) and vice versa, mirroring `find()` and the fact that Mongo
  treats them as the same document. This is the trap that a naive
  byte-equality short-circuit (6.6) would get wrong.
- **String `_id`.**
- **`_id: {$in: [k1, k2, k3]}`** tracks several documents at once (OR over
  ids, like multi-pathspec): a commit touching any listed `_id` is
  included.
- **`_id: {$gte: a, $lt: b}`** range: a commit touching any `_id` in the
  range is included.
- **`_id` across a dormant branch** (compose with 7.14): a document touched
  only on the low feature branch surfaces only when that branch's tip is
  examined, never earlier.
- **`_id` in a per-collection map** (`{orders: {_id: 1}, audit: {_id: 9}}`)
  ORs across collections by id.

### 7.19 Scope -- filtered stat / patch report only matched documents

Build a commit that changes both a matching and a non-matching document, in
a filtered collection and in an unfiltered collection. With `filters` +
`stat` (and separately `patch`):

- Only the filtered collection appears; the unfiltered collection it also
  touched is absent.
- Within the filtered collection, only the matching document is counted /
  diffed; the non-matching document changed in the same commit is absent.
- The same query with **no** `filters` still reports all changed collections
  (whole-commit), confirming scope is filter-driven (regression guard for the
  revised Section 6.3 behaviour).

### 7.20 The `full` snapshot

With `filters` + `full` over a document's lifecycle (add, modify, delete):

- An **added** doc carries the diff plus `content` equal to the document.
- A **modified** doc carries the field diff plus `content` equal to the
  post-image; assert `content` is the post-state even when the doc matched
  only via its pre-image (`pending -> shipped` under `{status:"pending"}`
  yields `content.status == "shipped"`).
- A **deleted** doc is marked deleted and carries **no** `content`.
- Output is scoped (7.19): only matched docs appear.
- `full` without `filters` is rejected.
- `full` and `patch` together produce the same diffs as `full` alone
  (superset; no duplication or conflict).

### 7.21 Projection (the `[filter, projection]` form)

With `full` and `filters: { coll: [ {<filter>}, {a: 1} ] }`:

- The array form parses into the same filter plus a per-collection projection;
  the filter behaves identically to the plain-document form.
- Each `content` body for `coll` contains only `a` (and `_id` unless excluded).
- Full added/removed document bodies for `coll` are likewise projected.
- Field-level diffs of modified docs are unchanged (projection shapes bodies,
  not deltas).
- Exclusion projection (`{a: 0}`) drops `a` and keeps the rest.
- Per-collection: a projection on one collection does not affect another
  filtered collection's bodies.
- A malformed spec (array length != 2, or a non-document filter/projection
  element) is a parameter error.

## 8. Interface and type changes

- `internal/backends/versioning.go`
  - `LogParams.From string` -> `LogParams.From []string`.
  - Add `LogParams.Filters map[string]*types.Document` (collection ->
    filter doc).
  - Add `LogResult.Next []string`.
  - Add `LogParams.ScanCap int` (0 = unbounded) if the scan cap is adopted.
- `internal/handler/msg_dumbodb_versioning.go`
  - Parse `from` via `document.Get("from")` with a type switch over
    `string` and `*types.Array` (each element a string); build `[]string`.
  - Parse `filters` as a document of collection -> filter document; build
    the map. Absent -> nil (no filtering).
  - After the backend call, append `next` to the response only when
    non-empty.
- `internal/backends/dolt/backend.go` (`DumboDBLog`)
  - Resolve `[]string` seeds (existing hash parse/validate; empty -> branch
    HEAD).
  - Pass all seeds to `GetTopologicalOrderIterator`.
  - Track `discovered` / `examined` / `returned`; compute and sort `next`;
    filter ghosts.
  - When `Filters` is set, evaluate the touched predicate (Section 6) per
    examined commit using the existing parent1 diff lists and the
    registered `common.FilterDocument`.
- Fix the in-repo caller of the changed signature: the read-only
  `DumboDBStatus` path builds `&backends.LogParams{... Limit: 1}` with no
  `From` (`msg_dumbodb_versioning.go:1303`); a nil `From` slice and nil
  `Filters` preserve its behaviour. Audit for any other `LogParams{...}`
  construction.
- `docs/COMMANDS.md`: document the `from` array form, the `next` field, the
  `filters` map with touched/pathspec semantics, the per-collection OR rule,
  filtered-`limit` behaviour, and a full-history pagination loop example.

### Content increment (not yet built)

- `internal/backends/versioning.go`
  - Add `LogParams.Full bool`. The backend needs the per-collection **filter**
    only; `LogParams.Filters map[string]*types.Document` is unchanged. The
    projection is a presentation concern handled in the handler (below), so no
    backend projection field is added.
  - `CommitInfo.Diff` carries the scoped per-collection content. Extend the
    modified-document entry to carry the full post-image alongside its field
    diff (e.g. add a `Content *types.Document` to `ModifiedDoc`, populated
    only when `Full` is set). Added/removed entries already carry full bodies.
- `internal/backends/dolt/backend.go` (`DumboDBLog`)
  - When `Filters` is set, **scope** the `stat` / `patch` / `full` computation
    to matched documents in the filtered collections (filter the
    added/removed/modified lists through `common.FilterDocument`), instead of
    diffing every changed collection. This replaces the current whole-commit
    diff in the filtered path.
  - When `Full` is set, populate the modified entries' post-image content
    (deleted entries carry none).
- `internal/handler/msg_dumbodb_versioning.go`
  - Extend the `filters` parser: each value is a filter **document** (today's
    form) **or** a two-element `[filter, projection]` array. The filter goes
    to `LogParams.Filters`; the projection is retained handler-side in a
    per-collection map. Reject a malformed array (length != 2 or non-document
    elements) and `full` without `filters`.
  - Parse `full` (bool). Emit the `content` field per emitted document; apply
    each collection's projection to that collection's full document bodies
    (content + added/removed) via the common projection engine before writing
    the response. (Backend `Filters` type and the touched predicate are
    unchanged -- projection never reaches the backend.)
- `docs/COMMANDS.md` and `docs/verify/log-pagination-filtering.md`: document
  `full`, the `[filter, projection]` array form, and the scoped-output
  behaviour; revise the existing stat/patch-composition scenario (verify B5),
  which currently asserts whole-commit content.

## 9. Risks

- **Seed dedupe in the iterator.** Section 7.6 verifies overlapping seeds.
  If `GetTopologicalOrderIterator` does not dedupe natively, the backend
  dedupes the resolved seed set before constructing it. Locked by a test.
- **Frontier size growth.** Wide octopus merges or many concurrent dormant
  branches enlarge `next`. Inherent to correct DAG pagination, bounded by
  the number of live branches in the queue; no cap imposed. Worth a code
  comment.
- **Unbounded filtered walk.** With a sparse filter and no scan cap, a
  single call can traverse the whole history to collect `limit` matches
  (exactly git `-n` behaviour, but git streams while we block per call).
  The optional scan cap (3.3, 7.13) is the operational mitigation; the
  contract stays "limit = matches."
- **Client misuse of `next`.** A stale or hand-edited `next` is treated as
  an ordinary seed set; results are well-defined (a walk from those seeds)
  but may not align with a prior page. `next` is transparent (raw hashes),
  not a signed opaque token, by explicit design decision.

## 10. Out of scope

- **Pickaxe (`-S`) / match-set-count semantics** (excludes the c3 case).
  The chosen pathspec semantics (6.2) is the default; a future `mode` flag
  could add pickaxe.
- **Filtering on commit metadata** (author, date range, message grep).
- **Range syntax (`A..B`) and `--first-parent`.**
- **Git merge history simplification** (TREESAME pruning across parents).
- **Pre-image / before-and-after `content` for modified docs.** `full` emits
  the post-image only; the pre-image is recoverable from the field diff.
- **A real MongoDB cursor with `getMore`.** The frontier-seed scheme is the
  pagination contract; layering a cursor on top, if ever wanted, is a
  separate change.
