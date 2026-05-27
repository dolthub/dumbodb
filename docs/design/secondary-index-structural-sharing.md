# Secondary Indexes with Structural Sharing

**Issue:** workspace-6r7
**Date:** 2026-05-20
**Status:** Draft

---

## 1. Problem

DumboDB persists secondary indexes as dolt `prolly.Map`s and routes new-index
builds and `InsertAll` through `prolly.MutableMap`, so single-document inserts
already touch O(log N) chunks per index. Three gaps stop the indexes from
behaving like dolt's:

1. **`UpdateAll` does not touch secondary indexes.** `collection.go:1754-1762`
   re-persists whatever in-memory index AMs it finds; it never deletes the old
   secondary entry or inserts the new one. After an update that changes an
   indexed field, the index is stale and equality lookups return wrong results.
2. **`DeleteAll` does not touch secondary indexes.** `collection.go:1905-1910`
   has the same shape: documents are removed from the primary map, but
   secondary entries keying those `_id`s are left behind.
3. **3-way merge ignores secondary indexes.** `mergeAddressMapsWithConflicts`
   in `merge_conflict.go:740-849` merges only the primary `prolly.Map` per
   collection. `dtblHashForCollection` then inlines `state.collIndexAMs[name]`,
   which is the *into-branch's* in-memory index AM. Documents added by the
   "from" branch never appear in the merged-branch indexes, and chunk reuse
   across the merge is whatever `state.collIndexAMs` happened to contain,
   which by construction did not see "from"'s edits.

Even where the path is correct (`InsertAll`), there is no test that asserts
chunk-level structural sharing across branches. The user's two-branch
"a..m / n..z" scenario is not exercised anywhere in
`/workspace/dumbodb/internal/backends/dolt/*_test.go`.

## 2. Requirements

Cost (steady-state, per write):

- R1. A write that touches `k` documents must cost `O(k * log N)` chunk writes
  per affected secondary index, where `N` is the index size. Equivalently,
  no write path may iterate the index or rebuild it. A bulk write of `k`
  documents may amortize through dolt's `MutableMap` pending-edits buffer
  (`tuples.Edits`, flushed at 64 KB) but must not require a full scan.
- R2. A write that does not change any indexed field must produce zero edits
  on that index (the dolt equivalent: `isNoopUpdate`,
  `prolly_index_writer.go:419-434`).
- R3. Persisting after a write batch must produce a new index root whose
  chunks below the mutation frontier are identical (by hash) to the pre-write
  root. Verified by counting distinct chunks before and after.

Structural sharing (cross-branch):

- R4. Disjoint-range writes on two branches must merge into a single index
  whose leaf chunks are reused by content hash from both sides. Concretely,
  for the "a..m on branch X, n..z on branch Y" scenario, leaf chunks above
  some splitter on each side appear unchanged in the merged tree.
- R5. A document that exists on both branches with the same indexed-field
  value contributes the same index leaf to both branches' index roots.
- R6. The DTBL written after a merge inlines an index `AddressMap` whose
  index roots reflect the merged primary, not the into-branch's stale view.

Read performance (must not regress):

- R7. Equality and range lookups continue to use `prolly.Map.IterKeyRange`
  (`index.go:191`), touching only chunks along the bound paths.
- R8. `tryIndexedCount` and `DistinctScan` continue to skip per-document
  fetches when the filter shape allows.

Conflict semantics:

- R9. Unique-index violations exposed by a merge surface through the existing
  artifact path (`merge_conflict.go:828-832`), modeled on dolt's
  `replaceUniqueKeyViolation` (`violations_unique_prolly.go:83-109`).
- R10. Partial-filter and sparse indexes correctly add/remove entries on
  membership transitions (a document that newly satisfies / stops satisfying
  the partial filter after an update).

## 3. Dolt-Grounded Findings (Appendix A)

These are the dolt code paths that the implementation should mirror or reuse.

### 3.1 Per-row write propagation

- `prollyTableWriter.Insert / Update / Delete`
  (`/workspace/dolt/go/libraries/doltcore/sqle/writer/prolly_table_writer.go:158-213`)
  iterates the slice of secondary index writers **before** writing the primary
  index, so unique-constraint failures abort the write before primary state
  changes.
- Each secondary writer is a `prollySecondaryIndexWriter`
  (`/workspace/dolt/go/libraries/doltcore/sqle/writer/prolly_index_writer.go:284-305`)
  holding a `prolly.MutableMapInterface` (a per-index mutable map) plus the
  key/val tuple builders.
- `Update` (`prolly_index_writer.go:436-462`) does the canonical pair:
  - `isNoopUpdate` (lines 419-434) short-circuits when the indexed columns
    are unchanged.
  - Otherwise: `Delete(oldKey)` then `Put(newKey, empty)`.
- DumboDB's analogue lives in `idxpkg.InsertEntry / DeleteEntry`
  (`internal/index/index.go:86-101`), which already exist; today only
  `InsertEntry` is called, and only on `InsertAll`.

### 3.2 Per-index mutability and flush

- `MutableMap` accumulates edits in a skip list (`tuples.Edits`,
  `/workspace/dolt/go/store/prolly/tuple_mutable_map.go:40`) and flushes via
  `flushPending` at `defaultMaxPending = 64 KB` (line 30). Flush calls
  `ApplyMutationsWithSerializer` (line 217), which in turn runs the
  tree-level `ApplyMutations` (`/workspace/dolt/go/store/prolly/tree/mutator.go:63-69`).
  `ApplyMutations` walks the old tree once, in key order, and the chunker
  emits only chunks that lie on the path of an edit. Unedited subtrees keep
  their existing addresses.
- This satisfies R1 and R3 for any write path that routes through
  `MutableMap`.

### 3.3 Persistence layout (`serial.Table`)

- The `serial.Table` flatbuffer (`/workspace/dolt/go/gen/fb/serial/table.fbs:19-42`)
  has a `secondary_indexes: [ubyte]` field that inlines an `AddressMap` keyed
  by index name (`/workspace/dolt/go/store/prolly/address_map.go:28-30`).
- DumboDB already inlines an `AddressMap` here (`helpers.go:184-223`,
  `buildDoltTableFlatbuffer`). The difference is the *value*: dumbodb stores
  `name -> JSON-IndexEntry-chunk-hash`, where the JSON entry carries metadata
  plus the prolly.Map root. Dolt stores `name -> per-index-root-hash`
  directly, with index metadata coming from the schema. The dumbodb wrapping
  is fine for structural sharing as long as the wrapper is content-addressed
  (it is: `writeIndexEntryChunk` -> `tree.SerializeJsonToAddr`,
  `index_persist.go:154-165`), but it is one extra chunk per index per
  metadata-change. We can keep it.

### 3.4 3-way merge of secondary indexes

- Entry point: `RootMerger.MergeTable`
  (`/workspace/dolt/go/libraries/doltcore/merge/merge_rows.go:204`) ->
  `mergeProllyTable` -> `mergeProllyTableData`
  (`merge_prolly_rows.go:56,92`).
- For each secondary index, dolt:
  - Constructs a `MutableSecondaryIdx` per index
    (`merge_prolly_rows.go:1483-1509`), pre-loaded with the left side's
    index root.
  - Streams the **primary-row diffs** (the output of `ThreeWayDiff` over the
    primary maps) and calls `applyEdit(idx, key, leftValue, mergedValue)`
    per row (`merge_prolly_rows.go:1511-1606`, `merge_prolly_indexes.go:172-190`).
    Each `applyEdit` resolves to a `Delete(oldIndexKey)` + `Put(newIndexKey)`
    on that index's mutable map.
  - When an index is missing, or its definition changed across the merge,
    falls back to `buildIndex`
    (`merge_prolly_indexes.go:75-96`, `:111-167`), which is a full scan of
    the merged primary.
- The diff that drives the per-index updates is a true 3-way prolly diff:
  `ThreeWayMerge` (`/workspace/dolt/go/store/prolly/tree/merge.go:56-104`)
  uses `PatchGeneratorFromRoots` and `SendPatches` to compare the two
  sides against the base in a structural walk. When both sides reach an
  identical chunk address, `SendPatches` (`merge.go:200-223`) accepts that
  subtree as-is. This is what gives R4: subtrees outside the edit window on
  each branch are physically reused in the merged root.
- Unique-constraint validation runs through `uniqValidator.validateDiff`
  (`merge_prolly_rows.go:709-837`) and emits artifacts via
  `replaceUniqueKeyViolation` (`violations_unique_prolly.go:83-109`).

### 3.5 Index schema reconciliation (does not apply to dumbodb)

Dolt has mutable column schemas, so it has to reconcile same-name-different-
definition indexes on merge (`mergeIndexes`,
`/workspace/dolt/go/libraries/doltcore/merge/merge_schema.go:762-786`).
DumboDB indexes are immutable by name: an index is created with a fixed
spec (keys, unique, sparse, partial filter) and editing it means dropping
and recreating under a different name. So:

- Two branches can never present the same index name with different specs.
  If they do, the names differ -- they are independent indexes and merge as
  separate AM entries.
- The dolt "rebuild on definition change" fallback
  (`merge_prolly_indexes.go:75-96` -> `buildIndex` at `:111`) is unreachable
  in dumbodb; we should not port it.
- The remaining cases are name-level only: index present on both sides
  (merge the maps), present on one side and absent from base (carry through),
  dropped on one side and modified on the other (conflict, mirroring the
  collection-level deleted-vs-modified handling in
  `merge_conflict.go:800-803`).

## 4. Dolt Code Reuse Strategy

The design's premise is that dolt has already solved this and dumbodb should
**use its types and call its functions directly** rather than re-implementing.
Dolt is a sibling repo we control, so when an API is internal or SQL-bound,
the answer is to lift / generalise it in dolt and depend on the lifted form
from both sides.

This section catalogues exactly what we use as-is, what we lift, and what
stays dumbodb-side.

### 4.1 Used as-is (no dolt change required)

The dumbodb backend already imports `github.com/dolthub/dolt/go/store/...`
freely, so these are pure go-import dependencies:

- **`prolly.MutableMap`** (`/workspace/dolt/go/store/prolly/tuple_mutable_map.go`)
  -- the per-index write buffer. Already used on `InsertAll`
  (`collection.go:1532`). Extend usage to update / delete paths and to merge.
- **`prolly.MergeMaps(left, right, base, cb)`**
  (`/workspace/dolt/go/store/prolly/tuple_map.go:171`) -- the public 3-way
  merge of two `prolly.Map`s with a `tree.CollisionFn`. This is exactly the
  primitive Phase 3 needs for the "present on both sides" branch.
  Caveat: the function carries a TODO ("MergeMaps does not properly detect
  merge conflicts when one side adds a NULL to the end of its tuple ...
  since MergeMaps is not currently called, fixing this is not a priority,"
  `tuple_map.go:173`). The index value tuple in dumbodb is a fixed single
  dummy byte (`idxValDesc` at `index/index.go:35`), so the NULL-suffix case
  cannot arise for us, but we should add a unit test covering it and, if
  the TODO is still live, fix it upstream as a paired PR -- it's a six-line
  comparator change at most.
- **`tree.ThreeWayDiffer[K, O]`**
  (`/workspace/dolt/go/store/prolly/tree/three_way_differ.go:61`) -- iterates
  divergent / convergent / clash diffs over three primary maps. This is what
  drives per-index `applyEdit` calls when one branch has an index the other
  side touched documents in (Phase 3b, "present on one side, absent on base").
- **`prolly.ArtifactsEditor`** + **`replaceUniqueKeyViolation`**
  (`/workspace/dolt/go/libraries/doltcore/merge/violations_unique_prolly.go:83`)
  -- the path that turns a unique-index conflict into a row in
  `dolt_conflicts`. Dumbodb already writes primary-conflict artifacts via
  `buildConflictArtifactHash` (`merge_conflict.go:828`); we extend that to
  emit `ArtifactTypeUniqueKeyViol` entries.
- **`tree.ApplyMutations`** (`/workspace/dolt/go/store/prolly/tree/mutator.go:63`)
  -- the underlying flush primitive; not called directly, used via
  `MutableMap.Map(ctx)`. This is what gives R1 / R3.
- **`prolly.AddressMap` + editor** (`/workspace/dolt/go/store/prolly/address_map.go`)
  -- the `name -> hash` map already used for the per-collection index AM
  (`index_persist.go:207`, `:234-243`). No change needed.

### 4.2 Lifted from dolt (small upstream PRs)

These are dolt types we want to call from dumbodb but which currently bind
to `schema.Schema` / `sql.Row`. The fix is to introduce a key-builder
interface in dolt, have dolt's existing SQL implementations satisfy it, and
have dumbodb supply a BSON implementation. Both sides then call the same
machinery.

- **`merge.MutableSecondaryIdx`**
  (`/workspace/dolt/go/libraries/doltcore/merge/mutable_secondary_index.go:116`)
  -- today its fields are `mut *prolly.MutableMap` plus two
  `index.SecondaryKeyBuilder` instances (left and merged). Proposed change
  (PR-1):

  ```go
  // in package merge (or hoisted to package prolly):
  type SecondaryKeyBuilder interface {
      SecondaryKeyFromRow(ctx context.Context, primKey, primVal val.Tuple) (val.Tuple, error)
  }
  ```

  `index.SecondaryKeyBuilder` (`/workspace/dolt/go/libraries/doltcore/sqle/index/key_builder.go`)
  already has this exact method shape; the change is to declare the
  interface and have `MutableSecondaryIdx`'s fields hold it. No call-site
  change in dolt; dumbodb supplies a `bsonSecondaryKeyBuilder` whose
  `SecondaryKeyFromRow` calls `idxpkg.BuildSecondaryKey`
  (`internal/index/index.go:54`) with the doc reconstructed from the
  `(primKey, primVal=JSONAddr)` pair via `readDocFromEntry`
  (`collection.go:diff.go:352`).

- **`merge.NewMutableSecondaryIdx`** signature (PR-1, same change) becomes:

  ```go
  func NewMutableSecondaryIdx(idx prolly.Map, left, merged SecondaryKeyBuilder) MutableSecondaryIdx
  ```

  shedding the `sql.Context`, `tableName`, and `schema.Schema` parameters
  that were only needed to *build* the dolt-side key builders. Dolt's
  existing call sites (`merge_prolly_rows.go:1490`) move the
  `index.NewSecondaryKeyBuilder` calls out into the caller, where the SQL
  context is already in scope.

- **`merge.applyEdit`** (`/workspace/dolt/go/libraries/doltcore/merge/merge_prolly_indexes.go:172`)
  -- 18 lines, depends only on the lifted `MutableSecondaryIdx`. No change
  needed beyond PR-1.

- **`merge.secondaryMerger`** (`/workspace/dolt/go/libraries/doltcore/merge/merge_prolly_rows.go:1471-1606`)
  -- coordinates one `MutableSecondaryIdx` per index against the
  `tree.ThreeWayDiff` stream. Today it also reads `tm.leftTbl.GetIndexSet`
  and `schema.Schema` to build the per-index writers. Proposed change
  (PR-2): split into

  - `MergeSecondaryIndexes(ctx, idxs []MutableSecondaryIdx, diffs *tree.ThreeWayDiffer[...]) ([]prolly.Map, error)`
    -- the pure inner loop, callable from dumbodb,
  - a thin SQL-side wrapper that builds the `MutableSecondaryIdx` slice
    from the dolt `IndexSet` and then calls the pure inner loop.

  Dolt's existing behaviour is preserved; dumbodb supplies the slice
  directly from its own `state.secIndexMaps[c.name]`.

- **`creation.BuildSecondaryProllyIndex`** /
  **`BuildUniqueProllyIndex`** (`/workspace/dolt/go/libraries/doltcore/table/editor/creation/index.go:154,198`)
  -- the bulk index-from-primary builder. Today it takes `schema.Schema`
  and `schema.Index` purely to construct a `SecondaryKeyBuilder` internally.
  Proposed change (PR-3): add overloads `BuildSecondaryProllyIndexFromBuilder`
  and `BuildUniqueProllyIndexFromBuilder` that accept the
  `SecondaryKeyBuilder` interface directly, and have the existing functions
  call those. Dumbodb's `buildSecondaryIndex` (`collection.go:2497`) is
  replaced by a call to the builder-from-builder variant; the dumbodb side
  no longer needs to handcraft the primary-scan loop.

### 4.3 Stays dumbodb-side

Even with maximal dolt reuse, three pieces remain on the dumbodb side
because they encode dumbodb's specific document model.

- **`internal/index/index.go`** -- the BSON-to-`KeyString` encoding
  (`BuildSecondaryKey` at `:54`, `EncodeValue` in `keystring.go`). This is
  the dumbodb analogue of dolt's column-projection key builder; both feed
  the same `prolly.Map`.
- **The doc-resolution step** -- given a primary `(key, value=JSONAddr)`
  pair, fetch the BSON document to extract indexed fields. Already lives
  in `diff.go:readDocFromEntry`. The `SecondaryKeyBuilder`
  implementation for dumbodb wraps this.
- **Multi-key expansion** -- dolt's secondary indexes are scalar per column;
  dumbodb expands array-valued indexed fields via `expandMultiKeyValues`
  (called at `collection.go:1544`). The dumbodb `SecondaryKeyBuilder`
  returns multiple tuples per primary row when the indexed field is an
  array, which means the interface above must be plural:

  ```go
  type SecondaryKeyBuilder interface {
      SecondaryKeysFromRow(ctx context.Context, primKey, primVal val.Tuple, out []val.Tuple) ([]val.Tuple, error)
  }
  ```

  -- it appends keys to `out` and returns it. Dolt's existing single-key
  builders append exactly one. This shape stays correct for both backends
  and folds multi-key naturally into PR-1.

### 4.4 What we change in dolt vs. what we work around

The principle is **change dolt, do not fork or shadow**. We do not want
two copies of `applyEdit` or two `MutableSecondaryIdx` types drifting. The
upstream PRs above (PR-1 through PR-3) are small, mechanical, and
zero-behaviour-change for dolt's existing SQL surface. Each is a candidate
for landing on dolt's `main` before the dumbodb code change that depends on
it.

If, for scheduling reasons, we need to land the dumbodb change before the
dolt PR ships, the fallback is to type-assert against a build-tagged shim
inside `internal/backends/dolt/` that wraps the not-yet-public dolt
function. We accept that as a temporary measure, never as an architecture.

## 5. Implementation Plan

### Phase 0. Tests that fail today (red bar)

Before any code changes, land the failing tests so the gap is captured:

- `index_update_test.go`: insert a doc, update an indexed field, equality
  lookup on the new value (must return the doc) and on the old value (must
  return nothing). Today the new-value lookup misses and the old-value
  lookup hits. (Closes part of `workspace-4ee`.)
- `index_delete_test.go`: insert, delete by `_id`, equality lookup on the
  indexed value must return nothing. (Closes part of `workspace-4ee`.)
- `index_merge_test.go`: branch X writes `{field: "alpha"}` through
  `{field: "mike"}`, branch Y writes `{field: "november"}` through
  `{field: "zulu"}`, both branches share an index on `field`. Merge Y into
  X; equality lookups on both halves must succeed on the merged branch.
  (Closes part of `workspace-ife`.)
- `index_chunk_reuse_test.go`: same scenario, but instead of assertions on
  lookups, walk the index prolly tree pre- and post-merge and count chunks
  that share addresses with each parent. At least one non-root chunk from
  each parent index must appear in the merged index. This is the structural
  bar from R4. Use `tree.Node.Address()` and `prolly.Map.WalkAddresses` from
  dolt.

### Phase 1. Wire `UpdateAll` / `DeleteAll` through `MutableSecondaryIdx`

Land dolt PR-1 (Section 4.2) first, then on the dumbodb side:

- Add `internal/index/builder.go` containing
  `type DocSecondaryKeyBuilder struct { idx backends.IndexInfo; ns tree.NodeStore }`
  with method
  `SecondaryKeysFromRow(ctx, primKey, primVal val.Tuple, out []val.Tuple) ([]val.Tuple, error)`.
  Implementation: reconstruct the BSON doc via `readDocFromEntry`
  (`diff.go:352`), run `extractIndexFieldValues` (`collection.go:2570`),
  optionally short-circuit on `MatchesPartialFilter`, and for each value in
  `expandMultiKeyValues` build a tuple via `idxpkg.BuildIndexEntry`
  (`index.go:65`).
- In `collection.go`, replace the bespoke
  `updateSecondaryIndexesOnInsert` (`:1510`) with a call into dolt's
  `merge.MutableSecondaryIdx.InsertEntry` per index, instantiated once per
  write batch from the dumbodb key builder. Multi-key arrays come out of
  the builder's appended-slice return value (Section 4.3).
- In `UpdateAll` (`:1639`), for each updated doc:
  - Run the dolt `isNoopUpdate` analogue: build old and new keys, sort,
    compare. If equal, skip (R2).
  - Otherwise call `MutableSecondaryIdx.UpdateEntry(ctx, primKey,
    oldPrimVal, newPrimVal)` per index. The dolt method already does
    `Delete(old) + Put(new)`
    (`mutable_secondary_index.go:156-172`). The old doc is already on
    hand at `:1700` via `existingHash`; thread it as a val.Tuple.
- In `DeleteAll` (`:1790`), call `MutableSecondaryIdx.DeleteEntry(ctx,
  primKey, oldPrimVal)` per index per doc.
- After the per-doc loop, finalise each index with
  `MutableSecondaryIdx.Map(ctx)` and write through `persistIndexes`.

Cost: one prolly.Map.Get per *unique* index per write (the unique-validation
probe; non-unique indexes are pure mutate path). No scan. Satisfies R1.

### Phase 2. Unique-index enforcement on write

Reuse the same machinery dolt uses on write, not just on merge. Dolt's
`prollySecondaryIndexWriter.Insert` / `Update`
(`/workspace/dolt/go/libraries/doltcore/sqle/writer/prolly_index_writer.go`)
returns `sql.UniqueKeyError` on duplicate; the merge path emits an artifact
via `replaceUniqueKeyViolation`. We mirror the same split:

- **Write-path enforcement.** Before `MutableSecondaryIdx.InsertEntry` or
  the new-key half of `UpdateEntry`, probe the index range
  `[LowerBoundInclusive(v), UpperBoundExclusive(v))` (helpers already at
  `internal/index/index.go:116-134`). Any entry with a different primary
  ID rejects the write with a `WriteException` carrying MongoDB error
  code 11000 (DuplicateKey). This is a single range probe per unique
  index per row -- the same shape dolt uses.
- **Merge-path artifact.** Reuse
  `merge.replaceUniqueKeyViolation`
  (`/workspace/dolt/go/libraries/doltcore/merge/violations_unique_prolly.go:83`)
  from inside the `merge.MutableSecondaryIdx`-driven loop. The
  `ArtifactsEditor` already plumbed through `buildConflictArtifactHash`
  (`merge_conflict.go:828`) accepts the same artifact type.

Together this closes the gap that today's `backends.IndexInfo.Unique` is
metadata-only.

### Phase 3. Merge secondary indexes

Two sub-tasks. Both call dolt directly.

**3a. Expose the primary-row diff.** `captureConflictsForCollection`
(invoked at `merge_conflict.go:819`) already constructs a
`tree.ThreeWayDiffer` over the primary maps internally to detect document
conflicts. Refactor it to *return* the diff stream alongside the merged
map -- pure code motion, no semantic change -- so the per-index loop can
consume the same stream. This avoids walking the primary twice and uses
dolt's diff machinery without copying it.

**3b. Per-index merge.** Walk the index AMs from into / from / base
(`loadIndexesFromDTBL` already gives us the per-branch sets). Per index
name, four cases. Index *definitions* never differ when names match
(Section 3.5), so this is a name-level union:

- **Present on into, from, and base, all roots distinct:**
  Call `prolly.MergeMaps(intoMap, fromMap, baseMap, collisionCb)`
  (`/workspace/dolt/go/store/prolly/tuple_map.go:171`). The collision
  callback is `nil` for non-unique indexes (no collisions possible -- the
  key embeds the unique primary ID) and `replaceUniqueKeyViolation`
  (`violations_unique_prolly.go:83`) for unique indexes. This single call
  gives us R4 / R5 directly: the underlying `tree.MergeOrderedTrees` /
  `tree.ThreeWayMerge` (`store/prolly/tree/merge.go:62`) reuses identical
  chunks from both sides by hash.

- **Present on both into and from but base lacks the index:** treat the
  base as `prolly.NewEmptyMap(...)` and call `MergeMaps` the same way.
  Edge case validated by a unit test (Section 6.2).

- **Present on one side only (absent in base):** the existing side's
  index covers only that side's docs. Drive the dolt
  `MergeSecondaryIndexes` loop (Section 4.2, PR-2) over the primary
  diff stream with a `MutableSecondaryIdx` seeded from the existing side.
  Inserts and updates from the *other* side land via `applyEdit`. No
  other code is needed.

- **Present on base, dropped on one side, kept on the other:** Treat as
  the collection-level delete-vs-modify policy at
  `merge_conflict.go:800-803`. A drop on one side combined with primary-
  row edits on the other is a name-level AM conflict. Drop-on-both is a
  clean drop.

- **Absent on both sides:** nothing to do.

After the per-index loop, finalise each `MutableSecondaryIdx` via
`Map(ctx)`, update the per-collection index AM via its editor (existing
`persistIndexes` code), and write the merged DTBL via
`dtblHashForCollection`. The change is that the AM holds merged roots,
not the into-branch's stale roots.

**Why not use dolt's whole-table merge entry point?**
`merge.RootMerger.MergeTable` (`merge_rows.go:204`) is tempting because it
already does everything, but it is `sql.Context`-bound and assumes a
`durable.IndexSet` keyed by SQL schema. The cost of pulling that in would
be the same as the PRs above plus a synthetic SQL-schema shim for every
dumbodb collection. PR-1 / PR-2 are the cleaner cut: take the inner
primitives, leave the outer table-merge driver in dolt.

**Note on dolt code reuse.** `tree.ThreeWayMerge` is parameterised on
`Ordering[K]` (`merge.go:56-64`) and the val.Tuple ordering already used by
`prolly.Map` is compatible. We should be able to call it directly with the
existing tuple descriptors, without copying the algorithm. Open question
(see Section 6): does dolt expose a public wrapper at the `prolly.Map`
layer that we can use without dropping into `tree`?

### Phase 4. Cherry-pick / rebase / revert

`backend.go:1832`, `:2812`, `:3121` all call `mergeAddressMapsWithConflicts`.
Once Phase 3 lands they inherit index merging for free.

### Phase 5. Replace the JSON IndexEntry wrapper

Today dumbodb's per-collection index AM stores
`name -> JSON-IndexEntry-chunk-hash`, where the JSON entry carries metadata
plus the prolly.Map root (`index_persist.go:42-49`). Dolt's `serial.Table`
stores `name -> per-index-root-hash` directly and parks index metadata in
the table schema (`/workspace/dolt/go/gen/fb/serial/table.fbs`,
`schema.Index`).

Plan: introduce a per-collection metadata chunk (a small flatbuffer mirroring
the relevant fields of `schema.Index`: name, keys, unique, sparse, partial
filter) referenced from the DTBL, and change the AM to `name -> root-hash`.
Two benefits:

- The AM lookup becomes one chunk read, matching dolt.
- `persistIndexes` no longer writes a JSON chunk per persist call (small
  but real savings on write-heavy workloads).

This is independent of correctness and can ship after Phases 1-3. We can
defer if upstream PR-1/PR-2/PR-3 reveal a better metadata-sharing scheme.

## 6. Testing Plan

### 6.1 Unit-level correctness (Phase 1, 2)

- `TestUpdateAllUpdatesSingleFieldIndex` -- index on `field`, update
  changes `field`, both lookups behave.
- `TestUpdateAllNoChangeNoIndexEdit` -- update touches an unindexed field;
  walk the index root before and after, assert the hash is identical (R2).
- `TestUpdateAllMultikeyIndex` -- update changes one element of an array
  field; old keys for unchanged elements must remain.
- `TestDeleteAllRemovesIndexEntries` -- delete one of N documents sharing an
  indexed value; lookup returns N-1 docs.
- `TestUniqueIndexBlocksDuplicateInsert` and
  `TestUniqueIndexBlocksDuplicateUpdate`.
- `TestPartialIndexMembershipTransition` -- update that changes the partial
  filter result both ways.

### 6.2 Structural sharing (R3, R4, R5)

A new helper `countSharedChunks(rootA, rootB hash.Hash, ns tree.NodeStore)`
that walks both trees and returns the set of addresses present in both.
Reuse dolt's tree walk; do not reimplement.

- `TestIndexChunkReuseOnNoopUpdate` -- assert pre/post root hashes equal.
- `TestIndexChunkReuseOnBranchedInserts` -- the user's scenario. Branch X
  writes keys "alpha".."mike", branch Y writes "november".."zulu". Assert:
  - X's index root and Y's index root share the empty-tree chunk at most,
    and more interestingly,
  - the merged index root, after Phase 3, contains at least the leaf chunks
    that lie strictly below "m" from X and strictly above "n" from Y.
- `TestIndexChunkReuseOnOverlap` -- both branches insert the *same* doc
  (same `_id`, same indexed value). The leaf chunk holding that entry must
  be byte-identical on both branches and in the merged root.

### 6.3 Read-path regression (R7, R8)

Existing tests in `index_test.go`, `index_partial_test.go`,
`prefilter_range_test.go`, `index_bench_test.go` must continue to pass.
The benchmarks in `index_bench_test.go` should not regress more than 5% on
single-doc inserts after Phase 1 (the only new cost is one prolly.Map.Get
per unique index, which we will run only when `idxInfo.Unique`).

### 6.4 Cross-DB sanity (against dolt)

Build a small Go test binary that, given an in-memory `tree.NodeStore`,
runs the same insert sequence through a hand-built dolt SQL table and a
dumbodb index. Both go through the same lifted `MutableSecondaryIdx`
machinery (Section 4.2) but with different `SecondaryKeyBuilder`s. After
each batch:

- The number of chunks rewritten per insert should be identical between
  the two paths -- they share the flush code in `tree.ApplyMutations`.
- The merged-index leaf-chunk reuse percentage on the branched-write
  scenario (R4) should be within 1% between the two paths.

This is the empirical check that dumbodb genuinely shares dolt's
behaviour rather than approximating it.

## 7. Open Questions

- Q1. **`MergeMaps` TODO.** `tuple_map.go:173` notes that `MergeMaps`
  "does not properly detect merge conflicts when one side adds a NULL to
  the end of its tuple." Dumbodb's `idxValDesc` is a fixed single byte
  (`index/index.go:35`) so the case can't fire for us, but the comment
  also says "since `MergeMaps` is not currently called, fixing this is
  not a priority." Once dumbodb starts calling it, we own that priority.
  Decide whether to fix upstream as part of Phase 3 or to keep dumbodb-
  side regression coverage as a tripwire.

- Q2. **PR-1 / PR-2 / PR-3 ordering.** Dolt's release cadence is
  independent from dumbodb's. Confirm the policy for landing dependent
  PRs across the two repos and whether a single batched upstream PR
  (one PR adding the interface plus all three call-site touches) is
  preferred over three independent ones.

- Q3. **Primary-ID stability.** The dumbodb index key includes a 20-byte
  SHA-512-derived primary ID (`helpers.go:225-279`). Across branches, the
  same MongoDB `_id` must always produce the same 20 bytes for R5 to
  hold. Confirm no per-database salt anywhere on the hash path; add a
  regression test.

- Q4. **Artifact surfacing.** The artifact map writer
  (`buildConflictArtifactHash`) currently only emits primary-row
  conflicts. Confirm `ArtifactTypeUniqueKeyViol` round-trips through
  dumbodb's read side of `dolt_conflicts`-equivalent.

- Q5. **Capped collections.** `evictCappedDocs` (`collection.go:1566`)
  deletes from the primary map directly. It must also drive
  `MutableSecondaryIdx.DeleteEntry` per eviction; verify before Phase 1
  ships.

## 8. Out of Scope

- Compound indexes beyond what `extractIndexFieldValues` /
  `expandMultiKeyValues` already support.
- Text / geospatial / hashed index merging. These have their own encoding
  paths and the same plan applies, but each needs its own ordering for
  `ThreeWayMerge` correctness.
- Online index build under concurrent writes. Today dumbodb takes
  `state.mu` for all writes; the design assumes that lock continues to
  serialise.
