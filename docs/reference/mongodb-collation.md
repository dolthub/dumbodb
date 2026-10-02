# MongoDB Collation

Probed against MongoDB 8.0.4 (recorded 2026-07-23), except where marked.

A collation is a rule set for comparing strings. `locale: "simple"` (or no
collation) means binary comparison.

## Scopes

| Scope | Exists? | Set where | Mutable? | What it affects |
|---|---|---|---|---|
| Database | No | -- | -- | Nothing. There is no database-level default. Collections in one database may each have a different collation, `dbStats` reports none, and new collections default to simple. |
| Collection | Yes | `createCollection({collation})` | No. `collMod` rejects `collation` as an unknown field. | The default for every operation on the collection that does not give its own collation, and the collation inherited by any index created without one, including `_id`. |
| View | Yes | `createCollection` / `createView({viewOn, collation})` | No | Same role as a collection default, for a view. |
| Index | Yes | `createIndex({collation})`, otherwise inherited from the collection default | Fixed at create | Uniqueness: a unique index enforces under its own collation. Eligibility: an index can serve a query only if the query's effective collation equals the index's. |
| Operation | Yes | `collation` option on find/aggregate/count/distinct/update/delete/findAndModify | Per call | Overrides the collection or view default for that operation and governs every string comparison in it. Can opt down to simple. |

Precedence is **operation > collection-or-view default > simple**. There is no
database rung. Indexes are not part of the precedence for matching. They impose
a constraint (uniqueness) and allow an optimization (eligibility), and both must
line up with whatever collation the query resolves to.

## Probe results

- Database level: `dbStats` has no collation. Two collections in one database
  held en/2 and en/3 at the same time.
- Collection collation is immutable: `collMod {collation: ...}` fails with
  `IDLUnknownField` ("BSON field 'collMod.collation' is an unknown field").
  Renaming a collection keeps its collation.
- Index inheritance: an index created with no collation in an en/2 collection
  reported en/2. An index created with explicit en/3 kept en/3.
- Operation default: in an en/2 collection, `find({u: "bob"})` with no
  operation collation matched "BOB". The same find with
  `collation: {locale: "simple"}` matched nothing.
- `listCollections` reports a view's fully resolved collation.

## The `_id` index

The `_id` index is not always simple:

- It inherits the collection default like any other index. `listIndexes`
  reports `_id_` with the collection collation (en/2 in the probe).
- `_id` uniqueness is enforced under that collation. In a strength-2
  collection, inserting `_id: "a"` and then `_id: "A"` fails with a
  duplicate-key error (11000). In a simple collection both succeed.
- `createIndex({_id: 1}, {collation: X})` with X different from the collection
  default fails with `BadValue` "The _id index must have the same collation as
  the collection." An explicit collation equal to the default, or none, is
  accepted.

So the `_id` collation is pinned to the collection default and cannot differ
from it.

## Resolution

Effective collation of an operation on a collection or view:

```
if op.collation is set:          use op.collation (normalized)
else if target has a default:    use target default
else:                            simple
```

Collation of an index, resolved once at `createIndex` and then stored:

```
if spec.collation is set:        use spec.collation (normalized)
else if collection has default:  use collection default
else:                            simple
```

Two collations are equal when their normalized specs match on locale, strength,
caseLevel, caseFirst, numericOrdering, alternate, maxVariable, normalization,
and backwards. The reported `version` (the ICU version) comes from the
server's ICU build. MongoDB 8.0 reports 57.1.

An index can serve a query only when the query's effective collation equals the
index's collation. Otherwise MongoDB falls back to a collection scan and applies
the collation during the scan. Results are the same either way.

"D" = a non-simple collection default; "X" = a non-simple operation collation
different from D; "-" = unset.

| op.collation | coll default | effective | eligible index collation |
|---|---|---|---|
| - | - | simple | simple (including `_id`) |
| - | D | D | D (including indexes that inherited D) |
| X | - | X | X |
| X | D | X | X only; a D index is not eligible |
| simple | D | simple | simple; a D index is not eligible |
| D | D | D | D |

A unique index enforces uniqueness under its own collation, whatever collation
an operation carries. For example, a strength-2 unique index on `email` rejects
"A@x.com" when "a@x.com" exists.

## ICU and Go libraries

These facts concern ICU and Go libraries, not MongoDB.

- MongoDB's collation is ICU. `golang.org/x/text/collate` cannot express
  `caseFirst`, `strength` 4 or 5, `maxVariable`, or `normalization: false`.
  go-mysql-server collations are a single-weight MySQL engine that cannot
  express contractions, expansions, or MongoDB's option set.
- ICU appends its version to its exported C symbols (for example `ucol_open_78`
  and `uregex_open_72`). Two ICU versions can therefore link into one process.
- Collation sort keys depend on the ICU version that produced them.
- When collation options have no effect (from the Unicode Collation Algorithm):
  - `maxVariable` matters only when `alternate` is `shifted`.
  - `caseFirst` has no effect at strength 1-2 unless `caseLevel` is true.
  - `backwards` reorders the secondary (accent) level, so it has no effect at
    strength 1.
  - Quaternary differences appear only at strength >= 4, mostly with
    `alternate: shifted`. The identical level appears only at strength 5.
  - `numericOrdering` and `normalization` change results only for inputs with
    digits or combining sequences.
