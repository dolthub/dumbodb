# Secondary Indexes with Structural Sharing

**Issues:** workspace-4ee (update/delete gap), workspace-ife (merge gap)
**Date:** 2026-06-09
**Status:** Draft

## 1. Goal

Secondary indexes should behave like the primary document store already
does: every write keeps them correct, write cost scales with the size of
the write (not the size of the collection), and branches share unchanged
index storage with each other and with their merge results.

Today only the insert path maintains indexes. Updates, deletes, and
merges silently leave indexes stale, and unique-index validation on
insert scans the whole collection. This doc defines the expected
behaviors, the tests that pin them, and the plan to get there.

## 2. Expected Behaviors

Each behavior states what a user or test should observe. Status is one
of: WORKS (passes today), BROKEN (wrong result today), MISSING (feature
absent today).

Every behavior is pinned by tests in one of two categories:

- **Parity** -- the behavior is observable through the wire protocol
  and MongoDB defines the correct answer. These live in
  `dumbodb-parity-testing/tests/` and run the identical operations
  against MongoDB and DumboDB side by side. Red-bar means landing
  them as `DumboDBXFail` (divergence expected and recorded); the fix
  flips them to `DumboDBFull`. One behavior usually needs a family
  of parity cases, not a single test.
- **DumboDB-only** -- the behavior has no MongoDB equivalent: version
  control (branches, merges, history ops) and storage internals
  (chunk addresses, root hashes, encodings, benchmarks). These live
  in `internal/backends/dolt/` and `internal/index/`.

### 2.1 Writes keep indexes correct (single branch)

- **W1. Insert is indexed.** After inserting a doc, an equality or
  range lookup on its indexed field finds it.
  Status: WORKS.
  Tests: parity (existing index suites) and dumbodb-only
  (`index_test.go`).

- **W2. Update re-indexes.** After updating a doc's indexed field from
  A to B, queries behave exactly as MongoDB's: lookup by B finds the
  doc, lookup by A does not.
  Status: BROKEN -- both halves diverge from MongoDB; the index still
  says A.
  Tests: parity family `Index_UpdateReindex_*` covering each update
  shape -- `$set` to a new value, `$unset`, `$inc`, whole-document
  replace, multi-document update, and upsert-as-update -- each
  asserting find-by-old and find-by-new against MongoDB side by side.

- **W3. Delete un-indexes.** After deleting a doc, lookups on its
  indexed values return nothing.
  Status: BROKEN -- stale entries remain and lookups return the
  deleted doc.
  Tests: parity family `Index_DeleteUnindex_*` -- deleteOne by _id,
  deleteOne by indexed-field filter, deleteMany, findAndModify
  remove.

- **W4. Untouched indexes are untouched.** An update that does not
  change any indexed field leaves the index storage bit-for-bit
  identical (same root hash).
  Status: MISSING (vacuously "true" today only because updates never
  touch indexes at all).
  Test: dumbodb-only `TestNoopUpdateLeavesIndexRootUnchanged` (root
  hashes are not observable over the wire).

- **W5. Array (multikey) updates are incremental.** Updating one
  element of an indexed array field fixes entries for the changed
  element only; lookups on unchanged elements still hit, the removed
  element misses, the added element hits.
  Status: BROKEN (no update maintenance at all).
  Tests: parity family `Index_MultikeyUpdate_*` ($push, $pull,
  $set of one element, full array replace) for the query-visible
  half; dumbodb-only test asserting only changed entries were
  edited (storage-level, see P2).

### 2.2 Index membership rules (sparse / partial)

Index *content* should be the single source of truth for which docs an
index covers. Today membership rules are scattered through validation
and read paths while the stored index covers everything.

- **M1. Sparse indexes skip absent fields.** A doc missing the indexed
  field has no entry in a sparse index; if an update adds the field the
  entry appears, and if an update unsets it the entry disappears.
  Status: BROKEN -- sparse indexes store Null entries for missing
  fields; reads compensate case-by-case.
  Tests: parity family `Index_Sparse_*` (query results, sparse-unique
  coexistence -- MongoDB defines all of it); dumbodb-only assertion
  that the stored index omits non-member docs (entry-count or
  root-hash level).

- **M2. Partial indexes track their filter.** A doc only has entries
  in a partial index while it satisfies the filter expression; updates
  that cross the boundary (either direction) add or remove entries.
  Status: BROKEN -- same everything-indexed issue as M1.
  Tests: parity family `Index_Partial_*` (membership transitions in
  both directions, queried side by side); dumbodb-only stored-content
  assertion as in M1.

### 2.3 Unique indexes

- **U1. Duplicate insert is rejected.** Inserting a doc whose unique
  key collides with an existing doc fails with duplicate-key error
  11000; the collection is unchanged.
  Status: WORKS, but by scanning the entire primary per insert batch
  -- correct result, disqualifying cost (see P1).
  Tests: parity coverage exists; keep green through the rework.

- **U2. Colliding update is rejected.** An update that would change a
  doc's unique key to collide with another doc fails the same way and
  leaves both docs unchanged.
  Status: MISSING -- updates perform no unique validation.
  Tests: parity family `Index_UniqueUpdate_*` ($set collision,
  replace collision, error code and message shape, doc unchanged
  after failure).

- **U3. Unique respects sparse/partial membership.** Two docs both
  missing a sparse-unique field coexist; two docs both outside a
  partial-unique filter coexist.
  Status: WORKS in validation logic; must stay true when validation
  moves to index probes (depends on M1/M2).
  Tests: parity (folded into the `Index_Sparse_*` / `Index_Partial_*`
  families).

### 2.4 Write cost and storage sharing (single branch)

- **P1. Write cost scales with the write.** A write touching k docs
  performs O(k log N) index work. No write path may scan or rebuild an
  index or the primary. Concretely: doubling collection size must not
  measurably change per-doc insert/update/delete latency.
  Status: BROKEN for unique inserts (full primary scan per batch,
  U1); WORKS for non-unique inserts; not yet applicable for
  update/delete (no maintenance exists).
  Tests: dumbodb-only -- `BenchmarkInsertWithUniqueIndexScalesFlat`
  plus benchmark guards on update/delete (cost is not wire-visible).

- **P2. Small writes share storage with the previous version.** After
  a write batch, index tree chunks outside the touched key range have
  identical addresses to the pre-write tree (structural sharing across
  time).
  Status: WORKS for insert; MISSING for update/delete.
  Test: dumbodb-only `TestIndexChunkReuseAcrossWrites` (walks pre/post
  trees, asserts shared chunk addresses).

- **P3. Reads stay bounded.** Equality/range lookups, indexed count,
  and distinct-scan touch only chunks on the lookup path. No
  regression permitted by any phase.
  Status: WORKS.
  Tests: dumbodb-only (existing index read tests and benchmarks).

### 2.5 Branches and merges

All behaviors in this section are version-control or storage concerns
with no MongoDB equivalent; every test here is dumbodb-only. The one
exception: post-merge *query* results are still wire-observable, so B2
and B4 each get a follow-on parity sweep (run the standard index query
families against the merged branch via the `dbname@branch` extension)
in addition to their dumbodb-only scenario tests.

- **B1. Same doc, same bytes.** A doc inserted with the same _id and
  indexed value on two branches produces byte-identical index leaf
  chunks on both (the chunk store deduplicates them).
  Status: expected-WORKS (key encoding is branch-independent), never
  asserted.
  Test: dumbodb-only `TestSameDocSameIndexLeafAcrossBranches`.

- **B2. Merged indexes are correct.** After merging branch Y into
  branch X, index lookups on the merge result find exactly the docs in
  the merged collection: docs added on either side hit; docs deleted
  on either side miss; docs whose updates merged field-wise are
  findable by their *merged* values (even when the merged doc existed
  on neither parent).
  Status: BROKEN -- merge keeps X's index untouched; Y's writes are
  invisible to index lookups.
  Tests: dumbodb-only `TestMergedIndexReflectsBothBranches`,
  `TestMergedIndexReflectsFieldMergedDocs` (the neither-parent case),
  `TestMergedIndexConflictKeepsOurs`; plus the post-merge parity
  sweep noted above.

- **B3. Merged indexes share storage with both parents.** For
  disjoint-range writes (X writes "a".."m", Y writes "n".."z"), the
  merged index reuses leaf chunks from both parents rather than
  rewriting the tree.
  Status: MISSING.
  Test: dumbodb-only `TestMergedIndexChunkReuseFromBothParents`.

- **B4. One-sided indexes survive a merge and cover both sides.** An
  index created on only one branch since the base exists after the
  merge and covers documents written on the *other* branch.
  Status: BROKEN (it survives but does not cover the other side's
  docs).
  Tests: dumbodb-only `TestOneSidedIndexCoversMergedDocs`; plus the
  post-merge parity sweep noted above.

- **B5. A drop wins.** The index-level three-way cases when the two
  sides disagree about an index's existence. Merging Left (ours) and
  Right (theirs) against Base:

  - Base: has Index-A. Left: dropped Index-A. Right: still has
    Index-A, its content advanced by Right's document writes.
    Result: Index-A is absent from the merge. Right's documents
    still merge into the collection; only the index is gone. The
    drop wins over the content change because indexes are derived
    data -- recreating is cheap and explicit, and nothing is lost.
    No warning artifact; the drop is silent and final (decided).
  - The mirror image (Right dropped, Left kept and wrote): same
    result, Index-A absent.
  - Base: has Index-A; both sides dropped it: absent, trivially.
  - Base: has Index-A; Left dropped it and re-created the same name
    with the same spec: the name exists on both sides with matching
    specs, so this is the ordinary present-on-both content merge
    (B2/B3) -- the drop+recreate is invisible at the name level.
  - Base: has Index-A; Left dropped it and re-created the same name
    with a DIFFERENT spec; Right did not alter the definition
    (Right's spec still equals Base's, regardless of Right's
    document writes). Result: Left's re-created index wins -- the
    definition change is an intentional edit and the unaltered side
    has no competing claim. The winning index must still cover
    Right's documents (same obligation as B4: seed from Left's
    index, apply Right's document diffs).
  - Base: has Index-A; BOTH sides altered the definition (each
    dropped and re-created with specs that differ from Base's and
    from each other): conflict -- there is no unaltered side to
    defer to. Surfaced as a merge error naming the index.
  - Base: lacks Index-A; one side created it: not a drop at all --
    that is B4 (carry it and cover both sides' docs).

  The general rule: an index definition change (drop, or
  drop+recreate with a new spec) wins over a side that left the
  definition untouched; two competing definition changes conflict.
  "Altered" is judged by comparing each side's spec fields (keys,
  unique, sparse, partial filter) to Base's -- index content (the
  map root) does not count as alteration.

  Status: MISSING (never exercised).
  Tests: dumbodb-only `TestDroppedIndexStaysDroppedAfterMerge` (one
  case per bullet, both directions).

- **B6. Merge-created unique violations are conflicts.** When each
  branch inserts a different doc with the same unique key, the merge
  records a conflict requiring manual resolution -- the standard
  conflict resolution workflow (Section 2.6).
  Status: MISSING.
  Test: dumbodb-only `TestMergeUniqueViolationIsConflict`.

- **B7. Cherry-pick, rebase, and revert behave like merge.** All of
  B2-B6 hold for these operations (they share the merge machinery).
  Reverting a commit that created an index drops it.
  Status: BROKEN/MISSING in line with B2-B6.
  Tests: dumbodb-only, one scenario per operation in
  `index_history_ops_test.go`.

- **B8. Branch isolation (already shipped).** Creating or dropping an
  index on one branch never changes another branch's behavior.
  Status: WORKS (branch-scoped metadata refactor).
  Tests: dumbodb-only `index_branch_isolation_test.go`. Must stay
  green.

### 2.6 Conflict resolution

Resolving a merge conflict is a write: it replaces (or deletes) a
document in the in-progress merge state, and whatever document state
ends up in the resulting dumboCommit must be reflected in the indexes
of that commit. This is where membership rules bite hardest: with a
partial index on `{status: "active"}`, resolving a conflict by
changing `status` to `"inactive"` must remove the doc's index entry,
even though no ordinary update ever ran.

Today `DumboDBResolveConflict` writes the chosen value directly into
the merged primary map and re-attaches the existing index AM
untouched -- a third index-bypassing write path alongside UpdateAll
and DeleteAll. All behaviors below are dumbodb-only (MongoDB has no
merge conflicts), but each test should include the self-consistency
check that index-driven query results equal full-scan results on the
same branch -- that equivalence is what parity tests give us
elsewhere and it needs no MongoDB to assert.

- **C1. Resolving "theirs" re-indexes.** After resolving a conflict
  with theirs, lookups find the doc by theirs' field values and no
  longer find it by ours' values. Resolving with theirs-deleted
  removes all the doc's index entries.
  Status: BROKEN -- the index keeps ours' entries regardless.
  Tests: dumbodb-only `TestResolveTheirsReindexes`,
  `TestResolveTheirsDeleteUnindexes`.

- **C2. Resolving "custom" re-indexes.** After resolving with a
  custom document, lookups find the doc by the custom values; both
  parents' old values miss.
  Status: BROKEN -- same bypass.
  Test: dumbodb-only `TestResolveCustomReindexes`.

- **C3. Resolution can flip membership.** The motivating example: a
  resolution that moves a doc across a partial-filter boundary (or
  sets/unsets a sparse field) adds or removes its entries
  accordingly, in both directions.
  Status: BROKEN (compounds the M1/M2 gap with the resolve bypass).
  Tests: dumbodb-only `TestResolveCrossesPartialFilterBoundary`,
  `TestResolveSetsUnsetsSparseField`.

- **C4. Resolution respects unique constraints.** A "custom"
  resolution whose unique key collides with a different doc is
  rejected like any other write (duplicate-key, the conflict stays
  unresolved). A "theirs" resolution that collides surfaces the same
  way the merge itself would (B6 artifact path).
  Status: MISSING -- no validation on the resolve path.
  Test: dumbodb-only `TestResolveUniqueCollisionRejected`.

- **C5. The committed root is index-correct.** Catch-all: after any
  sequence of resolutions ("ours", "theirs", "custom", deletions, in
  any order) followed by the commit that concludes the merge, the
  committed indexes describe exactly the committed documents.
  Status: BROKEN.
  Test: dumbodb-only `TestResolvedMergeCommitIndexConsistency`
  (randomized resolution sequence; assert index-scan == collscan for
  every index).

### 2.7 Mixed-type values in index keys

MongoDB allows a single indexed field to hold different BSON types
across documents and defines a total order over them (the type bracket
order: MinKey < Null < Numbers < String < Object < Array < BinData <
ObjectId < Boolean < Date < Timestamp < Regex < MaxKey, with all
numeric types compared by value inside one bracket). DumboDB's
KeyString encoding (`internal/index/keystring.go`) copies MongoDB's
ctype byte layout, so the *bracket* order is already right; the
problems are inside and outside the brackets. Verified by direct
byte-comparison probes on 2026-06-09:

- **T1. Type brackets sort in Mongo order.** Sorting or
  range-querying a field whose values span types returns documents in
  MongoDB's bracket order (null < numbers < string < bool < date
  etc.), and equality lookups never return documents from another
  bracket.
  Status: WORKS for the encoded types (verified at the byte level:
  null/int/string/bool/date brackets compare correctly), but never
  asserted against MongoDB.
  Tests: parity family `Index_MixedTypeBrackets_*` (sort over a
  mixed-type indexed field, equality and range queries that must not
  leak across brackets -- this is the essential coverage); supported
  by a dumbodb-only `TestKeyStringTypeBracketOrder` byte-comparison
  unit test pinning the encoding itself.

- **T2. Numeric types compare by value, not representation.** int32,
  int64, and float64 holding the same number produce the same index
  key (2 == 2.0), and mixed int/float ranges sort numerically.
  Status: BROKEN for non-integer floats. Whole-value floats unify
  correctly (verified: KS(int64(2)) == KS(float64(2.0))), but every
  non-integer float is bucketed above all integers: KS(2.5) sorts
  after KS(3), KS(0.5) sorts after KS(1) (verified by byte compare).
  Consequence: an indexed range scan for `{n: {$gt: 2.5}}` starts
  past the integer bucket and silently misses n=3 -- wrong query
  results on the find path today, not just a perf issue. This breaks
  the soundness contract `indexBoundsForFilterValue` claims.
  Tests: parity family `Index_MixedNumeric_*` (find and count with
  $gt/$gte/$lt/$lte bounds crossing the int/float boundary, sort over
  mixed numerics -- MongoDB defines every answer); dumbodb-only
  encoding-order test pinning the byte-level fix.

- **T3. Types the encoder cannot represent must not produce index
  plans.** Decimal128, Timestamp, and Regex values fall through
  `EncodeValue`'s default case and encode as Null; NaN and +/-Inf
  also encode as Null; all embedded documents encode as one
  marker byte (no content), as do nested arrays. Any of these in
  index *content* is tolerable for find (the handler re-filters
  false positives) but must never feed a *plan that skips
  re-validation*: today `tryIndexedCount` counts raw index entries
  in the computed range and its bounds builder accepts these values,
  so `count({field: null})` includes Decimal128/Timestamp docs and
  `count({field: <some object>})` counts every object-valued doc.
  The Phase 2 unique probe would inherit the same falseness (two
  different Decimal128 values would look like duplicates).
  Status: BROKEN for counts today; blocks Phase 2.
  Tests: parity family `Index_LossyTypes_*` (find and count with
  Decimal128 / Timestamp / NaN / object / nested-array values, each
  compared against MongoDB -- correct whether dumbodb answers by
  guard-and-scan or by faithful encoding); dumbodb-only test pinning
  that the planner rejects lossy values (no index plan chosen).

- **T4. Multikey arrays may mix element types.** An indexed array
  field whose elements span types indexes each element under its own
  bracket; lookups on each element value hit.
  Status: expected-WORKS (multikey expansion encodes each element
  independently); never asserted with mixed-type elements.
  Tests: parity family `Index_MultikeyMixedTypes_*`.

The fix has two tiers. Tier 1 (guards, small): make
`indexBoundsForFilterValue` and the entry-generation path reject
values whose encoding is lossy (Decimal128, Timestamp, Regex, NaN,
Inf, Document, Array-as-element) so plans fall back to scans, and fix
the float bucketing so T2 holds. Tier 2 (fidelity, larger, optional
per type): add
faithful encodings (Mongo solves Decimal128/double unification with
continuation bytes; objects/arrays encode recursively) so the guard
list shrinks and counts/probes accelerate for those types. Tier 1 is
required before Phase 2; Tier 2 items are independent follow-ups.

### 2.8 Explicitly out of behavior scope

- Text / geo / hashed index merging, KeyString decoding (covered
  queries), online index builds, runtime $or execution.
- Tier 2 encoding fidelity for Decimal128 / Timestamp / Regex /
  nested documents (Section 2.7) beyond the soundness guards --
  each is its own follow-up.

## 3. How (mechanism, briefly)

The implementation approach in one paragraph per area. Code-level
grounding lives in the appendix.

**Writes.** Indexes are prolly maps; entry keys are
`KeyString(values) || 0x04 || primaryID`, so every maintenance action
is a Put/Delete on a `prolly.MutableMap` -- O(log N) and
automatically chunk-sharing (P2). One new function,
`indexEntriesForDoc`, becomes the single place that turns (doc, index
spec) into entry keys, applying sparse/partial membership (M1/M2).
Update maintenance is "diff the old and new entry sets, delete/insert
the difference, skip if equal" (W2-W5); delete maintenance is the
delete half (W3). These extend the same pure
resolve-apply-rebuild-AM pattern the insert path already uses.

**Unique checks.** Replace the whole-collection scan with one bounded
range probe per unique index per written row -- the index itself
answers "does this key exist for a different _id" in O(log N) (P1,
U1-U3). Probes are only sound once index content respects membership
(M1/M2 first).

**Merges.** The primary-map merge already walks a three-way diff of
the documents and resolves them (including field-level merging). Index
maintenance rides that same diff stream: seed each surviving index
from one parent's index map (wholesale chunk reuse, B3), then apply
per-document entry edits for the other side's changes and for resolved
docs (B2, B4). Name-level reconciliation (which index definitions
survive) compares each side's spec fields to Base's: a definition
change (drop, or drop+recreate with a new spec) wins over an
untouched side, and two competing definition changes are a merge
error -- the full case table is in B5. A surviving index whose
definition only one side carries (B4, or the recreated-spec winner
in B5) seeds from that side's map and applies the other side's
document diffs. Unique collisions detected while applying edits
become conflict artifacts (B6). Cherry-pick/rebase/revert call the
same function (B7).

**Conflict resolution.** Resolving a conflict is an update (or delete)
against the in-progress merge state, so it routes through the same
entry-set diff used by ordinary updates: old entries from the document
currently in the merged map, new entries from the chosen value via
`indexEntriesForDoc` -- which is exactly what makes membership flips
(C3) fall out for free. "Ours" resolutions change nothing (the merged
map already holds ours). The resolve path then rebuilds the index AM
into the DTBL it already writes, instead of re-attaching the stale
one. Unique probes run against the merged index state (C4).

A note on the road not taken: 3-way-merging the index *maps* directly
(`prolly.MergeMaps`) looks attractive but is wrong -- field-level
document merging produces docs that exist on neither parent, whose
index entries exist in neither parent's index. Only the
diff-stream-driven approach indexes those (B2's neither-parent test
exists to catch exactly this).

## 4. Plan

Phases ship independently; each lists the behaviors it turns green.

- **Phase 0 -- red bar.** Land the W2, W3, T2, and T3 parity families
  as `DumboDBXFail` (divergence recorded side by side), and the B2 and
  B3 dumbodb-only scenarios as failing tests (chunk-reuse walker
  helper included). Nothing turns green; the gaps become visible in
  CI and in the parity report.
- **Phase E -- encoding soundness (Tier 1 of Section 2.7).** Fix
  non-integer float bucketing; reject lossy value types in the bounds
  builder and entry generation so plans fall back to scans.
  Standalone bugfix; can ship before or alongside Phase 1.
  Turns green: T1 (pinned), T2, T3, T4.
- **Phase 1 -- write correctness.** `indexEntriesForDoc` +
  update/delete maintenance, wired into UpdateAll and DeleteAll.
  Turns green: W2, W3, W4, W5, M1, M2, P2 (update/delete cases).
- **Phase 2 -- unique via probes.** Replace the insert scan; add
  update-path checks.
  Turns green: U2, U3, P1. Keeps green: U1.
- **Phase 3 -- merge and resolution.** Diff-driven index maintenance
  inside the collection merge, name-level reconciliation, unique
  artifacts, and the same maintenance wired into the conflict-resolve
  path.
  Turns green: B1-B6, C1-C5.
- **Phase 4 -- history ops.** Test-only phase confirming cherry-pick /
  rebase / revert inherit Phase 3.
  Turns green: B7.
- **Phase 5 -- metadata format cleanup (deferred).** Replace the JSON
  per-index metadata chunk with BSON (or fold into the collection
  metadata) and point the index address map at raw roots. No behavior
  changes; pure storage hygiene. Lowest priority.

Dependencies: Phase 2 needs Phase 1 (membership correctness before
probes) and Phase E (probe equality is only sound over faithful or
guarded encodings). Phase 3 needs Phase 1 (shared entry generation).
Phase 4 needs Phase 3. Phases 0, E, and 5 are independent of the
rest.

No dolt changes in any phase: everything builds on public dolt APIs
(`prolly.MutableMap`, `tree.ThreeWayDiffer`, `prolly.AddressMap`).
DumboDB stays on its current dolt branch untouched.

## 5. Decided Policies

No open questions remain. Decisions recorded so they are not
re-litigated:

- **Unique-violation conflicts reuse the document-conflict shape
  (B6).** The conflict entry carries the left and right document
  values of the three-way merge, which is sufficient to show the
  user what collided; there is no richer vocabulary available today.
  No distinct artifact type. Revisit only if user feedback on the
  existing data-conflict workflow demands it.
- **Drop wins (B5).** An index definition change beats an untouched
  side, silently; two competing definition changes are a merge
  error. Full case table in B5.
- **No backward compatibility.** Index data persisted before these
  changes is not detected or migrated. The membership fixes (M1/M2)
  and the float-bucketing fix (T2) change persisted index bytes;
  pre-existing indexes are simply invalid under the new code.

## 6. Appendix: Code Ground Truth (verified 2026-06-09)

Where each behavior's current status was established. Line refs are to
the rebased `dumbo-indexes` tree.

- Storage is BSON-at-rest: value tuple is `[0x01][raw BSON]` in a
  `BytesAdaptiveEnc` field (`helpers.go:55`, `bson_codec.go:30-36`);
  docs decode straight from the tuple via `readDocFromValue`
  (`collection.go:2688`). Primary key is salt-free
  `SHA512(canonical-BSON(_id))[:20]` (`hashID`, `helpers.go:227`) --
  this is why B1 holds without new work.
- Index entry layout: `index/index.go:29-101` (descriptors, key
  builder, InsertEntry/DeleteEntry); KeyString encoding in
  `index/keystring.go`. Lookups via `IterKeyRange`
  (`index/index.go:106-260`).
- Branch-scoped resolution (B8) landed as `index_resolve.go`: pure
  read resolvers (`resolveBranchIndexState` `:237`,
  `resolveCollIndexAM` `:180`) and pure write helpers
  (`applyInsertsToIndexes` `:257`, `buildIndexAM` `:302`). InsertAll
  composes them (`collection.go:1297`, `:1468-1475`).
- W2/W3 gaps: TODO comments at `collection.go:1717` (UpdateAll) and
  `:1880` (DeleteAll); both preserve the existing index AM untouched.
  UpdateAll already holds the old doc bytes (`existingTup`,
  `:1656-1666`) needed for entry-set diffing.
- M1/M2 gap: `buildSecondaryIndex` (`collection.go:2468`) and
  `applyInsertsToIndexes` index every doc; missing fields become Null
  entries (`extractIndexFieldValues`, `collection.go:2539`).
  Membership checks exist only in unique validation
  (`collection.go:1386-1422`) and some reads (`:2162`).
- U1 scan: `collection.go:1311-1342` iterates and decodes the whole
  primary per insert batch when any unique index exists.
- T2/T3 gaps: `EncodeValue` (`index/keystring.go:74-136`) routes all
  non-integer floats to the largest-magnitude buckets
  (`encodeFloat64`, `:241-272` -- ctypePosLarge/ctypeNegLarge
  regardless of value) and its default case encodes Decimal128 /
  Timestamp / Regex as Null; documents and nested arrays are
  marker-byte-only (`:118-123`). NaN and +/-Inf also map to Null
  (`:243`). `indexBoundsForFilterValue` (`collection.go`) rejects
  only nil/Null/Array scalars and Document/Array/Null/Regex operator
  operands -- the lossy types above pass through.
  `tryIndexedCount` (`collection.go:1979`) counts raw entries in the
  computed range with no re-validation.
- B2-B6 gap: `mergeAddressMapsWithConflicts` (`merge_conflict.go:716`)
  merges primaries via a `tree.ThreeWayDiffer` walk in
  `captureConflictsForCollection` (`:559`) -- seeded from the FROM
  side, into-side edits applied on top, field-level resolution via
  `mergeBSONDoc` (`:563-594`) -- then explicitly re-attaches the
  into-branch's index AM (`:811-818`). The differ loop is the natural
  driver for per-index edits (Section 3).
- B7 callers: merge `backend.go:1758`, cherry-pick `:2035` and
  `:3065`, revert `:3374`.
- C1-C5 gap: `DumboDBResolveConflict` (`merge_conflict.go:240`) puts
  the chosen value (or deletes) directly on the merged collection map
  and re-attaches the existing index AM via `indexAMFromAM` before
  `dtblHashForCollection` -- no index maintenance, no unique
  validation, on the resolve path.
- Phase 5 target: JSON metadata chunk written by
  `writeIndexEntryChunk` (`index_persist.go:148-159`); also the
  `docToExtJSON` -> `docToBSON` rename (`collection.go:2692`).

### Decisions revisited from the 2026-05-20 draft

1. Upstream dolt changes (a `SecondaryKeyBuilder` interface, lifting
   `MutableSecondaryIdx`) -- dropped; dumbodb stays on its pinned dolt
   branch and the glue is small enough to own.
2. `prolly.MergeMaps` for index merge -- dropped for correctness (see
   Section 3); diff-driven instead, which is also what dolt itself
   does for secondary indexes.
3. Stateful per-index writer objects -- replaced by the pure
   resolve/apply/build pattern the codebase adopted in the
   branch-scoped metadata refactor.
4. The old JSONAddr document-chunk assumptions -- superseded by
   BSON-at-rest; doc decoding for index maintenance is cheaper than
   the v1 design assumed.
