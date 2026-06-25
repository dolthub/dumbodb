# Resolve Index Metadata Per-Read; Drop the dbState Caches

**Issue:** workspace-cct
**Parent epic:** workspace-i0u (Refactor version-controlled metadata caches
off dbState)
**Sequenced after:** docs/design/working-set-session-ownership.md (already
landed: commits 0231dea ... 5d632c3)
**Sequenced before:** docs/design/secondary-index-structural-sharing.md
**Status:** Draft
**Date:** 2026-05-27

---

## 1. Problem

DumboDB stores secondary index metadata in three caches hung off `dbState`:

```go
// internal/backends/dolt/backend.go:128-162
type dbState struct {
    ...
    indexes      map[string][]backends.IndexInfo            // collName -> infos
    secIndexMaps map[string]map[string]prolly.Map           // collName -> idxName -> map
    collIndexAMs map[string]prolly.AddressMap               // collName -> AM
    ...
    mu sync.RWMutex
}
```

Three problems compound here:

1. **No branch dimension.** The on-disk truth is per-branch -- each DTBL
   inlines its own `secondary_indexes` AM
   (`helpers.go:184-223 buildDoltTableFlatbuffer`). The cache collapses
   every branch into one. Creating an index on `feat` overwrites `main`'s
   view in memory, and the next persistence on `main` inlines `feat`'s AM
   into `main`'s DTBL -- actual on-disk corruption.

2. **`state.mu` is a read bottleneck.** Every reader of the caches takes
   `state.mu.RLock()` (`collection.go:889, 1138, 1305, 2039, 2188, 2383,
   2400, ...`). Index lookups are on the hot read path. Concurrent reads
   on a busy collection serialise through one mutex even though the
   underlying data is immutable.

3. **Eager hydration.** `hydrateAllIndexes` (`index_persist.go:349`) runs
   once at db open and walks the default branch's entire collections AM,
   chunk-reading every collection's DTBL whether anyone will ever use
   them. This is the wrong default: it slows db-open in proportion to
   collection count, biases towards the default branch, and was added
   without measurement -- there is no benchmark establishing that the
   alternative (load on demand) is slower in any workload that matters.

The first two problems get worse together. A more granular cache (per
(branch, collection)) fixes correctness but multiplies the lock-contention
problem. The right move is to **stop caching mutable per-database state
entirely** and replace it with content-addressed memoization that is
naturally lock-free.

## 2. Scope

In:

- `state.indexes`
- `state.secIndexMaps`
- `state.collIndexAMs`
- `hydrateAllIndexes` (delete; no replacement)

Out (deferred to workspace-i0u's remaining sub-tasks; same shape, separate
PRs):

- `state.uuids`
- `state.validators`
- `state.capped`
- `state.insertionOrder`
- `state.views`
- `state.timeSeries`

These have the same family of bugs but live on different write paths.
Bundling them into one PR risks a merge nightmare with the in-flight
structural-sharing work. This design is the template; workspace-i0u opens
follow-ups that apply the template to each.

Stays where it is:

- `state.collSchemaHash` -- the DSCH chunk holds `(_id BINARY(20), doc
  JSON)`, identical across every collection in every branch of a
  database. Genuinely per-database.
- `state.emptyIndexAM` -- a flyweight empty `AddressMap` pinned to the
  NodeStore for writes that have no secondary indexes. Genuinely
  per-database. (We may not need it after the refactor; revisit in 6.4.)

## 3. Target Model

Three principles, in priority order:

1. **No mutable cache of per-database state.** The truth is on disk in
   the DTBL chain, and the chunk store already caches chunks. Read paths
   resolve from the session's working root every time.
2. **Memoize only immutable, content-addressed work.** Decoded
   `IndexInfo` for a given IndexEntry hash never changes. A
   process-wide `sync.Map` keyed by hash gives us lock-free reads and
   automatic deduplication across branches and databases that happen to
   share an index.
3. **No eager hydration.** Load on demand. The chunks aren't going
   anywhere.

### 3.1 What "the truth on disk" looks like

For any (session, branch, collection):

```
session WorkingRoot (via session.LookupDbState)
  |
  RTVL.tables -> ADRM
     "myColl" -> DTBL hash
        |
        Read DTBL chunk
        |
        DTBL.secondary_indexes (bytes inline) -> AddressMap node
           |
           Walk AM (sorted by index name)
             "idx_age" -> IndexEntry hash
                |
                Read IndexEntry chunk
                Decode JSON -> { metadata, map_root hash }
                   |
                   Read map_root chunk
                   -> prolly.Map node
                   -> prolly.Map handle
```

Every arrow except the working-root lookup is a chunk read. Chunks live
in the NBS chunk store, which has its own LRU cache. The per-arrow cost
on a hot chunk is microseconds; on a cold chunk it's an NBS read.

### 3.2 What we memoize and what we don't

| Layer | Memoize? | Why |
|---|---|---|
| `WorkingRoot.HashOf() -> ADRM` | No | Session-scoped, already routed via `workingRootViaSession` (`session_reads.go:89`). |
| `ADRM["coll"] -> DTBL hash` | No | One `AddressMap.Get`; the AM itself is cached in chunk store. |
| DTBL hash -> `secondary_indexes` AM | No | Parsing a DTBL flatbuffer is cheap; bytes inline. |
| `secondary_indexes AM["idx"] -> entryHash` | No | One `AddressMap.Get`. |
| `entryHash -> IndexInfo + mapRoot` | **Yes** | JSON decode is the most expensive step per index and is purely a function of `entryHash`. |
| `mapRoot -> prolly.Map handle` | **Optionally** | Building the handle is cheap (a few field assignments); skipping the memo here is fine. Measure before adding. |

The memoization is a single field with three lines of code:

```go
// internal/backends/dolt/index_resolve.go (new file)

// indexEntryMemo caches the decoded IndexInfo for a given IndexEntry
// chunk hash. Hashes are immutable and content-addressed; the same
// hash from any branch or database resolves to the same metadata,
// so this is safe as a process-wide cache with no invalidation.
var indexEntryMemo sync.Map // hash.Hash -> *resolvedIndexEntry

type resolvedIndexEntry struct {
    info    backends.IndexInfo
    mapRoot hash.Hash
}
```

`sync.Map` is right here because the access pattern is "load mostly,
store rarely, never delete." No mutex on the read path.

### 3.3 Resolver API

```go
// All four functions take the session-resolved working root the caller
// already has in scope. None of them touch state.mu or any dbState
// metadata field.

// dtblHashForColl returns the DTBL hash for collName under rv, or
// zero-hash if the collection doesn't exist.
func dtblHashForColl(ctx context.Context, ns tree.NodeStore,
    rv doltdb.RootValue, collName string) (hash.Hash, error)

// indexAMForDTBL returns the secondary_indexes AddressMap inlined in
// dtbl. Empty AM if the collection has no secondary indexes.
func indexAMForDTBL(ctx context.Context, ns tree.NodeStore,
    cs chunks.ChunkStore, dtblHash hash.Hash) (prolly.AddressMap, error)

// resolveIndexEntry decodes the IndexEntry chunk at entryHash, using
// the process-wide memo. The returned pointer must not be mutated.
func resolveIndexEntry(ctx context.Context, ns tree.NodeStore,
    entryHash hash.Hash) (*resolvedIndexEntry, error)

// openIndexMap returns a prolly.Map handle for the secondary index
// stored at mapRoot.
func openIndexMap(ctx context.Context, ns tree.NodeStore,
    vs *dolttypes.ValueStore, mapRoot hash.Hash) (prolly.Map, error)
```

A typical read path becomes:

```go
// before: state.mu.RLock(); idxs := state.indexes[c.name]; state.mu.RUnlock()
rv, err := workingRootViaSession(ctx, sess, ws, c.db.name, c.db.rootish)
dtblH, err := dtblHashForColl(ctx, state.ns, rv, c.name)
am, err := indexAMForDTBL(ctx, state.ns, state.cs, dtblH)
// iterate am, resolveIndexEntry per name
```

No locks. The bottleneck is gone.

### 3.4 What happens on writes

Writes are unchanged in shape: build the new prolly.Map for each index in
memory, write its root chunk, write a new IndexEntry chunk pairing
metadata with the root, build a new `secondary_indexes` AM, fold it into
a new DTBL, update ADRM. The current `persistIndexes`
(`index_persist.go:199-246`) does this end-to-end.

The change: it no longer reads or writes `dbState` fields. Its inputs
become explicit -- a list of `(IndexInfo, prolly.Map)` pairs -- and its
output is the new AM. The caller (insert / update / create-index)
already has those pairs in scope because it just produced them.

No write path needs to hold a long-lived in-memory AM. Each operation
constructs the AM fresh from N small index records (N is small -- a
collection typically has < 10 secondary indexes), serialises it, inlines
into the new DTBL, done.

This also drops a subtle correctness hazard: today `collIndexAMs[c.name]`
is the same value across branches, so if two operations on different
branches race, one inlines the other's AM. After the refactor there is
no shared AM to race on.

### 3.5 No hydration

`hydrateAllIndexes` and the call to it at `backend.go:1114` are
**deleted**. The first read that needs index metadata for a given
collection resolves it from the session's working root via the chain in
3.1 / 3.3. The chunk-store LRU handles repeat-access locality.

This is a deliberate revert of an optimization that had no measurement
backing it. If a real workload appears where on-demand resolution costs
too much (e.g. listing 10000 collections via `listCollections` triggers
10000 DTBL reads), the right response is to memoize *that specific path*
in *that workload's measurement*, not to eagerly walk the default branch
at db-open. The new memo layer (3.2) already covers the most expensive
step (JSON decode).

## 4. Mapping

Site-by-site translation.

### 4.1 Reads (no lock taken)

| Site | Today | After |
|---|---|---|
| `collection.go:889,890,891` (`tryIndexLookup`) | `state.mu.RLock(); state.secIndexMaps[c.name] / state.indexes[c.name]` | resolve via 3.3 chain from session WorkingRoot |
| `collection.go:1138-1141` (`Query` planner) | same | same |
| `collection.go:1305-1307` (`UpdateAll` index iter) | same | same |
| `collection.go:1523-1528` (`updateSecondaryIndexesOnInsert` resolver) | same | same |
| `collection.go:2039-2041` (`tryIndexedCount`) | same | same |
| `collection.go:2188-2191` (`DistinctScan`) | same | same |
| `collection.go:2383-2402` (`ListIndexes`) | same | same |

Every one of these reads is in a `state.mu.RLock()` region today. After
the refactor: no lock. The chunk store handles its own concurrency.

### 4.2 Writes

| Site | Today | After |
|---|---|---|
| `collection.go:2428-2452` (`CreateIndexes`) | `state.mu.Lock(); state.indexes[c.name] = ...; build maps; state.secIndexMaps[c.name] = ...; persistIndexes; updateAddressMap` | resolve current indexes via 3.3; locally build new info list + maps; call refactored `buildIndexAM`; fold into DTBL; updateAddressMap |
| `collection.go:2669-2678` (`DropIndexes`) | `state.mu.Lock(); state.indexes[c.name] = kept; persistIndexes` | same shape: resolve current, drop the named indexes locally, rebuild AM, write DTBL |
| `index_persist.go:199-246` (`persistIndexes`) | reads dbState, writes `state.collIndexAMs` | refactored to `buildIndexAM(ctx, ns, vs, idxs)`: pure function that takes a list of `(IndexInfo, prolly.Map)` and returns a new `prolly.AddressMap`. No side effects. |
| `index_persist.go:256-344` (`loadIndexesFromDTBL`) | populates `state.*` directly | refactored to return `([]IndexInfo, map[string]prolly.Map, prolly.AddressMap, error)`; no side effects. Used by tests and the merge path that needs the full set for a branch. |
| `helpers.go:316-321` (`dtblHashForCollection`) | reads `state.collIndexAMs[collName]` | takes the AM as a parameter; callers pass the one they just built |

Concurrent writes on the same (branch, collection) are still serialised
by the existing session/working-set mechanism: each session has its own
working root, and dsess's `Provider().TxLocks()` keymutex on
`dbname + " " + workingSetRef` (`dsess/transactions.go:418`) serialises
the commit. Removing `state.mu` from the write path does not loosen any
existing concurrency guarantee.

### 4.3 Hydration

| Site | Today | After |
|---|---|---|
| `backend.go:1114` (`db.hydrateAllIndexes(ctx)`) | walks default-branch AM at db-open, populates dbState caches for every collection | **deleted** |
| `index_persist.go:346-358` (`hydrateAllIndexes`) | reads default branch's working set | **deleted** |

### 4.4 dbState fields

| Field | Disposition |
|---|---|
| `state.indexes` | deleted |
| `state.secIndexMaps` | deleted |
| `state.collIndexAMs` | deleted |
| `state.emptyIndexAM` | retained for now (6.4 may delete in a follow-up) |
| `state.collSchemaHash` | retained (genuinely per-database) |

After this, `state.mu` is not taken on the index read path at all. We
should audit what *else* it protects and whether it can be narrowed or
deleted entirely, but that's a separate exercise (see 6.5).

## 5. Test Plan

Branches are a dumbodb concept; mongodb has no equivalent, so the entire
test surface must live in dumbodb's Go-level tests. The plan below uses
the existing test idiom (`be.Database("name@branch")` for branch
pinning, `ctxWithSession` for lsid attachment, `DumboDBBranch` /
`DumboDBCommit` for version-control operations) so all of these compile
and run inside `internal/backends/dolt/`.

Every test is structured to assert per-branch behaviour against
per-branch state. There are no "list of indexes" assertions that
omit the branch dimension.

### 5.1 Fixtures and conventions

A new test helper module `internal/backends/dolt/index_branch_testutil.go`
provides:

```go
// branchPin returns a backend.Database handle for (dbName, branch) and
// a context with a session pinned to (lsid). Sessions are distinct per
// branch by default; pass sameLsid=true to share one session across
// pins (used by within-session-branch-switch tests).
func branchPin(t *testing.T, be *Backend, dbName, branch, lsid string) (backends.Database, context.Context)

// commit, branch, checkout: thin wrappers over DumboDBCommit /
// DumboDBBranch that fail the test on error and return the new commit
// hash.
func commit(t *testing.T, be *Backend, ctx context.Context, dbName, msg string) string
func branchFrom(t *testing.T, be *Backend, ctx context.Context, dbName, from, name string)

// equalityLookup runs the same code path as the handler's equality
// filter but returns the matched _id list, so tests can assert on
// set equality without per-test BSON wiring.
func equalityLookup(t *testing.T, ctx context.Context, db backends.Database, coll, field string, value any) []any
```

`equalityLookup` is the workhorse: every "what does this branch see"
assertion calls it. It internally drives the same `tryIndexLookup` path
the production query layer uses, so the tests cover the actual index
read path -- not a shadow API.

### 5.2 Cross-branch isolation (red bar today)

Each of these is failing on commit `5d632c3` for the reason cited and
passes after this design lands.

#### TestIndexCreated_OnFeat_NotVisibleOnMain

1. Open db. Insert `{_id: 1, age: 30}` on `main`. Commit.
2. `branchFrom(main, feat)`.
3. Pin to `feat`: `createIndex(coll, "by_age", {age:1})`. Commit.
4. Pin to `main` (different lsid): `listIndexes(coll)`.
5. Assert: result is exactly `["_id_"]`.
6. Pin to `feat`: `listIndexes(coll)`.
7. Assert: result is exactly `["_id_", "by_age"]`.

Today: step 5 returns `["_id_", "by_age"]` because `state.indexes` is
unbranched.

#### TestIndexCreated_AfterReopen_VisibleOnOriginatingBranch

1. Open db. Branch `feat`. Pin to `feat`. Create `by_age`. Insert
   `{_id: 1, age: 30}`. Commit. Close backend.
2. Reopen backend. Pin to `feat`. `listIndexes`.
3. Assert: `["_id_", "by_age"]` and `equalityLookup("age", 30)` returns
   `[1]`.

Today: `hydrateAllIndexes` walks only `main`'s working root at open;
`feat`'s indexes aren't in the cache and no read path materialises
them.

#### TestIndexLookup_OnEachBranch_SeesOnlyOwnData

This is the canonical "interleaved writes diverge" test. It is what the
user described.

1. Create coll on `main`. Create `by_first_letter` index on field `name`.
   Insert `{_id: 1, name: "alpha"}`. Commit.
2. `branchFrom(main, am)`. `branchFrom(main, nz)`.
3. Pin to `am`: insert `{_id: i, name: w}` for `i = 100..112` and
   `w in {"bravo","charlie","delta","echo","foxtrot","golf",
   "hotel","india","juliet","kilo","lima","mike"}`. Commit.
4. Pin to `nz`: insert `{_id: i, name: w}` for `i = 200..212` and
   `w in {"november","oscar","papa","quebec","romeo","sierra",
   "tango","uniform","victor","whiskey","xray","yankee","zulu"}`.
   Commit.
5. Pin to `am`: `equalityLookup("name", "mike")`. Assert `[111]`.
6. Pin to `am`: `equalityLookup("name", "zulu")`. Assert `[]`.
7. Pin to `nz`: `equalityLookup("name", "zulu")`. Assert `[212]`.
8. Pin to `nz`: `equalityLookup("name", "mike")`. Assert `[]`.
9. Pin to `main`: `equalityLookup("name", "alpha")`. Assert `[1]`.
10. Pin to `main`: `equalityLookup("name", "mike")`. Assert `[]`.
11. Pin to `main`: `equalityLookup("name", "zulu")`. Assert `[]`.

Today: at least one of steps 5-11 returns the wrong set because
`state.secIndexMaps["coll"]["by_first_letter"]` is whichever map was
written last.

#### TestWrite_OnFeat_DoesNotCorruptMainDTBL

1. Pin to `main`: create `by_age`, insert one doc. Commit.
2. Branch `feat`. Pin to `feat`: create `by_name` (different index!),
   insert one doc. Commit.
3. Pin to `main`: insert another doc on `main` (forces a DTBL rewrite).
   Commit.
4. Read `main`'s DTBL chunk directly via `state.cs.Get`; parse with
   `serial.TryGetRootAsTable` and walk the `secondary_indexes` AM.
5. Assert the AM contains exactly one entry, named `by_age`.

Today: step 5 fails because step 3's DTBL write inlines
`state.collIndexAMs["coll"]`, which has been overwritten with `feat`'s
AM holding `by_name`. The on-disk DTBL ends up with the wrong indexes.

#### TestIndexes_Within_SameSession_BranchSwitch

1. Pin one lsid to `main`. Create `by_age`. Insert one doc. Commit.
2. Same lsid: switch the `Database` handle to `coll@feat` via a fresh
   `branchFrom(main, feat)`.
3. Pin same lsid to `feat`. Drop `by_age`. Create `by_name`. Insert one
   doc. Commit.
4. Same lsid: get a `coll@main` handle again. `listIndexes`.
5. Assert: `["_id_", "by_age"]`.
6. Same lsid: get `coll@feat` handle. `listIndexes`.
7. Assert: `["_id_", "by_name"]`.

Today: fails for the same reason as the cross-session case, but also
exercises the "session-resolved working root" path explicitly to
prove single-session correctness.

### 5.3 Interleaved writes with incremental updates

The user's explicit ask: incremental add/update/delete on each branch
must reflect in that branch's index lookups. These tests interleave
write operations across branches, asserting after every step.

#### TestInterleaved_Inserts_OnDisjointBranches

A finer-grained version of TestIndexLookup_OnEachBranch_SeesOnlyOwnData:
after each insert, the indexed lookup on the writing branch sees the
new doc and the other branch does not.

1. Set up: `main` has `by_age` and `{_id:1, age:30}`. Commit. Branch
   `feat` and `dev` off `main`.
2. Loop 50 times: alternately insert a doc with a unique age on
   `feat` (ages 100, 102, 104, ...) and on `dev` (ages 101, 103, 105,
   ...).
3. After every loop iteration, on the branch that just wrote, the new
   age looks up correctly and returns exactly the doc just inserted.
   On the *other* branch, that age returns the empty set.
4. After the full loop, on `main`, `equalityLookup("age", 100)` through
   `("age", 199)` all return empty. `equalityLookup("age", 30)` returns
   `[1]`.
5. On `feat`, ages 100 and every even-100s value return their single
   doc; ages 101 and every odd-100s value return empty.
6. Symmetric on `dev`.

#### TestInterleaved_Updates_ChangeIndexedField

1. `main` has `by_age` and ten docs aged 30..39. Commit. Branch `feat`.
2. Pin to `feat`: update `{_id: 5}` to set `age: 99`.
3. Pin to `feat`: `equalityLookup("age", 35)`. Assert `[]` (was the
   pre-update value).
4. Pin to `feat`: `equalityLookup("age", 99)`. Assert `[5]`.
5. Pin to `main`: `equalityLookup("age", 35)`. Assert `[5]` (`main`
   still sees the unchanged doc).
6. Pin to `main`: `equalityLookup("age", 99)`. Assert `[]`.

Note: this also depends on `UpdateAll` actually maintaining secondary
indexes, which is the workspace-4ee work. Until that lands, this test
exercises the read side only; the update side is asserted as an
xfail-style placeholder with the comment `pending workspace-4ee`. Once
4ee lands, remove the placeholder and the test goes green.

#### TestInterleaved_Deletes_RemoveFromIndex

1. `main` has `by_age` and ten docs aged 30..39. Commit. Branch `feat`.
2. Pin to `feat`: delete `{_id: 5}`.
3. Pin to `feat`: `equalityLookup("age", 35)`. Assert `[]`.
4. Pin to `main`: `equalityLookup("age", 35)`. Assert `[5]`.

Same workspace-4ee dependency as above for the actual delete-side
correctness; the read-side test passes once we resolve correctly.

#### TestInterleaved_CreateThenDropIndex_PerBranch

1. `main` has coll with `_id_` and ten docs. Commit. Branch `feat`.
2. Pin to `feat`: create `by_age`. `equalityLookup("age", 35)` returns
   `[5]` via the index (assert the planner picked the index, e.g. via
   the same `tryIndexedCount` probe the production code uses).
3. Pin to `main`: `listIndexes`. Assert `["_id_"]`.
4. Pin to `feat`: drop `by_age`. `listIndexes` returns `["_id_"]`.
5. Pin to `main`: `listIndexes` still `["_id_"]`.

#### TestInterleaved_SameNameDifferentDefinition

Per the design rule that indexes are immutable by name
(`secondary-index-structural-sharing.md` section 3.5):

1. `main` has coll. Branch `feat` immediately.
2. Pin to `feat`: `createIndex(coll, "by_x", {x:1})`.
3. Pin to `main`: try to `createIndex(coll, "by_x", {x:1, y:1})`.
4. Assert: this is permitted (different branch, different definition).
5. Pin to `feat`: `listIndexes` shows `by_x` with key `{x:1}`.
6. Pin to `main`: `listIndexes` shows `by_x` with key `{x:1, y:1}`.
7. Insert docs on both sides; assert each branch's `by_x` lookups
   reflect that branch's definition.

This nails down the case `secondary-index-structural-sharing.md`
calls out as "two branches can never present the same index name with
different specs -- if specs differ they are independent indexes."

### 5.4 Persistence and reopen

#### TestReopen_NoHydration_FirstReadResolves

Direct test of the "no eager hydration" decision.

1. Open db. Create `main` collection with `by_age`. Branch `feat` with
   `by_name`. Commit both. Close backend.
2. Reopen backend. Instrument: add a counter that increments on every
   call to `loadIndexesFromDTBL` (or the new resolver function it
   refactors into).
3. After reopen, before any client read, assert the counter is `0`
   (no eager hydration).
4. Pin to `feat`, do one `listIndexes`. Assert the counter is now `1`
   (the `feat` DTBL was resolved on demand).
5. Same pin, `listIndexes` again. Assert the counter is still `1` (the
   chunk store / memo answered from cache).
6. Pin to `main`, `listIndexes`. Assert the counter is now `2`.

Today (with `hydrateAllIndexes`): step 3 fails (counter is non-zero
because `main` was eagerly hydrated). Steps 4-6 would also reveal that
`feat` was never hydrated.

#### TestReopen_LargeDatabase_StartupCostFlat

Workload benchmark, gated behind `-short`:

1. Create 1000 collections, each with 5 secondary indexes, with 10 docs
   each. Commit. Close.
2. Reopen and time the open-to-first-ready interval.
3. Assert: time is independent of collection count (scales as O(1) of
   metadata reads, dominated by NBS open). Before this design lands,
   open scales as O(collections * indexes).

### 5.5 Concurrency

#### TestConcurrent_ReadsOnSameBranch_DoNotContend

Proves `state.mu` is off the index read path.

1. Set up coll with 2 indexes, 1000 docs.
2. Spawn N goroutines, each doing 10000 `equalityLookup` calls on
   distinct values.
3. Benchmark N=1, N=4, N=16, N=64. Per-op latency must be effectively
   flat as N grows.

This is a permanent regression bar in the bench suite. A future PR
that puts a mutex back on the read path fails it.

#### TestConcurrent_WritesOnDifferentBranches_DoNotBlock

1. Branch `a` and `b` off `main`. Each branch has its own `by_age`.
2. Two goroutines: one writes 1000 docs on `a`, the other writes 1000
   docs on `b`, both in parallel.
3. Assert: total wall time is no more than 1.5x the time of a single
   1000-write run. (Today, both contend on `state.mu`; after the
   refactor they don't share any lock that the index path holds.)

#### TestConcurrent_ReadOnA_WriteOnB

1. `a` has 1000 docs and `by_age`. `b` branched off `a`, no writes yet.
2. Two goroutines:
   - Goroutine A: 10000 lookups on `a` of values known to be present.
     Every lookup must return the correct set (a permanent stable view
     because no one is writing to `a`).
   - Goroutine B: 1000 inserts on `b`.
3. Assert: every read on `a` returns the same set every time; no read
   sees `b`'s data.

This catches any accidental sharing between branches at the
prolly.Map handle level.

### 5.6 Memo behaviour

#### TestMemo_DedupesAcrossBranches

1. Branch `feat` off `main` with no other changes. Both branches
   start with identical DTBLs and identical index AMs.
2. Pin to `main`: `listIndexes` (forces `_id_`'s IndexEntry to memo).
3. Pin to `feat`: `listIndexes`.
4. Assert: the memo size grew by 0 between steps 2 and 3 (the
   IndexEntry hash was identical, so the memo answered from cache).

Implementation: instrument `resolveIndexEntry` with a miss counter
exposed for tests only (build tag `dumbo_test_internal`).

#### TestMemo_DistinctEntriesForDistinctDefinitions

1. `main` creates `by_x` with `{x:1}`. Memo gains one entry.
2. `feat` (branched then redefined as in 5.3
   TestInterleaved_SameNameDifferentDefinition) creates `by_x` with
   `{x:1, y:1}`. Memo gains a second entry.
3. Reads on both branches resolve to their respective entries.

#### TestMemo_SurvivesGCOfUnrelatedChunks

Memo is process-wide, not tied to a specific chunk store generation.
This test makes the explicit guarantee:

1. Open db, populate, read once to memoize.
2. Trigger a GC cycle on the chunk store. (Use the existing
   `gcctx`-driven test hook from `working-set-session-ownership.md`
   if available; otherwise force a chunk-store reopen.)
3. Read again on the same branch; assert the memo answered without a
   chunk fetch.

### 5.7 Existing tests must keep passing unchanged

Per the strict rule from `working-set-session-ownership.md`:

- `index_test.go`
- `index_partial_test.go`
- `index_persist_test.go`
- `index_bench_test.go`
- `prefilter_range_test.go`
- `partial_update_test.go`
- `persist_test.go`
- `diff_test.go`
- `reset_test.go`
- `rtvl_load_test.go`
- `session_*_test.go`

These exercise the default-branch single-branch path, which is where
the bug doesn't fire. If any of them needs modification to pass after
the refactor, that is a signal the refactor broke a contract -- stop
and reconsider, do not edit the test.

### 5.8 Wire-level verify doc and matching test

In addition to the Go-level tests above, this design ships with a
human-runnable verify doc plus its automated equivalent:

- **Doc:** `docs/verify/index-branch-isolation.md`. Seven scenarios a
  reviewer runs by hand in `mongosh` against a live DumboDB instance.
  Each step shows the expected output. The scenarios cover the
  full lifecycle: create index per branch, interleaved insert /
  update / delete on different branches, drop index per branch,
  server restart preserving per-branch state.
- **Test:** `tests/verify/index_branch_isolation_test.go`
  (`TestIndexBranchIsolationVerify`). One sequential subtest per
  scenario, sharing one database the way the doc walks one database
  top-to-bottom. Uses the existing `startDumboDB` / `dumboDBCommit`
  test helpers in `tests/` and the `mongo-driver/v2` Go client, the
  same pattern as `verify/branch_test.go`.

This pair sits alongside the Go backend tests (5.2 through 5.7) rather
than replacing them: the backend tests cover internal invariants (memo
behaviour, lock contention, on-disk DTBL bytes) that the wire surface
cannot see; the verify pair covers the user-visible contract a
DumboDB operator can validate against any build.

The "what mongodb cannot test" caveat does not apply here. DumboDB
extends the mongo wire protocol with the `dbname@branch` pinning
convention, so a `mongo-driver` client can exercise branches against
DumboDB even though there is no equivalent against MongoDB. The verify
doc is a DumboDB-specific behaviour test, not a parity comparison.

### 5.9 What this plan deliberately does not test

- **End-to-end mongo wire protocol.** MongoDB has no branches; a
  mongo-driver-level test cannot exercise the bug. The plan above is
  entirely at the Go backend layer because that is where the branch
  dimension is observable.
- **Merge correctness for secondary indexes.** That is the
  workspace-ife scope, blocked on this design. Tests there will build
  on the same `branchPin` and `equalityLookup` helpers.
- **Structural sharing of index chunks across branches.** That is
  `secondary-index-structural-sharing.md` (workspace-4ee /
  workspace-ife). Tests there will assert chunk-address reuse; this
  design only needs correctness of per-branch resolution.

## 6. Resolved Decisions

### 6.1 No memo eviction

`indexEntryMemo` is a `sync.Map[hash.Hash, *resolvedIndexEntry]`. The
key is content-addressed; entries are never wrong. The only growth
driver is the number of *distinct* IndexEntry hashes the process sees
over its lifetime, bounded by (collections) x (indexes per collection)
x (distinct definitions over time). A live database with 10000
collections and 5 indexes each, churning through 10 redefinitions of
each, caps the memo at 500000 entries of a few hundred bytes. No
eviction. If a real workload ever overruns this, the fix is a bounded
LRU swapped in behind the same accessor; no call-site change.

### 6.2 Do not memoize the prolly.Map handle

Constructing a `prolly.Map` handle from a root hash is a few field
assignments plus a chunk read of the root node, already cached by
the chunk store. A secondary memo `sync.Map[hash.Hash, prolly.Map]`
would shave microseconds. Skip. Revisit only if profiling shows the
construction in the hot path.

### 6.3 Do not memoize the DTBL-to-AM step

The AM is parsed from inline bytes inside the already-cached DTBL
chunk; the parse is fast (flatbuffer access plus one prolly node
decode). Profile before adding. Same disposition as 6.2.

### 6.4 `emptyIndexAM` is not state

It does not need to live on `dbState`. The cleanest form is a
package-level helper that builds one on demand, with an optional
NodeStore-keyed memo if profiling later shows the rebuild cost
matters:

```go
// internal/backends/dolt/index_resolve.go

var emptyIndexAMCache sync.Map // tree.NodeStore -> prolly.AddressMap

func emptyIndexAM(ns tree.NodeStore) (prolly.AddressMap, error) {
    if v, ok := emptyIndexAMCache.Load(ns); ok {
        return v.(prolly.AddressMap), nil
    }
    am, err := prolly.NewEmptyAddressMap(ns)
    if err != nil {
        return prolly.AddressMap{}, err
    }
    actual, _ := emptyIndexAMCache.LoadOrStore(ns, am)
    return actual.(prolly.AddressMap), nil
}
```

`buildIndexAM` of an empty input list returns this. The field
`state.emptyIndexAM` is deleted along with the other index fields in
step 6 of the migration.

### 6.5 Stale memo entries are impossible

If `entryHash` is in the memo, the IndexEntry chunk it points at is in
the chunk store (the hash was discovered by walking a live DTBL). The
chunk's bytes are immutable. The decoded `IndexInfo` and `mapRoot` are
deterministic from those bytes. No stale entries can arise.

The only way the memo could be wrong is if `MatchesPartialFilter` --
the closure synthesised in `entryToIndexInfo` (`index_persist.go:144-148`)
-- captures global state that itself changes. It captures only the
decoded `*types.Document` for the filter expression and calls
`backends.MatchPartialFilter`, which is pure. The memo is safe.

## 7. Related Work: Other Branch-Scoped Fields on `dbState`

After this refactor, `state.mu` no longer guards index metadata. It
still guards the remaining branch-scoped fields on `dbState`
(`backend.go:128-162`): `workingSets`, `uuids`, `validators`, `capped`,
`insertionOrder`, `views`, `timeSeries`, `mergeState`. Every one of
those has the same cross-branch bug pattern as the index caches did,
and is slated for the same treatment under workspace-i0u. When the
last of them moves out, `state.mu` becomes a candidate for removal.
This design does not depend on that outcome; it only removes the
index-metadata callers of the lock.

## 8. Migration Order

Same green-at-every-step discipline as
`working-set-session-ownership.md` "Migration Order". Each step is a
separate bd issue with the prior as a dependency.

1. **Land the failing tests** (5.1). They sit red.

2. **Introduce `index_resolve.go`** with the four resolver functions
   (3.3) and the `indexEntryMemo` sync.Map. No call site uses them
   yet. Cover with unit tests at the resolver level.

3. **Convert one read site as a proof-of-concept.** `ListIndexes`
   (`collection.go:2383-2412`) is the smallest and most read-only.
   Switch it to resolve via 3.3. Drop the `state.mu.RLock()` in that
   path. Run the 5.3 suite -- everything passes. Run
   TestIndexCreatedOnFeatNotVisibleOnMain (5.1) -- it now passes.

4. **Convert the remaining read sites** (4.1). One PR per site or in
   small batches. Each PR keeps the dbState fields populated by the
   write path so untouched read sites stay correct. By the end:
   TestIndexLookupOnFeatUsesFeatData and TestIndexAMSurvivesBranchSwitch
   pass.

5. **Refactor writes to pure functions** (4.2).
   - `loadIndexesFromDTBL` -> pure: returns `([]IndexInfo, map[string]prolly.Map, prolly.AddressMap, error)`; no side effects.
   - `persistIndexes` -> `buildIndexAM`: pure; returns `prolly.AddressMap`.
   - `dtblHashForCollection` -> takes AM as parameter.
   - `CreateIndexes` / `DropIndexes`: resolve current state via 3.3,
     mutate locally, write back via `buildIndexAM` + DTBL write.

   This step no longer writes to `state.indexes` / `state.secIndexMaps`
   / `state.collIndexAMs`. TestWriteOnFeatDoesNotCorruptMainDTBL passes.

6. **Delete the dbState fields and `state.mu` from the index path.**
   Remove `dbState.indexes`, `dbState.secIndexMaps`,
   `dbState.collIndexAMs`. Audit `state.mu.RLock()` / `state.mu.Lock()`
   call sites that no longer protect anything; remove. Any site that
   still legitimately needs the lock stays.

7. **Delete `hydrateAllIndexes`** and the call at `backend.go:1114`.
   `loadIndexesFromDTBL` is now reached only via the resolver path.
   TestIndexCreatedOnFeatVisibleAfterReopen passes. Db-open is faster.

8. **Land the verify doc and its matching test** (5.8).
   `docs/verify/index-branch-isolation.md` and
   `tests/verify/index_branch_isolation_test.go` go in together. The
   doc walks `mongosh` scenarios end-to-end; the test exercises the
   same scenarios via `mongo-driver/v2` as sequential subtests. Both
   should be in the same PR so they cannot drift.

9. **Land 5.5 / 5.6** in CI. The concurrency benchmark
   (TestConcurrent_ReadsOnSameBranch_DoNotContend) is the lock that
   prevents a future regression from putting a mutex back on the
   read path.

Each step except step 1 keeps every existing test passing. Step 1 adds
the new tests in their failing state; subsequent steps flip them to
passing.

## 9. Relationship to Other Designs

- **`working-set-session-ownership.md`** established that the session's
  working root is the authoritative read source for branch-scoped
  data. This design rides on that: the resolver chain (3.1) starts at
  the session's working root and resolves the index data from there
  every time. No duplicate per-session cache, no duplicate per-branch
  cache.

- **`secondary-index-structural-sharing.md`** (epic workspace-6r7,
  closed; child issues workspace-4ee and workspace-ife open). That
  design assumed the existing `state.indexes` / `state.secIndexMaps` /
  `state.collIndexAMs` caches were correctly per-branch -- they are
  not. Its Phase 1 (UpdateAll/DeleteAll wiring) and Phase 3 (merge)
  cannot land until this design lands, because each touches every
  index call site and would otherwise re-cement the bug. The new
  resolver API (3.3) is the substrate those phases call into.

- **workspace-i0u (epic).** This is the first sub-issue. The template
  here -- "resolve from the session's working root, memoize only
  content-addressed work, no eager hydration, no shared mutex" --
  applies to `validators`, `capped`, `views`, `timeSeries`, `uuids`,
  and `insertionOrder`. Each gets its own design, but the shape is
  shared.
