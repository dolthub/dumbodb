# Merge Modes

**Issues:** workspace-mxv; motivated by workspace-dn0; optimistic locking
(workspace-wdv) is deferred behind this
**Date:** 2026-08-26
**Status:** Design. Behaviour specification only -- testing and implementation
are out of scope until the behaviour below is agreed.
**Problem:** the standard MongoDB compare-and-swap does not work in dumbodb.
Section 1.

## 1. The problem

The standard way to do optimistic locking in MongoDB is a compare-and-swap on a
sequence field:

```js
db.docs.updateOne(
  { _id: 1, v: 7 },                                  // CAS condition
  { $set: { status: "review" }, $inc: { v: 1 } }
)
// n: 1 -> we won.  n: 0 -> someone else moved it; re-read and retry.
```

**In dumbodb this pattern does not work.** Two clients that both read `v: 7`
can both be told `n: 1`, and one of the two updates is then discarded. Neither
client is told anything. The counter reads 8 when two increments were
acknowledged.

### The proposal: four "mergeModes"

Branches, be they long lived or brief transactional update serialization,
need to merge. Merging requires conflict resolution, and currently Dumbo
follows Dolt's behaviour: row-level conflicts are handled column by column
and converging values (both branches modify to equal values) are accepted
and non-conflicting.

In order to solve the problem above, Dumbo will introduce configurable merge
conflict handling at the collection level. We'll call them mergeModes.

Each mergeMode is named for **what triggers a conflict**. `Touched` means a side
wrote it at all; `Divergent` means the two sides wrote it differently. The unit
is either the whole `document` or an individual `field`.

| mode | conflicts when |
|---|---|
| `documentTouched` | both sides wrote anything in the same document |
| `fieldTouched` | both sides wrote **the same field**, even to the same value |
| `documentDivergent` | both sides wrote the same document *and the results differ* |
| `fieldDivergent` | both sides wrote **the same field** *and the values differ* |

`fieldTouched` is the new default; `fieldDivergent` is today's only behaviour.

### What each mergeMode does

`merge` means the merge proceeds. `CONFLICT` means it does not.

| scenario | `documentTouched` | `fieldTouched` | `fieldDivergent` | `documentDivergent` |
|---|---|---|---|---|
| only one side changed the document | merge | merge | merge | merge |
| different fields, same document | CONFLICT | merge | merge | CONFLICT |
| **same field, same value** (the CAS race) | CONFLICT | **CONFLICT** | **merge** | merge |
| same field, same value, plus a disjoint edit | CONFLICT | CONFLICT | merge | CONFLICT |
| same field, different values | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| modify vs delete | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| add/add same `_id`, identical content | CONFLICT | CONFLICT | merge | merge |
| add/add same `_id`, different content | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| both sides deleted | CONFLICT | CONFLICT | merge | merge |

Row 3 is the reason this document exists: two compare-and-swap updates that
agree on the resulting value are read as agreement, and one of them is
discarded. Section 6 traces that row key by key.

Row 2 separates `fieldTouched` from `documentDivergent`, and row 4 separates
`fieldDivergent` from `documentDivergent`. Section 5 gives the predicates the
table is derived from.

### They do not form a line

The four sit in a lattice, not a strictness ranking. Higher catches everything
lower catches, and more:

```
                documentTouched              strictest: any concurrent write
                 /           \
        fieldTouched     documentDivergent   incomparable with each other
                 \           /
                fieldDivergent               loosest: only differing values
```

**`fieldTouched` and `documentDivergent` are incomparable.** Neither implies
the other, so no list of the four can be read as strict-to-loose. Both
directions have witnesses:

```
fieldTouched conflicts, documentDivergent merges -- same field, same value:
  base   { a: 0, b: 0 }
  ours   { a: 0, b: 1 }      both wrote b, so fieldTouched conflicts
  theirs { a: 0, b: 1 }      but the documents are identical, so
                             documentDivergent merges

documentDivergent conflicts, fieldTouched merges -- disjoint fields:
  base   { a: 0, b: 0 }
  ours   { a: 0, b: 1 }      no field in common, so fieldTouched merges
  theirs { a: 1, b: 0 }      but the documents differ, so
                             documentDivergent conflicts
```

Making a document divergent requires touching *a* field, but `fieldTouched`
asks whether both sides touched **the same** field. Each side touching some
field of its own is a weaker condition than the two sides colliding on one
field, so `documentDivergent` does not imply `fieldTouched`.

Worked against one scenario -- branch A sets `status`, branch B sets `owner`,
and both `$inc` the same `v` from 7 to 8:

| mode | verdict |
|---|---|
| `documentTouched` | conflict; both wrote the document |
| `fieldTouched` | conflict; both wrote `v` |
| `documentDivergent` | conflict; both wrote the document and the results differ |
| `fieldDivergent` | **merges**; `v` is 8 either way, `status` and `owner` are disjoint |

That last row is the behaviour this document changes -- row 4 of the table
above, made concrete. Section 6 traces it key by key.

### Where this stands today

There is exactly one behaviour, `fieldDivergent`, and it is not configurable.
**It silently loses updates on every merge path**, in every server mode:
explicit `dumboMerge` of two branches, `dumboRebase`, `dumboCherryPick`,
`dumboRevert`, and the implicit merge at `dumboCommit`. Section 2.1 is the
proof and section 3 names the exact line responsible. The
default becomes `fieldTouched` (section 8).

The modes decide what counts as a collision. A separate defect stops an
ordinary write reaching a merge at all, so it has to be fixed before the modes
govern anything but `dumboMerge`; see `docs/design/working-set-publish.md`.

## 2. The problem, measured

The compare-and-swap of section 1 fails because of what the merge does with two
changes that agree.

### 2.1 Any merge of two divergent lines

Measured with two explicit branches and an explicit `doltMerge`. Baseline
`{_id: 1, v: 1, a: "s", b: "s"}` committed, branch `feature` created, then
each branch runs the same CAS on `v == 1`:

| scenario | branch CAS matched | `doltMerge` | final |
|---|---|---|---|
| each branch sets a different field and `$inc`s `v` | main 1 / feature 1 | **`ok: 1`, no conflict** | `{a: "A", b: "B", v: 2}` |
| each branch only `$inc`s `v` | main 1 / feature 1 | **`ok: 1`, no conflict** | `{v: 2}` |

Two CAS-guarded increments from `v: 1` and the counter reads 2. The merge
reported success. Nobody was told anything.

**The loss is in the merge itself**, so it reaches every path that merges:
`dumboMerge`, `dumboRebase`, `dumboCherryPick`, `dumboRevert`, and the
commit-time merge. Any two lines of history that both ran the compare-and-swap
produce this, however they came to diverge.

### 2.2 Why convergence is the wrong test

The reason is that the merge treats a field both sides changed to the *same*
value as agreement. A disciplined CAS protocol -- everybody increments by
exactly one -- **guarantees** the two sides converge, which guarantees a clean
merge, which guarantees the lock is defeated. Increment by inconsistent amounts
and the merge catches it instead. The mechanism fails safe only when it is
misused.

For any field derived from its own prior value -- `$inc`, `$mul`, `$push`,
`$addToSet`, `$bit` -- **convergence is coincidence, not agreement.** By the
time the merge runs the derivation is gone and only the value remains. Two
writers arriving at 8 from 7 did two increments, not one.

Under `--session-isolation` the per-session fork and the `dumboCommit` merge
are both implicit, so "avoid merges" is not a state a user can arrange or even
observe. This is what two ordinary concurrent clients get.

## 3. Two independent knobs, and only one of them is the bug

**Knob A -- comparison granularity.** Which units the merge compares.
Ground truth: `mergeBSONDoc` (`internal/backends/dolt/bson_merge.go:27`)
enumerates **top-level keys only** and compares whole values with
`reflect.DeepEqual`. Subdocuments and arrays are atomic values.

**Knob B -- the conflict trigger.** Given a unit that both sides changed
relative to base, is that a conflict? Today:

```go
case bOK && lOK && rOK:
    leftUnchanged  := reflect.DeepEqual(lVal, bVal)
    rightUnchanged := reflect.DeepEqual(rVal, bVal)
    sameMod        := reflect.DeepEqual(lVal, rVal)
    switch {
    case leftUnchanged && rightUnchanged: out.Set(k, lVal)
    case leftUnchanged:                   out.Set(k, rVal)
    case rightUnchanged:                  out.Set(k, lVal)
    case sameMod:                         out.Set(k, lVal)   // <-- the bug
    default:                              return nil, true
    }
```

**`case sameMod` is the lost update.** Both sides changed `v` from 7 to 8, so
`leftUnchanged` and `rightUnchanged` are both false and `sameMod` is true, and
the field merges. That single branch is the entire difference between
`fieldDivergent` and `fieldTouched`.

**The four modes vary knob B. The CAS fix is knob B alone.** `v` is a
top-level scalar, so knob A already isolates it correctly; nothing about key
enumeration needs to change to fix the CAS. But knob B does have to change:
`fieldTouched` means deleting the `case sameMod` escape, so **this is a change
to the differ**, not a no-op. Knob A is a separate question, still open
(section 9).

## 4. What this requires from Dolt

A dumbodb collection is a Dolt table with two columns: `_id binary(20)` as the
primary key, and `doc longblob` holding the whole BSON document
(`internal/backends/dolt/helpers.go`). So every document is a **single cell**.

That is the whole dependency, and it has two consequences.

**Dolt cannot make these decisions.** Its merge decides per cell, and every
field of a document lives in one cell. `fieldTouched` and `fieldDivergent` ask
questions about the fields inside that cell, which Dolt has no way to see.
dumbodb has to answer them.

**Deferring to Dolt gives the wrong answer, not a weaker one.** Two writers
editing disjoint fields both change the one `doc` cell, to different values, so
Dolt's cell rule conflicts. `fieldTouched` says merge. dumbodb therefore has to
compose the merged document which doesn't exist on either branch, and hand it
back. Classification isn't enough.

### The hook Dolt does not have

Dolt's three-way differ classifies two byte-identical edits as a convergent
edit and merges them **before** consulting anything
(`go/store/prolly/tree/three_way_differ.go`), and does the same for two sides
deleting the same row. The fast prolly-tree merge elides convergent edits the
same way, and above the leaf level skips whole subtrees the two sides rewrote
identically, never visiting the rows inside.

The convergent case is exactly the CAS race of section 1. A hook placed after
that classification can express `fieldDivergent` and `documentDivergent` and
neither `Touched` mode.

### What Dolt needs to provide

A per-row merge policy, consulted at **every** three-way row decision including
convergent ones, on both merge paths, answering:

| answer | Dolt does |
|---|---|
| defer | classifies the row itself, today's behaviour |
| resolved | takes the supplied row |
| conflict | records a data conflict |

Defer is the zero value, so a merge with no policy is unchanged.

The policy is handed the base, left and right rows. Three rows are not enough:
it also needs the value descriptor and the node store, because **the rows alone
do not contain the documents.**

`doc` uses Dolt's adaptive encoding. A small document lives inline in the cell;
above a size threshold the cell holds an address and the bytes live out of
band. Reading the field directly therefore returns a document sometimes and a
pointer other times, and resolving the pointer requires the node store.
dumbodb's own reader already does this dance -- `getBSONStoredBytes`
(`internal/backends/dolt/bson_codec.go`) resolves the field through the store
and switches on what comes back -- and a policy has to do the same or it will
read small documents correctly and misread large ones. That is the worst shape
a bug can take here: correct in every small test, wrong in production.

The same two are needed on the way out. A merged document is built, not copied
from either side, so the policy has to construct a row -- which means the
descriptor to build against, and the store to spill into if the result is large
enough to go out of band.

Two cases defer unconditionally: keyless tables, and merges that also change
schema, where the shape of a returned row would be ambiguous.

This is generic. No document format and no dumbodb concept appears in Dolt.
The four modes live entirely on the dumbodb side, as one policy: the knob B
decision of section 3, supplied to Dolt's merge rather than run beside it. That
is where `mergeBSONDoc` ends up.

The Dolt change stands on its own: it is reviewable and testable there, with
the four modes written as test policies over ordinary SQL rows and over a
single adaptive blob column in dumbodb's shape, and it carries no dependency on
dumbodb.

## 5. The four mergeModes, precisely

Section 1 introduced the names. This is the formal statement.

The names decompose on two axes -- the unit and the trigger -- which is why
there are exactly four and no fifth:

| | trigger: `Touched` | trigger: `Divergent` |
|---|---|---|
| unit: `document` | `documentTouched` | `documentDivergent` |
| unit: `field` | **`fieldTouched`** *(default)* | `fieldDivergent` *(today)* |

For one document with common ancestor `base` and two versions `ours` and
`theirs`, let `Fo` be the set of fields `ours` changed relative to `base` and
`Ft` the same for `theirs`:

| name | conflict when | plain english |
|---|---|---|
| `documentTouched` | `Fo` and `Ft` both non-empty | both sides wrote this document |
| `fieldTouched` | `Fo` and `Ft` intersect | both sides wrote the same field |
| `fieldDivergent` | some field in `Fo` intersect `Ft` has different values | both sides wrote the same field, differently |
| `documentDivergent` | `Fo` and `Ft` both non-empty and `ours != theirs` | both sides wrote this document, to different results |

Formally the strictness order is `documentTouched` > `fieldTouched` >
`fieldDivergent` and `documentTouched` > `documentDivergent` >
`fieldDivergent`, with `fieldTouched` and `documentDivergent` incomparable
(section 1).

What each is for:

- **`documentTouched`** -- the document is a unit and any concurrent write
  needs a human. Strongest, noisiest.
- **`fieldTouched`** -- protects read-modify-write fields. The default,
  because the CAS pattern must work.
- **`fieldDivergent`** -- cheapest merges. Correct only when fields are
  independent and no value is derived from its own prior value.
- **`documentDivergent`** -- never synthesize a document no branch wrote, but
  let duplicate identical writes through. For idempotent ingestion and replay
  (retried delivery, two pipelines on one upstream event, a backfill run
  twice), and for cross-field invariants a validator cannot express, such as a
  state machine or a ledger entry.

## 6. The CAS race traced through every mergeMode

```
base:   { _id: 1, v: 7, status: "draft" }

session A: updateOne({_id: 1, v: 7}, {$set: {status: "review"}, $inc: {v: 1}})
  ours:   { _id: 1, v: 8, status: "review" }

session B: updateOne({_id: 1, v: 7}, {$set: {owner: "bob"}, $inc: {v: 1}})
  theirs: { _id: 1, v: 8, status: "draft", owner: "bob" }

Fo = { v, status }        Ft = { v, owner }        Fo intersect Ft = { v }
```

Both CAS conditions matched, because each session is pinned to its own fork and
cannot see the other. The merge is the only thing between this and a lost
update.

Per key, under today's differ:

- **`status`** -- base `"draft"`, ours `"review"`, theirs `"draft"`.
  `rightUnchanged` is true, so ours wins. Merges under every mode.
- **`owner`** -- absent in base and ours, present in theirs. Theirs wins.
  Merges under every mode.
- **`v`** -- base 7, ours 8, theirs 8. Both changed it. This is the only
  contested key, and it decides the document.

| mode | verdict on `v` | outcome |
|---|---|---|
| `documentTouched` | both sides wrote the document | CONFLICT |
| `fieldTouched` | both sides wrote `v` | **CONFLICT -- only one write lands. Required.** |
| `fieldDivergent` | `v` is 8 on both sides, convergent | merge -> `{_id: 1, v: 8, status: "review", owner: "bob"}`. Both writes landed, `v` records one increment, neither client was told. **The bug.** |
| `documentDivergent` | both wrote, `ours != theirs` | CONFLICT |

## 7. What the losing side sees

The modes decide *what is a conflict*. This section is what happens next,
and it differs by how the divergence arose -- not by server mode.

### 7.1 The CAS condition lives in the operation, not the document

This is the whole reason a merge cannot answer a CAS.

MongoDB reports a failed compare-and-swap as `{ ok: 1, n: 0, nModified: 0 }` --
a successful command reporting no match, no error:

```js
const doc = await db.collection('posts').findOne({ _id: postId });

const result = await db.collection('posts').updateOne(
  { _id: postId, version: doc.version },              // expecting version 1
  { $set: { title: "Published" }, $inc: { version: 1 } }
);

if (result.matchedCount === 0) {
  // someone else got there first; re-read and retry
}
```

The condition that makes this work is `version: doc.version` in the **filter**.
A three-way merge never sees it. By merge time both sides are just documents
holding `version: 2`, and the fact that one of them only reached 2 because it
believed it was starting from 1 has been discarded. **State-based
reconciliation cannot recover an operation's precondition.** That is not a
timing problem or a server-mode problem; it is what merging end states means.

Replaying the *operation* does recover it. Applied against a tip already at
`version: 2`, the filter matches nothing and the CAS fails for exactly the
reason MongoDB would fail it.

### 7.2 Two kinds of divergence, two answers

| divergence | reconciliation | what the loser sees |
|---|---|---|
| two branches, each with committed history | **merge** -- the states are both real history and there is no operation left to replay | a conflict via `dumboConflicts`, resolvable as today; a human has context and should choose |
| a session's uncommitted pending work | **rebase** -- replay the pending operations onto the tip | each operation re-evaluates on its own terms. A CAS whose filter no longer matches simply matches nothing, exactly as in section 7.1 |

The second row is the proposal to validate (section 7.3). It makes
`--session-isolation` *less* of a special case, not more: a session is a
long-running transaction, its pending operations are replayed onto current
state at commit, and each one succeeds or fails on its own merits just as it
would have in default mode. No bespoke error code, no bespoke conflict shape.

**A pre-commit acknowledgment in `--session-isolation` is not final** -- no
more than a write inside a MongoDB transaction is final before
`commitTransaction`. MongoDB's own answer for a CAS that loses inside a
transaction is to abort and retry the transaction, not to resolve anything.
Treating the pending ack as provisional is the existing model, not a new
concession.

### 7.3 Rebase-on-commit: to validate

Stated as a requirement to test, not a settled design:

- A session forks at `v: 1` and runs the CAS from section 7.1. Another session
  commits
  `v: 2` first. On `dumboCommit` the pending operation is replayed against the
  new tip, matches nothing, and the session is told its CAS did not apply --
  rather than being merged in, and rather than being handed a conflict to
  resolve.
- The winning side's write is intact and the counter reads the number of
  increments that actually applied.
- A session whose pending operations do *not* collide replays cleanly and
  commits with no user-visible difference from today.

Open questions this raises, which belong to the rebase work rather than to the
modes:

1. **The session must retain its operations, not only its resulting working
   set.** Today it holds a forked snapshot. Keeping a replayable operation log
   is the main implementation cost and the main reason this needs its own
   design.
2. **Not every operation is replay-safe.** `$inc` is; `$currentDate`, generated
   `ObjectId`s, and anything reading server state are not obviously so. Which
   operations can be replayed, and what happens to the rest, needs an answer.
3. **Reporting.** If a replayed operation matches nothing, the commit has to
   say which ones did not apply, so the client can retry those rather than
   guessing.

Tracked on workspace-sb3. Replay is preferred over evaluating write filters
against the branch tip at write time: tip evaluation gives earlier feedback but
abandons the isolation it exists to provide, whereas replay preserves
isolation and still reaches the right answer.

### 7.4 What the mergeModes deliver on their own

Independent of any of the above: the modes stop the loss being **silent**
on the merge path. Under `fieldTouched`, two CAS results that collide on `v` do
not quietly combine, on any merge path. That is worth having by itself, and it
is what this document commits to. How the losing side is then told -- conflict
for a branch merge, replay for a pending session -- is section 7.2.

## 8. Default and configuration

**The default is `fieldTouched`**, because the compare-and-swap of section 1
has to work without a collection opting into anything. `documentTouched` would
also honour it, but `fieldTouched` is the weakest mode that does, so it buys
the guarantee at the smallest cost in false conflicts. This changes existing
behaviour and needs a release note.

The mode applies to every merge -- `dumboMerge`, the implicit merge at
`dumboCommit`, and the replay commands `dumboRebase`, `dumboCherryPick` and
`dumboRevert`. None is exempt.

It is collection config, stored in the per-collection catalog document in
`__dumbo_catalog__` (`backends.ReservedCatalogName`) alongside `validator` and
`collation`, as a new field on `collMeta`
(`internal/backends/dolt/collection_catalog.go:41`) carried through
`collMetaToDoc` / `docToCollMeta`:

```
{ _id: "<collection>", uuid: ..., validator: ..., mergeMode: "fieldTouched", ... }
```

- `docToCollMeta` returns the zero value for an absent key, so old catalogs
  decode as unset. **Unset must resolve to the default.**
- Settable at `create`, changeable with `collMod`, matching `validator`.
- Reported by whichever dumbo-only surface lists collection config, never by
  `listCollections`, which mirrors MongoDB.
- The wire value is always the name, never a number.

## 9. Still to pin

**What counts as a field.** Today a top-level key, with subdocuments and arrays
atomic. Full paths would let `a.b` and `a.c` be distinct, which a `fieldTouched`
collection would want, but that is a real change to the differ and needs a
companion answer for arrays, where element-wise identity is not well defined.
Independent of the modes and decidable separately.

**Whether the modes govern adds and deletes.** The matrix assumes they do. The
consequence at the default is that two pipelines inserting an identical
document conflict, which should be a deliberate choice rather than a
side-effect.

**What "identical" means for `documentDivergent`.** Proposal: equality of the
canonical stored bytes, which is how documents are already compared, so field
order and encoding cannot make two equal documents look different.

**Which mode governs a merge of two branches that disagree about the mode.**
It is itself branch-versioned data. Destination wins, strictest wins, or the
divergence is itself a conflict? `metaConflicts` in `merge_validation.go` gives
this a home. Note "strictest wins" is undefined across `fieldTouched` and
`documentDivergent`, which are incomparable (section 5).

**Conflict legibility.** A `fieldTouched` conflict reached through `dumboMerge`
can have identical `ours` and `theirs`, which reads as a bug unless the
envelope says why -- likely a new `reason.code`, following
`docs/design/unique-collision-conflict-representation.md`.

## 10. Out of scope

- Test plan and implementation.
- A per-invocation mode override on `dumboMerge`. The collection config is the
  only source for now.
- **Per-field policy.** A collection-level setting forces the whole document to
  the strictest mode any single field needs: protect a counter with
  `fieldTouched` and two idempotent writers of an identical `lastSyncedAt`
  conflict too. A per-field override over a collection default is the natural
  end state, so the catalog field should be shaped to allow one later without a
  format change. Not day one.
- **Server-side optimistic locking tokens** (workspace-wdv). With
  `fieldTouched` as the default these become an ergonomics feature rather than
  a correctness requirement.
