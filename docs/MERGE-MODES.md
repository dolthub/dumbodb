# Merge Modes

DumboDB keeps every collection in a branchable history, the way Git does for
directories. Any two branches -- long-lived ones you create and merge with
[`dumboMerge`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbomerge), or
the brief internal ones DumboDB uses to serialize concurrent writes -- are
recombined by **merging**.

How two documents are merged has implications for your application. DumboDB
supports four different document merging algorithms, all covered in detail by
this documentation.

## How a merge sees your data

A merge works one document at a time, and it identifies documents by `_id`, the
primary key of every collection. It lines the two branches up by `_id` and takes
each one in turn:

- an `_id` altered on one branch takes that branch's edits verbatim;
- an `_id` neither branch changed is left untouched;
- an `_id` altered on both branches may or may not result in a conflict. What is
  considered a conflict is determined by the merge mode.

Every merge mode handles the first two the same way. They differ only in that
third case: when both branches wrote the same document, how much agreement is
required before the merge accepts the result instead of raising a conflict. That
single choice is what a collection's **merge mode** sets.

## The Four Modes

A mode's name is two choices joined together. The first half is the **unit** the
merge compares -- the whole `document`, or a single `field`. The second half is
the **trigger** -- whether a side merely `Touched` it (wrote it at all) or the
two sides are `Divergent` (wrote it to different values). The four combinations
are the four modes:

|  | `Touched` -- a side wrote it at all | `Divergent` -- the sides wrote it differently |
|---|---|---|
| **`document`** -- the whole document | `documentTouched` | `documentDivergent` |
| **`field`** -- a single field | `fieldTouched` | `fieldDivergent` |

Read a name as unit then trigger: `fieldTouched` conflicts when both branches
wrote the same field at all, while `documentDivergent` conflicts only when both
branches wrote the document and the stored results differ. The precise rule for
each, and how they compare, is [below](#the-four-modes-in-detail).

## Setting a collection's merge mode

The mode is a collection option, set at creation or changed later. It takes one
of four string values (below); an unknown value is rejected.

Create a collection with a mode:

```js
db.createCollection("posts", { mergeMode: "fieldTouched" })
```

Change an existing collection's mode:

```js
db.runCommand({ collMod: "posts", mergeMode: "documentTouched" })
```

Inspect the mode currently in force:

```js
db.getCollectionInfos({ name: "posts" })
// [ { name: "posts", type: "collection", options: { mergeMode: "fieldTouched" }, ... } ]
```

A collection that declares nothing gets **`fieldTouched`**, the default. You
only need to set a mode to move away from it.

## The four modes in detail

The exact rule each mode applies:

| mode | conflicts when |
|---|---|
| `documentTouched` | both sides wrote anything in the same document |
| `fieldTouched` (default) | both sides wrote the same field, even to the same value |
| `fieldDivergent` | both sides wrote the same field and the values differ |
| `documentDivergent` | both sides wrote the same document and the results differ |

`fieldDivergent` is the classic Dolt behavior: agreeing edits coalesce, so two
branches that set a field to the same value merge instead of conflicting. It is
kept for workloads that genuinely want convergent writes to merge.

Worked across the cases that separate the modes (`merge` = the merge proceeds,
`CONFLICT` = it does not):

| scenario | `documentTouched` | `fieldTouched` | `fieldDivergent` | `documentDivergent` |
|---|---|---|---|---|
| only one side changed the document | merge | merge | merge | merge |
| different fields, same document | CONFLICT | merge | merge | CONFLICT |
| same field, same value (convergent edit) | CONFLICT | CONFLICT | merge | merge |
| same field, same value, plus a disjoint edit | CONFLICT | CONFLICT | merge | CONFLICT |
| same field, different values | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| modify vs delete | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| add/add same `_id`, identical content | CONFLICT | CONFLICT | merge | merge |
| add/add same `_id`, different content | CONFLICT | CONFLICT | CONFLICT | CONFLICT |
| both sides deleted | CONFLICT | CONFLICT | merge | merge |

The third row is why merge modes exist: two writers that set a field to the same
value must not both be silently accepted.

## They are a lattice, not a ranking

It is tempting to read the four as strictest-to-loosest and pick a point on the
line. There is no such line. `documentTouched` catches the most and
`fieldDivergent` the least, but the two middle modes are **incomparable** --
neither catches everything the other does:

```
                documentTouched              strictest: any concurrent write
                 /           \
        fieldTouched     documentDivergent   incomparable with each other
                 \           /
                fieldDivergent               loosest: only differing values
```

- Same field, same value: `fieldTouched` conflicts (both touched the field) but
  `documentDivergent` merges (the documents are identical).
- Disjoint fields: `documentDivergent` conflicts (the documents differ) but
  `fieldTouched` merges (no field in common).

So "stricter" is only meaningful along an edge of the lattice, not between
`fieldTouched` and `documentDivergent`. Choose by the guarantee you want, not by
a position on a scale.

## Why the default is what it is: compare-and-swap

Everything above describes how the modes behave. This is why `fieldTouched` is
the default rather than Dolt's looser original.

The usual way to do optimistic locking in MongoDB is a compare-and-swap on a
version field:

```js
const doc = await db.posts.findOne({ _id: postId });

const result = await db.posts.updateOne(
  { _id: postId, version: doc.version },              // the CAS condition
  { $set: { title: "Published" }, $inc: { version: 1 } }
);

if (result.matchedCount === 0) {
  // someone else moved it first; re-read and retry
}
```

Under a naive three-way merge this pattern is unsafe. Two clients that both read
`version: 1` can both be told `matchedCount: 1`, because by merge time both
sides simply hold `version: 2` and a merge that accepts agreeing edits reads
that as agreement. One of the two updates is silently discarded and the counter
reads 2 after two increments. This is the "same field, same value" row of the
table above.

`fieldTouched` closes that gap: two writers touching the same field is a
conflict, so at most one of the two increments can land. That is the guarantee
the default buys, and the reason it is the default.

## Choosing a mode

- **`fieldTouched` (default).** Compare-and-swap on a version field is safe, and
  writers editing genuinely different fields of the same document still merge.
  Start here.
- **`documentTouched`.** Any two concurrent writers of the same document
  conflict, even on disjoint fields. Use it when a document is a single
  invariant that no two writers may touch at once.
- **`documentDivergent`.** Concurrent writers merge as long as the resulting
  document is identical; only a real difference conflicts. Use it when you care
  about the end state, not about who touched what.
- **`fieldDivergent`.** Agreeing edits to the same field coalesce. This is the
  historical Dolt behavior and the one mode where compare-and-swap is unsafe;
  choose it only when convergent writes should merge.

## What a writer sees when a merge refuses a change

A merge mode decides *what is a conflict*. What happens next depends on how the
two sides diverged, and it is two different experiences.

**Concurrent writes to one branch (the CAS case).** These are reconciled
per-write: each write forks from the tip it read, applies, and publishes with a
compare-and-swap on the branch. A writer whose branch moved underneath it
replays its operation against the new tip. For a compare-and-swap that means the
filter `{ _id, version: old }` no longer matches, so the write reports
`{ ok: 1, n: 0, nModified: 0 }` -- `matchedCount: 0`, exactly as MongoDB would
report a lost CAS. The client re-reads and retries; nothing is discarded and no
error is raised.

**Explicit branch merges (`dumboMerge`).** Two long-lived branches are merged on
demand, so there is no operation to replay -- only two end states. Here the mode
decides directly: an offending document becomes a merge conflict surfaced by
`dumboMerge`, to be resolved like any other conflict. A `fieldTouched` conflict
can have identical `ours` and `theirs` values; that is expected, not a bug -- it
is the mode reporting that both sides wrote the field.

