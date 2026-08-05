# Collation Resolution and Precedence

**Issues:** workspace-alp (epic: collation-unaware areas), workspace-alp.1
(collection default collation), workspace-alp.16 (collection-metadata
persistence)
**Date:** 2026-07-23
**Status:** Draft

## 1. Goal and scope

Define exactly how DumboDB decides *which* collation governs a given string
comparison, so that behavior matches MongoDB 8.0. The hard part is not the
comparison itself (a locale-aware collator is a separable concern -- an earlier
`internal/collation` implementation was reset, see 4.1); it is **resolution**:
across the scopes MongoDB actually has -- operation, collection/view default,
and index, but deliberately NOT database (2.1) -- which collation applies to a
given operation, which one an index carries, and when an index may serve a
query. Section 2 enumerates every scope and what it affects; this doc is not
index-specific.

This doc leads with the **parity test matrix** (section 4). The matrix is the
contract. MongoDB 8.0 is the reference platform; we do not hardcode expected
values, we run each case against both servers and diff. The resolution rules in
section 3 are our current model of that contract and will be corrected wherever
the harness shows MongoDB does something else.

Non-goals for this doc: the *fidelity of the comparator itself* to MongoDB's
ordering (strength/locale nuances -- workspace-alp.14), and performance of
collated reads (collation-keyed indexes -- workspace-alp.15). Those are tracked
separately and only referenced where they intersect resolution.

ICU here means International Components for Unicode, the C/C++ library that
implements the Unicode Collation Algorithm. MongoDB's collation is built on ICU;
the `version` field it reports (e.g. "57.1") is the ICU version it was compiled
against. DumboDB's comparator today is `golang.org/x/text/collate` (a separate
Go implementation), so it only approximates ICU. Note that the C ICU library is
already linked into the DumboDB binary through the SQL engine (Dolt pulls in
`github.com/dolthub/go-icu-regex`, which cgo-links `libicui18n/libicuuc/
libicudata`); it currently binds only ICU's regex API, not its collation API
(`ucol`). This opens a real option for the collator, discussed in 5.1. Which
comparator we use does not change the resolution rules below.

## 2. The collation model (scopes)

A collation is a rule set for comparing strings. In MongoDB it can be attached
at several scopes. All statements here were verified against MongoDB 8.0.4 (see
2.2); we do not assume, we probe.

### 2.1 The scopes, and what each affects

| Scope | Exists? | Set where | Mutable? | What it affects |
|-------|---------|-----------|----------|-----------------|
| **Database** | NO | -- | -- | Nothing. There is no DB-level default collation. Collections in one DB may each have a different collation; `dbStats` reports none. New collections default to simple regardless of DB. |
| **Collection** | yes | `createCollection({collation})` | NO (immutable; `collMod` rejects `collation` as an unknown field) | The default comparator for every operation on the collection that does not specify its own; and the collation inherited by any index created without its own (including `_id`). |
| **View** | yes, but EPHEMERAL | `createCollection/createView({viewOn, collation})` | NO | Same role as a collection default, but for a view. CAVEAT: DumboDB views work only within a running server -- the view definition (and its collation) lives in an in-memory map (`state.views`) and is NOT persisted, so it is lost on restart (same gap as workspace-alp.16). View collation is therefore gated on both view durability and the metadata catalog. |
| **Index** | yes | `createIndex({collation})`, else inherited from collection default | fixed at create | (a) uniqueness enforcement -- a unique index enforces under ITS collation; (b) index eligibility -- an index can serve a query only if the query's effective collation equals the index's. |
| **Operation** | yes | `collation` option on find/aggregate/count/distinct/update/delete/findAndModify | per-call | Overrides the collection/view default for that one operation; governs all string comparisons in it. Can opt *down* to simple. |

`locale: "simple"` (or absent) means binary comparison.

So the layering is: **operation > collection-or-view default > simple**. There is
no database rung. Indexes are not a precedence rung for *matching*; they are a
constraint (uniqueness) and an optimization (eligibility) that must line up with
whatever collation the query resolves to.

"View" above means a **standard (computed) view** -- `createView`/`create
{viewOn, pipeline}`, read-only, pipeline re-run per query. MongoDB's "on-demand
materialized view" is NOT a distinct object or scope: it is the pattern of an
aggregation ending in `$merge`/`$out` (both present in DumboDB) writing into a
normal collection. Such a collection carries a collection default collation like
any other and needs no special handling here; it is also durable (a real
collection), unlike the ephemeral computed-view metadata.

The `_id` index is a special case, corrected from an earlier wrong assumption:
it is NOT always simple. It inherits the collection default like any other
index, its uniqueness is enforced under that collation (so in a strength-2
collection `_id` "a" and "A" collide), and it may not be given a collation that
differs from the collection default (`createIndex({_id:1},{collation:...})` !=
default is rejected with BadValue). See 3.6.

### 2.2 Reference-verified facts (MongoDB 8.0.4)

- DB level: `dbStats` has no collation; two collections in one DB held en/2 and
  en/3 simultaneously -> collation is per-collection, not per-db.
- Collection immutable: `collMod {collation:...}` -> `IDLUnknownField` ("BSON
  field 'collMod.collation' is an unknown field").
- Index inheritance: an index created with no collation in an en/2 collection
  reported en/2; an explicit en/3 index kept en/3.
- Operation default: `find({u:"bob"})` with no op collation in an en/2
  collection matched "BOB"; the same find with `collation:{locale:"simple"}`
  matched nothing.
- View: supported in-session (create/query with pipeline applied, writes
  rejected, `listCollections type:"view"`, parity-tested Full) and
  `listCollections` reports the view's fully-resolved collation -- BUT DumboDB
  stores the view definition only in memory (`state.views`, no persistence, no
  hydration on open), so views do not survive a restart. Verified live 8.0.4 +
  DumboDB source (internal/backends/dolt/database.go:256).
- `_id`: `listIndexes` reported `_id_` as en/2; inserting `_id:"a"` then
  `_id:"A"` in an en/2 collection -> duplicate-key error; `createIndex` on `_id`
  with a non-default collation -> BadValue "The _id index must have the same
  collation as the collection".

### 2.3 Status in DumboDB today (all reset; to build)

- Database scope: nothing to do (does not exist).
- Collection + View default scope: NOT implemented; blocked on collection-
  metadata persistence (workspace-alp.16). This is the keystone.
- Index scope: prior WIP stored/echoed an explicit index collation and enforced
  uniqueness, but had no inheritance and a raw (non-normalized) identity check;
  reset. To rebuild on top of resolution.
- Operation scope: prior WIP wired matching+sort; reset. To rebuild once
  resolution consults the collection/view default.

## 3. Resolution and precedence

### 3.1 Effective collation of an operation

```
effective(op, target):        # target is a collection or a view
    if op.collation is set:        return normalize(op.collation)
    if target.defaultCollation set: return target.defaultCollation
    return SIMPLE
```

There is no database term -- resolution stops at the collection/view default.
`target.defaultCollation` is the collection's (or view's) collation. `normalize`
fills MongoDB's defaults and maps `{locale:"simple"}` to SIMPLE (no collation).
SIMPLE means binary comparison; `Comparator()` returns nil.

Note the override direction: an explicit `op.collation: {locale:"simple"}` on a
collection whose default is non-simple forces binary for that operation. So
"operation wins" includes the ability to opt *down* to simple.

### 3.2 Effective collation of an index (inheritance)

Resolved once, at `createIndex`, and then stored:

```
indexCollation(spec, coll):
    if spec.collation is set:     return normalize(spec.collation)
    if coll.defaultCollation set: return coll.defaultCollation   # inheritance
    return SIMPLE
```

Consequence: in a collection with a non-simple default, an index created with no
collation is NOT simple -- it inherits the collection default. This is what lets
collated queries be served by "ordinary" indexes in MongoDB. DumboDB currently
stores only the explicitly-supplied index collation, so implementing 3.2
requires the collection default to exist first (workspace-alp.16 / .1).

### 3.3 Collation identity and equality

Two collations are equal iff their normalized specs match on all semantic
fields: locale, strength, caseLevel, caseFirst, numericOrdering, alternate,
maxVariable, normalization, backwards.

`version` (the ICU version, e.g. "57.1") is reported in listIndexes output but
is derived from the server's ICU build; it is metadata, not part of identity for
resolution/eligibility. (Open question O5: confirm MongoDB does not gate index
eligibility on version.) DumboDB currently hardcodes `Version = "57.1"` in
`internal/collation` to match MongoDB 8.0's reported value; that is honest only
while we approximate 57.1 and becomes a decision point if we adopt a real ICU of
a different version (see 5.1).

### 3.4 Index eligibility for a query

An index I may serve operation op only if `effective(op) == I.collation` by 3.3.
On mismatch the planner cannot use I; MongoDB falls back to a collection scan and
applies the collation during the scan. Results are identical either way; only
the plan differs.

DumboDB today: `QueryParams.Collated` is set whenever `effective(op)` is
non-simple, which disables all byte-exact narrowing (index lookup, _id lookup,
scan prefilter, indexed count) and forces a scan + collator re-check. That is
always correct but never index-served. Matching MongoDB's "use an index whose
collation equals the query collation" is the optimization tracked in
workspace-alp.15 and needs collation-keyed index encoding; out of scope here
except that resolution must produce the identity 3.4 compares.

### 3.5 Uniqueness

A unique index enforces uniqueness under **its own** collation (3.2), regardless
of any operation's collation. Example: a unique index on `email` with strength 2
rejects inserting "A@x.com" when "a@x.com" exists, even for an insert that
carries no collation. Implemented today via the value-level scan path folding
through the index's collator.

### 3.6 The `_id` index (verified; earlier assumption was wrong)

`_id` is NOT special-cased to simple. Verified against MongoDB 8.0.4:

- The `_id` index **inherits the collection default collation** like any other
  index. `listIndexes` reports `_id_` with the collection collation (en/2 in the
  probe), not simple.
- `_id` **uniqueness is enforced under that collation**. In a strength-2
  collection, inserting `_id:"a"` then `_id:"A"` fails with duplicate-key (11000).
  In a simple collection they coexist.
- The `_id` index **may not be given a collation different from the collection
  default**: `createIndex({_id:1}, {collation: X})` with X != default is rejected
  with `BadValue` "The _id index must have the same collation as the collection."
  (An explicit collation equal to the default, or none at all, is fine.)

Net rule: `_id`'s collation is *pinned to* the collection default -- it always
equals it, and cannot diverge. This is the opposite of "always simple." Query
matching on `_id` therefore uses the effective collation like any field.
(section 4, dimension D pins each of these.)

### 3.7 Immutability

`createCollection` collation is set-once. `collMod` cannot change it -- MongoDB
does not even recognize a `collation` field on `collMod` (`IDLUnknownField`).
Renaming a collection preserves it. Views likewise: their collation is fixed at
creation.

### 3.8 What resolution feeds

Once `effective(op)` is known, the same comparator must be threaded into every
string comparison the operation performs: match (all operators), sort,
distinct-value dedup, and every aggregation stage/expression that compares
strings. The comprehensive list of sites is the workspace-alp children; this doc
governs *which* collation each site receives, not the per-site plumbing.

### 3.9 Resolution matrix

Effective collation and index-eligibility for the combinations that matter.
"D" = a non-simple collection default; "X" = a non-simple operation collation
different from D; "-" = unset/simple.

| op.collation | coll default | effective(op) | can use index with collation... |
|--------------|--------------|---------------|---------------------------------|
| -            | -            | SIMPLE        | SIMPLE (incl. _id)              |
| -            | D            | D             | D (incl. indexes that inherited D) |
| X            | -            | X             | X                               |
| X            | D            | X             | X only (a D index is NOT eligible) |
| simple       | D            | SIMPLE        | SIMPLE (e.g. _id); a D index is NOT eligible |
| D            | D            | D             | D                               |

## 4. Parity test matrix (first deliverable)

Framing: each row becomes a `harness.PairTest`. We assert nothing a priori --
the harness runs the case against MongoDB 8.0 and DumboDB and diffs. Support
levels: `DumboDBFull` where we expect (and require) a match, `DumboDBXFail`
where we know we diverge today and want a regression guard until fixed,
`DumboDBMongoOnly` where the feature is out of scope. As pieces land, XFail ->
Full.

The "Expected (verify)" column is our best current understanding of MongoDB, to
be replaced by whatever the oracle actually does. Where it is uncertain it is
marked (?).

### 4.1 Prior WIP (reset -- available only as reference)

An earlier exploratory implementation (5 dumbodb commits + parity tests) was
reset to origin/main on 2026-07-28 because it predated this design: same-key
index-by-collation, listIndexes echo/resolved-spec, collated-unique
enforcement, and operation-scope matching+sort across the commands. It is NOT
in the tree. It is recoverable/referenceable via the `wip-collation-prereset`
git tag in both repos (dumbodb and dumbodb-parity-testing). Treat it as a source
of ideas and a known-conformance-gap list (e.g. the raw-vs-normalized index
identity bug), NOT as validated behavior. Everything in the matrix below is
to-be-built.

### 4.2 New cases to add

#### DB. Database scope (prove it does not exist)

| ID | Setup | Operation | Expected (verified 8.0.4) | Support |
|----|-------|-----------|---------------------------|---------|
| DB1 | fresh db | dbStats | no `collation` field present | Full |
| DB2 | one db: createCollection c1{en,2}, c2{en,3} | listCollections | c1 and c2 report their own distinct collations (no db-level coupling) | XFail (needs collection default) |
| DB3 | db with default-collated collection; create a second collection with no collation | listCollections | second collection is simple (no inheritance from a "db default") | XFail |

#### A. Collection default + inheritance (blocked on alp.16/.1)

| ID | Setup | Operation | Expected (verify) | Support |
|----|-------|-----------|-------------------|---------|
| A1 | createCollection default D(en,2) | listCollections options | reports options.collation = resolved D | XFail |
| A2 | createCollection D; createIndex(no collation) | listIndexes | index collation == resolved D (inherited) | XFail |
| A3 | createCollection D; createIndex(explicit E) | listIndexes | index collation == E (override, not D) | XFail |
| A4 | createCollection D; insert "Alice" | find({u:"alice"}) no op collation | matches (uses D) | XFail |
| A5 | createCollection D; insert "Alice" | find({u:"alice"}) op collation simple | no match (override down to binary) | XFail |
| A6 | createCollection D; unique index(no collation) on u; insert "Alice" | insert "alice" | duplicate-key error (uniqueness under inherited D) | XFail |
| A7 | no default; createIndex(explicit E) on u | find no op collation | binary match only (E index not implicitly used) | Full(?) |

#### B. Operation override (mostly Full today; extend coverage)

| ID | Command | Case | Expected (verify) | Support |
|----|---------|------|-------------------|---------|
| B1 | find | op collation X overrides simple default | X semantics | Full |
| B2 | count/distinct/delete/update/findAndModify | each honors op collation | match | Full |
| B3 | aggregate | $match + $sort honor op collation | match | Full |
| B4 | any | op collation differs from a would-be default | op wins | XFail (needs default) |

#### C. Precedence resolution (blocked on default)

| ID | Setup | Operation | Expected (verify) | Support |
|----|-------|-----------|-------------------|---------|
| C1 | default D, index inherited D, data mixed-case | find no op collation | matches under D, index-served | XFail |
| C2 | default D, index D | find op collation X (!=D) | matches under X, NOT index-served (scan) | XFail |
| C3 | default D | find op collation == D | index-served | XFail |

#### D. The _id index (verified semantics -- pin all four)

| ID | Setup | Operation | Expected (verified 8.0.4) | Support |
|----|-------|-----------|---------------------------|---------|
| D1 | createCollection D(en,2) | listIndexes | `_id_` collation == resolved D (NOT simple) | XFail |
| D2 | createCollection D(en,2); insert _id "a" | insert _id "A" | duplicate-key error 11000 (uniqueness under D) | XFail |
| D3 | simple collection; insert _id "a" | insert _id "A" | both coexist (binary _id) | Full |
| D4 | createCollection D(en,2) | createIndex {_id:1} collation en,3 | BadValue "_id index must have the same collation as the collection" | XFail |
| D5 | createCollection D(en,2) | createIndex {_id:1} collation == D (or none) | accepted / idempotent | XFail |

#### E. Strength / option semantics (value-level; lean entirely on reference)

Each is find-equality and sort against both servers with the given collation.

| ID | Collation | Data | Probe | Expected (verify) | Support |
|----|-----------|------|-------|-------------------|---------|
| E1 | en,1 | cafe / cafe-with-acute-e | eq "cafe" | both match (accent-insensitive) | Full |
| E2 | en,2 | Alice / alice | eq "alice" | both match (case-insensitive) | Full |
| E3 | en,3 | Alice / alice | eq "alice" | only exact (case-sensitive) | Full(?) |
| E4 | en,2 caseLevel:true | Alice / alice | eq "alice" | only exact (caseLevel re-adds case) | XFail |
| E5 | en, numericOrdering:true | "2" / "10" | sort ascending | "2" before "10" | Full(?) |
| E6 | en, numericOrdering:false | "2" / "10" | sort ascending | "10" before "2" (lexical) | Full(?) |
| E7 | en,3 caseFirst:"upper" | apple/Apple | sort | uppercase-first ordering | XFail |
| E8 | fr, backwards:true | accented words | sort | accents compared right-to-left | XFail |
| E9 | tr (Turkish) | dotted/dotless i | eq | Turkish i-casing rules | XFail (locale fidelity) |

E4-E9 exercise the collation fields the current collator does not honor
(workspace-alp.14); expect XFail until the collator maps them.

#### F. Immutability and lifecycle

| ID | Case | Expected (verify) | Support |
|----|------|-------------------|---------|
| F1 | collMod attempt to change collection collation | IDLUnknownField (collMod has no `collation` field) | XFail |
| F2 | rename collection with default D | default preserved | XFail |
| F3 | createCollection with invalid locale | error; assert code | Full(?) |
| F4 | createCollection with unknown collation field | error; assert code | Full(?) |

#### G. Operator interactions

| ID | Case | Expected (verify) | Support |
|----|------|-------------------|---------|
| G1 | $regex under a non-simple collation | binary (regex ignores collation) | Full(?) |
| G2 | $in / range ($gt,$lt) under collation | collation applied | Full |
| G3 | min/max query bounds under collation | match | XFail (?) |
| G4 | aggregate $group string _id under collation | collation-bucketed | XFail |
| G5 | aggregate $lookup join under collation | collation-matched join | XFail |
| G6 | distinct returned-value dedup under collation | collation-collapsed values | XFail |
| G7 | update arrayFilters / $pull condition under collation | collation applied | XFail |

#### H. Simple normalization

| ID | Case | Expected (verify) | Support |
|----|------|-------------------|---------|
| H1 | index collation {locale:"simple"} | treated as no collation; coexists as binary | Full(?) |
| H2 | op collation {locale:"simple"} | binary | Full |
| H3 | collection default {locale:"simple"} | binary default (same as unset) | XFail (needs default) |

#### I. View collation (gated on view durability -- views are in-memory today)

Note: views currently do not persist across restart (in-memory `state.views`).
I0 pins that gap; I1-I4 are in-session behavior and additionally need the
metadata catalog for their collation to be durable.

| ID | Setup | Operation | Expected (verify) | Support |
|----|-------|-----------|-------------------|---------|
| I0 | createView; restart server | listCollections | view still present (durability) | XFail (in-memory today) |
| I1 | createCollection view {viewOn, collation D} | listCollections | view reports resolved D in options.collation | XFail |
| I2 | view with collation D over a simple source; source has "Alice" | find on the view {u:"alice"} no op collation | matches (view default D applies) | XFail |
| I3 | view with collation D | find on the view with op collation simple | binary (op overrides view default) | XFail |
| I4 | view with collation D | aggregate on the view, $match string | uses D | XFail |

## 5. Open questions

- O1: Where does the collection default live? (alp.16) Recommended: a persisted
  catalog map keyed by collection name, also home to validators/views. Decide
  catalog shape and whether it is per-branch (Dolt) versioned data or side
  metadata.
- O2: Index inheritance timing -- resolve+store at createIndex (snapshot), or
  store "inherited" and resolve at read? MongoDB snapshots (an index keeps its
  collation even though collection collation is immutable anyway). Prefer
  snapshot at createIndex.
- O3: Do we implement collation-keyed indexes for collated reads (alp.15), or
  keep the correct-but-scan behavior and document the perf gap?
- O4: RESOLVED (2026-07-28, verified vs 8.0.4): `_id` inherits and is pinned to
  the collection default collation; uniqueness enforced under it; may not differ
  from the collection default. No database-level collation exists. See 2.2, 3.6.
- O5: Is `version` part of index-eligibility identity? Assume no; verify.
- O6: Collated collection + `_id` string values: uniqueness vs query-match
  behavior (D3/D4) -- confirm.
- O7: Which comparator engine, and do we treat MongoDB's ICU *version* as a
  parity goal? See 5.1.

### 5.1 Collator engine (ICU) options

Resolution decides *which* collation applies; the engine decides how two strings
actually compare under it. Three options, with very different fidelity and cost:

| Option | Fidelity to MongoDB 8.0 | Cost / notes |
|--------|-------------------------|--------------|
| `x/text/collate` (today) | Approximate. Common cases (en strength 1/2/3, basic accents, numericOrdering) match; caseLevel/caseFirst/alternate/backwards and some locales diverge. `version` is faked as "57.1". | Pure Go, portable, no cgo. |
| cgo bind ICU `ucol` (host ICU) | High for folding/equality; ordering can still drift from Mongo where CLDR data changed between the host ICU and 57.1. | ICU is already linked (regex); add a `ucol` cgo binding. Build already needs system ICU. Forces the `version`-honesty choice below. |
| cgo `ucol` pinned to ICU 57.1 | Exact match to MongoDB 8.0, including `version`. | Must vendor/build ICU 57.1 specifically; heaviest; ties us to one Mongo release's ICU. |

The crux is the `version` field. MongoDB 8.0 always reports "57.1" and orders per
ICU 57.1. The host ICU (via go-icu-regex) is whatever the build image ships
(likely 70+). So with the middle option we must choose:

- keep emitting "57.1" -- the listIndexes `version` matches Mongo but
  misrepresents the engine actually comparing, or
- emit the real linked ICU version -- honest, but diverges from Mongo's "57.1"
  and fails the `version` field of the CollationResolvedSpec parity case unless
  the host ICU is 57.1.

Decision needed (O7): is byte-exact ordering + `version` parity with MongoDB 8.0
a goal, or only "correct enough" folding/matching? That answer picks the row.
This is the substance of workspace-alp.14 and does not block the resolution work
(phases 1-3); the resolution rules and the parity matrix are engine-independent.

## 6. Phasing

1. **Test matrix (this doc, section 4).** Land the parity cases first, as
   XFail where we diverge. This captures the reference contract and turns every
   subsequent change into an XFail->Full flip. THIS IS THE IMMEDIATE WORK.
2. **Collection-metadata persistence** (alp.16): catalog that survives restart;
   store the collection default collation (and, opportunistically, validators/
   views).
3. **Resolution wiring**: `effective(op)` consulting the collection default;
   index inheritance at createIndex; `_id` simple enforcement; collMod
   immutability.
4. **Collator fidelity** (alp.14): pick the engine per 5.1/O7, then map the
   remaining collation fields onto the comparator, flipping E4-E9.
5. **Optional: collation-keyed indexes** (alp.15) so collated reads are
   index-served.
</content>
