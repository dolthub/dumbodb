# Merge Strictness Levels

**Issues:** workspace-mxv; motivated by workspace-dn0; optimistic locking
(workspace-wdv) is deferred behind this
**Date:** 2026-08-26
**Status:** Design. Behaviour specification only -- testing and implementation
are out of scope until the behaviour below is agreed.

## 1. Goal

A collection declares what makes two branches' changes to the same document a
conflict. Four levels, set per collection in `__dumbo_catalog__`.

### The four levels

Each level is named for **what triggers a conflict**. `Touched` means a side
wrote it at all; `Divergent` means the two sides wrote it differently. The unit
is either the whole `document` or an individual `field`.

| level | conflicts when |
|---|---|
| `documentTouched` | both sides wrote anything in the same document |
| `fieldTouched` | both sides wrote **the same field**, even to the same value |
| `documentDivergent` | both sides wrote the same document *and the results differ* |
| `fieldDivergent` | both sides wrote **the same field** *and the values differ* |

`fieldTouched` is the new default; `fieldDivergent` is today's only behaviour.

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

The trap worth naming, because it is easy to reason past: making a document
divergent does require touching *a* field, but `fieldTouched` asks whether both
sides touched **the same** field. Each side touching some field of its own is a
weaker condition than the two sides colliding on one field, so
`documentDivergent` does not imply `fieldTouched`.

Worked against one scenario -- branch A sets `status`, branch B sets `owner`,
and both `$inc` the same `v` from 7 to 8:

| level | verdict |
|---|---|
| `documentTouched` | conflict; both wrote the document |
| `fieldTouched` | conflict; both wrote `v` |
| `documentDivergent` | conflict; both wrote the document and the results differ |
| `fieldDivergent` | **merges**; `v` is 8 either way, `status` and `owner` are disjoint |

That last row is the bug. Section 4 gives the precise predicates, section 5 the
full scenario matrix, section 6 this example traced key by key.

### Where this stands today

There is exactly one behaviour, `fieldDivergent`, and it is not configurable.
**It silently loses updates on every merge path**, in every server mode:
explicit `dumboMerge` of two branches, `dumboRebase`, `dumboCherryPick`,
`dumboRevert`, and the implicit commit-time merge under `--session-isolation`.
Section 2.2 is the proof and section 3 names the exact line responsible. The
default becomes `fieldTouched` (section 9).

**There is a second, independent defect that these levels do not fix.**
Concurrent non-transactional writes lose acknowledged updates in every mode,
with no merge involved, because the read-modify-write holds no lock
(section 2.1, workspace-q2c, P0). Both must be fixed before a
compare-and-swap can be relied on. Do not read this document as claiming the
levels alone make CAS work.

Scope note worth having up front: this feature stops the loss being
**silent**. What the losing side then sees is section 7.

## 2. The problem, exactly

The standard MongoDB optimistic-locking pattern is a compare-and-swap on a
sequence field:

```js
db.docs.updateOne(
  { _id: 1, v: 7 },                                  // CAS condition
  { $set: { status: "review" }, $inc: { v: 1 } }
)
// n: 1 -> we won.  n: 0 -> someone else moved it; re-read and retry.
```

It fails in two different ways, and they need separating.

### 2.1 Concurrent writers, no branching: every mode loses writes

An earlier draft of this section claimed default and `--auto-commit` were
correct here. That was measured with the two updates issued **sequentially**,
which is not a race and proves nothing. Under a genuine race -- both writers
released at the same instant, both having read `v: 1` -- every mode loses
writes:

| mode | filter | rounds | consistent | **lost** |
|---|---|---|---|---|
| default | CAS on `v` | 60 | 52 | **8** |
| default | plain update, no CAS | 60 | 48 | **12** |
| `--auto-commit` | CAS on `v` | 60 | 51 | **9** |
| `--auto-commit` | plain update, no CAS | 60 | 50 | **10** |

"Lost" means the server acknowledged a write it did not keep. Every failing
round has the identical shape:

```
ackedA=1  ackedB=1  ->  a="s"  b="B"  v=2      (expected a="A" b="B" v=3)
```

Both writers were told `n: 1`. One writer's field is simply absent and `v`
counts one increment instead of two. It happens **with or without a CAS
filter**, so this is not a CAS problem: it is a lost write.

**This is a different defect from section 2.2, with a different cause and a
different fix.** The survivor is never the field-merged
`{a: "A", b: "B", v: 2}` -- in 39 failing rounds across four configurations it
was never once that shape. It is always one writer's document wholesale, which
is last-writer-wins clobbering, not a convergent merge.

The cause is that a non-transactional write holds no lock across its
read-modify-write (`internal/backends/dolt/doc_locks.go:172`):

```go
owner, inTxn := ownerForTxn(ctx, false)
if !inTxn {
    return mgr.WaitForRelease(ctx, collection, ids)   // waits, acquires nothing
}
```

A plain `updateOne` is non-transactional, so it waits for any *transaction*
holding the document and then proceeds **without holding anything itself**. Two
concurrent plain updates both read the document, both build a full replacement
from the same base, and the second overwrites the first.

**Merge strictness does not fix this and must not be described as if it does.**
No merge runs on this path. Tracked as workspace-q2c (P0); the levels in this
document address section 2.2 only. A CAS is trustworthy only once both land.

### 2.2 Any merge of two divergent lines

This is the general case, and it is not mode-specific. Measured in **default
mode** with two explicit branches and an explicit `doltMerge` -- no session
isolation involved at all. Baseline `{_id: 1, v: 1, a: "s", b: "s"}` committed,
branch `feature` created, then each branch runs the same CAS on `v == 1`:

| scenario | branch CAS matched | `doltMerge` | final |
|---|---|---|---|
| each branch sets a different field and `$inc`s `v` | main 1 / feature 1 | **`ok: 1`, no conflict** | `{a: "A", b: "B", v: 2}` |
| each branch only `$inc`s `v` | main 1 / feature 1 | **`ok: 1`, no conflict** | `{v: 2}` |

Two CAS-guarded increments from `v: 1` and the counter reads 2. The merge
reported success. Nobody was told anything.

**So the lost update is a property of the merge, not of
`--session-isolation`.** It reaches every merge path in every mode. The only
thing `--session-isolation` adds is that ordinary concurrent clients get there
without creating a branch or asking for a merge, which is why 2.1 is the more
alarming presentation and 2.2 is the more general problem.

### 2.3 Why convergence is the wrong test

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

These get conflated. They must not be.

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

**The four levels vary knob B. The CAS fix is knob B alone.** `v` is a
top-level scalar, so knob A already isolates it correctly; nothing about key
enumeration needs to change to fix the CAS. But knob B does have to change:
`fieldTouched` means deleting the `case sameMod` escape, so **this is a change
to the differ**, not a no-op. Knob A is a separate question, still open
(section 11.2).

## 4. The four levels, precisely

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

## 5. Scenario matrix

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

Row 3 is the bug and the whole reason for this document. Row 2 separates
`fieldTouched` from `documentDivergent`; row 4 separates `fieldDivergent` from
`documentDivergent`.

Two things sit on top of the level and are unchanged by it:

- **Collection validators still apply.** A merge that is clean under the level
  can still be refused because the merged document fails the validator. This
  already works (`internal/backends/dolt/merge_validation.go`) and must keep
  working at every level.
- **Unique index collisions** are not document-identity conflicts; see
  `docs/design/unique-collision-conflict-representation.md`.

## 6. The CAS race traced through every level

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
  `rightUnchanged` is true, so ours wins. Merges at every level.
- **`owner`** -- absent in base and ours, present in theirs. Theirs wins.
  Merges at every level.
- **`v`** -- base 7, ours 8, theirs 8. Both changed it. This is the only
  contested key, and it decides the document.

| level | verdict on `v` | outcome |
|---|---|---|
| `documentTouched` | both sides wrote the document | CONFLICT |
| `fieldTouched` | both sides wrote `v` | **CONFLICT -- only one write lands. Required.** |
| `fieldDivergent` | `v` is 8 on both sides, convergent | merge -> `{_id: 1, v: 8, status: "review", owner: "bob"}`. Both writes landed, `v` records one increment, neither client was told. **The bug.** |
| `documentDivergent` | both wrote, `ours != theirs` | CONFLICT |

## 7. What the losing side sees

The levels decide *what is a conflict*. This section is what happens next, and
it differs by how the divergence arose -- not by server mode.

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
timing problem or a mode problem; it is what merging end states means.

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

It also settles a question an earlier draft got wrong. That draft argued an
`n: 1` handed to a client before commit "cannot be retracted", and concluded
MongoDB CAS semantics were unreachable. **A pre-commit acknowledgment in
`--session-isolation` was never final** -- no more than a write inside a
MongoDB transaction is final before `commitTransaction`. MongoDB's own answer
for a CAS that loses inside a transaction is to abort and retry the
transaction, not to resolve anything. Treating the pending ack as provisional
is the existing model, not a new concession.

### 7.3 Rebase-on-commit: to validate

Stated as a requirement to test, not a settled design:

- A session forks at `v: 1` and runs the CAS from 7.1. Another session commits
  `v: 2` first. On `dumboCommit` the pending operation is replayed against the
  new tip, matches nothing, and the session is told its CAS did not apply --
  rather than being merged in, and rather than being handed a conflict to
  resolve.
- The winning side's write is intact and the counter reads the number of
  increments that actually applied.
- A session whose pending operations do *not* collide replays cleanly and
  commits with no user-visible difference from today.

Open questions this raises, which belong to the rebase work rather than to the
levels:

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

Tracked on workspace-sb3, which previously proposed evaluating write filters
against the branch tip at write time. Rebase-on-commit is the better shape:
tip evaluation gives earlier feedback but abandons the isolation the mode
exists to provide, whereas replay preserves isolation and still reaches the
right answer.

### 7.4 What the levels deliver on their own

Independent of any of the above: the levels stop the loss being **silent** on
the merge path. Under `fieldTouched`, two CAS results that collide on `v` do
not quietly combine, on any merge path. That is worth having by itself, and it
is what this document commits to. How the losing side is then told -- conflict
for a branch merge, replay for a pending session -- is 7.2.

What the levels explicitly do **not** deliver is a trustworthy CAS on their
own. Section 2.1's lost write happens with no merge involved and is untouched
by any level; it needs the locking fix in workspace-q2c. Both are required
before the pattern in 7.1 can be relied on, and this document should not be
read as claiming otherwise.

## 8. Scope: the level governs every merge

The level applies to all merges, explicit and implicit. No merge path is exempt:

- `dumboMerge`.
- The **implicit merge at `dumboCommit` under `--session-isolation`**. Not
  optional: this is the path section 2.1 measures, so a level that skipped it
  would not fix the bug.
- `dumboRebase`, `dumboCherryPick`, `dumboRevert`, which replay changes onto a
  new base and make the same three-way comparison.

A per-invocation override on `dumboMerge` is a plausible later addition and is
deliberately not in this design. The collection config is the only source of
the level for now.

## 9. Default: `fieldTouched`

**The default is `fieldTouched`.** The standard MongoDB
CAS-on-a-sequence-field pattern must work out of the box. It is what every
Mongo developer writes and what the ecosystem's documentation teaches, and a
database that silently loses those updates is not usable for the pattern
regardless of what our own docs say. A silent lost update reachable without
opting into any version-control concept is a correctness bug in the default,
not a documentation gap. Correctness by default beats a cheaper merge by
default.

`fieldDivergent` stays available per collection for workloads that genuinely
want field-level composition and have no derived fields.

This changes existing behaviour, so it needs a release note and a documented
way to opt a collection back to `fieldDivergent`.

## 10. Configuration

The level is collection config, stored in the existing per-collection catalog
document in `__dumbo_catalog__` (`backends.ReservedCatalogName`), alongside
`validator`, `collation`, and the time-series fields. Concretely a new field on
`collMeta` (`internal/backends/dolt/collection_catalog.go:41`) carried through
`collMetaToDoc` / `docToCollMeta`:

```
{ _id: "<collection>", uuid: ..., validator: ..., mergeStrictness: "fieldTouched", ... }
```

Notes that follow from how that catalog already works:

- `docToCollMeta` returns the zero value for an absent key, so existing
  catalogs decode as unset. **Unset must resolve to the default**, which keeps
  old databases readable and gives `fieldTouched` to collections created before
  the field existed.
- Settable at `create` and changeable with `collMod`, matching `validator`.
- Reported by whichever dumbo-only surface lists collection config. It must
  not appear in `listCollections`, which mirrors MongoDB.
- The wire value is the name, never a number. The level numbers used in
  discussion do not appear in the API.

## 11. Behaviour still to pin

### 11.1 Both sides deleted the same document -- DECIDED 2026-08-28

**The `Touched` levels include deletion.** If `documentTouched` conflicts
because both sides wrote the document, that covers both sides deleting it, and
the same holds for `fieldTouched`. No exception for agreeing deletes.

The field reading follows without special-casing: deleting a document changes
every field that existed, so `Fo` and `Ft` are both the full field set and they
intersect. `fieldDivergent` and `documentDivergent` still merge, because the
two sides agree on the result -- absence.

Note the shape this gives: delete/delete lands in the matrix exactly where
"same field, same value" does, conflicting at both `Touched` levels and merging
at both `Divergent` ones. Agreement is agreement whether the agreed value is a
value or an absence, and it is the `Touched` levels that refuse to treat
agreement as permission.

Verified against a Dolt row merge policy, where delete/delete is one of the
convergent branches that merges today without consulting anything (see
`/workspace/pluggable-row-merge-policy.md`, phase 1).

### 11.2 Comparison granularity (knob A)

The levels are specified over a set of changed fields, so "field" needs a
definition. Today it is a top-level key, with subdocuments and arrays atomic
(section 3).

A consequence to be aware of either way: two sides editing *different
subfields of the same subdocument* already conflict under today's
`fieldDivergent`, because `a` is one key and the two resulting values of `a`
differ. For nested data, `fieldDivergent` is already as strict as
`fieldTouched`.

Options:

- **Top-level keys.** Today's behaviour. No change to the differ's
  enumeration. The CAS fix does not need more than this.
- **Full paths**, so `a.b` and `a.c` are distinct fields and a `fieldTouched`
  collection merges concurrent edits to different subfields. A real change to
  the differ, and it needs a companion decision on arrays, where element-wise
  identity is not well defined.

This is independent of the level semantics and can be decided separately.

### 11.3 Whether the level governs adds and deletes

The matrix assumes it governs adds, deletes, and field edits alike. Note the
consequence at the default: two pipelines inserting the identical document
conflict under `fieldTouched`. That is a real cost of the default and should be
a deliberate choice.

### 11.4 What "identical" means for `documentDivergent`

Proposal: equality of the canonical stored bytes, which is already how
documents are compared elsewhere, so field order and encoding cannot make two
equal documents look different.

### 11.5 Level divergence across branches

The level is itself branch-versioned data. If one branch ran `collMod` to
change it and the other did not, which level governs *their* merge: destination
branch wins, strictest wins, or the divergence is itself a conflict? The
`metaConflicts` machinery in `merge_validation.go` gives this a home. Note that
"strictest wins" is not fully defined, since `fieldTouched` and
`documentDivergent` are incomparable (section 4).

### 11.6 Conflict legibility for the explicit-merge path

A `fieldTouched` conflict reached through `dumboMerge` can have identical
`ours` and `theirs`, which reads as a bug unless the envelope says why. Likely
a new `reason.code`, following
`docs/design/unique-collision-conflict-representation.md`.

## 12. Out of scope for now

- Test plan and implementation.
- A `dumboMerge` per-invocation override (section 8).
- **Per-field policy.** A collection-level setting forces the whole document to
  the strictest level any single field needs: set a collection to
  `fieldTouched` to protect its counter and two idempotent writers of an
  identical `lastSyncedAt` now conflict too. A per-field override with a
  collection-level default is the natural end state, so the catalog field
  should be shaped so one can be added later without a format change. Not day
  one.
- **Server-side optimistic locking tokens** (workspace-wdv, PR #66). With
  `fieldTouched` as the default, the application-managed CAS of section 2 stops
  losing writes silently across a merge, so a server-maintained token becomes
  an ergonomics feature rather than a correctness requirement. It does not make
  the CAS report failure the MongoDB way; that is workspace-sb3 (section 7.3).

## 13. Decisions

- **2026-08-26: four levels, named for their conflict trigger:**
  `documentTouched`, `fieldTouched`, `fieldDivergent`, `documentDivergent`.
  Configured per collection in `__dumbo_catalog__`.
- **2026-08-26: the default is `fieldTouched`.** Rationale in section 9.
  Today's behaviour, `fieldDivergent`, remains available per collection.
- **2026-08-26: the level governs every merge**, including the implicit
  `dumboCommit` merge under `--session-isolation` and the replay commands. A
  per-invocation `dumboMerge` override may come later.
- **2026-08-26: a strictness conflict from an implicit merge is not
  resolvable.** The loser computed from a base that no longer exists, so there
  is nothing to adjudicate and resolving `"ours"` would overwrite the winner.
  Resolution via `dumboConflicts` remains for explicit `dumboMerge` branch
  integration. A session's *uncommitted* work is a different case and should
  be rebased rather than merged (section 7.2), which is to be validated.
- **2026-08-27: the lost update is a property of the merge, not of
  `--session-isolation`.** Measured in default mode with two explicit branches
  and an explicit `doltMerge`: two CAS-guarded increments from `v: 1` merge
  cleanly to `v: 2` with `ok: 1` and no conflict (section 2.2). Every merge path
  in every mode is affected. `--session-isolation` is only the path that reaches
  it without any branch work.
- **2026-08-27: a CAS precondition cannot survive a state-based merge, so a
  session's uncommitted work should be rebased, not merged.** The condition
  lives in the operation's filter; merging end states discards it. Replaying the
  operation against the tip recovers it and matches MongoDB exactly. This also
  retires an earlier claim that a pre-commit `n: 1` "cannot be retracted" --
  such an ack was never final, no more than a write inside a MongoDB
  transaction is final before commit. To be validated; see section 7.3 and
  workspace-sb3.
- **2026-08-28: the `Touched` levels include deletion.** Both sides deleting a
  document conflicts under `documentTouched` and `fieldTouched`, and merges
  under `fieldDivergent` and `documentDivergent`. Section 11.1 has the
  reasoning; the section 5 matrix row is updated to match. This closes the last
  open question in section 11 about scenario outcomes.
- **2026-08-27: merge strictness delivers the end of *silent* loss, and that is
  what this document commits to.** How the losing side is told is section 7.2:
  a conflict for a branch merge, a replay for a pending session.
