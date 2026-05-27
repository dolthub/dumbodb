# Singleton Per-Branch WorkingSet Entries

**Status:** Design
**Date:** 2026-05-27
**Side quest of:** GC integration. Not strictly required to ship GC,
but `state.workingSets` is guarded by `state.mu`, which makes
`state.mu` a write chokepoint and forces the qsc.5 flusher to dance
around races that this redesign eliminates.

## Problem

`dbState.workingSets map[string]*doltdb.WorkingSet` is a per-database
cache of "the current WorkingSet for this branch" keyed by branch
name. It is guarded by `dbState.mu`, the same `RWMutex` that guards
every other mutable field on `dbState`: indexes, secIndexMaps,
validators, capped state, mergeState, etc.

Three consequences:

1. **`state.mu` is hot.** Every branch's WS read/write contends with
   every other branch's WS read/write, and with every metadata-cache
   update. Two writers on independent branches in the same database
   serialize.
2. **The map is mutable in shape.** Entries are inserted on first
   write to a new branch and deleted on branch delete. Lock-holding
   patterns must protect both the entry's pointer and the map's
   structure. Today both protections come from the single `state.mu`.
3. **The qsc.5 flusher races on the entries.** Sessions hold per-
   session WS snapshots that, when flushed in arbitrary order, can
   overwrite each other on disk. The qsc.5 fix gates session pushes
   to in-txn writes only; the flusher then becomes blind to j:false
   autoCommit writes, which the GC design doc carries as a wrinkle.

## Target Model

Replace `workingSets map[string]*doltdb.WorkingSet` with a singleton-
per-branch map. Three properties:

1. **Entries are permanent for server lifetime.** The first access to
   a branch lazily creates an entry. Subsequent accesses reuse the
   same entry pointer. Branch deletion does NOT remove the entry; it
   nils the entry's `ws` field. The entry pointer stays stable
   forever.
2. **The map RWMutex protects only the map structure.** Once an
   entry is in the map, reads of "give me the entry pointer for
   branch X" take only the map's RLock. The slow path (entry
   creation) is the only writer of the map structure.
3. **Each entry owns its own RWMutex for the `ws` field.** Reads of
   the WS pointer take entry.RLock; writes take entry.Lock. Updates
   to two different branches' WSes never contend.

```go
type dbState struct {
    mu sync.RWMutex   // continues to guard non-WS state (indexes,
                      // secIndexMaps, validators, mergeState, etc.)

    branchWSMu sync.RWMutex            // guards the map STRUCTURE only
    branchWS   map[string]*branchWS    // singleton entries, never removed
}

type branchWS struct {
    mu     sync.RWMutex
    ws     *doltdb.WorkingSet   // nil until first load; nil after branch delete
    wsHash hash.Hash             // on-disk hash of ws; the optimistic-lock
                                 // value for the next UpdateWorkingSet call.
                                 // Zero when ws is nil.
}
```

`wsHash` is load-bearing for two reasons:

1. **`(*doltdb.WorkingSet).HashOf()` only works for WSes loaded from
   disk.** A WS built in memory via
   `ws.WithWorkingRoot(rv).WithStagedRoot(rv)` has no chunk-store
   address yet; `HashOf()` returns an error. Today's
   `updateWorkingSet` helper sidesteps this by re-resolving from
   disk every write to get a fresh `prevHash`. With the hash
   tracked on the entry, the per-write resolve goes away.
2. **`ddb.UpdateWorkingSet` requires the optimistic-lock hash.** The
   call signature is `(ctx, wsRef, newWS, prevHash, meta, rsc)`;
   `prevHash` must match the on-disk WS hash or the update fails
   with `ErrOptimisticLockFailed`. The entry-tracked `wsHash` is
   that value.

Concurrently with this, drop autoCommit's interpretation of
`j:false`. The wire `writeConcern.j` field continues to be parsed
(other modes may still honor it -- see "j:false" below); in
autoCommit, the backend treats `SkipDurableSync=true` as `false` and
every write fsyncs. The deferred flusher goes away because there is
no unflushed state to drain.

## j:false

`j:false` stays in the wire protocol. What changes is how each
backend mode interprets it:

| Mode | j:true (default) | j:false |
|---|---|---|
| autoCommit | Every write fsyncs. | Treated as j:true. The latency promise is broken, but no client breakage. |
| Session-iso / explicit-tx | Writes accumulate on the session. Commit fsyncs. | Reserved for future work: skip the commit-time fsync. Today, parsed but unused at commit time. |

So j:false is not removed. It is silently ignored where it was
previously implemented incorrectly (autoCommit), and reserved for
future work where it could be implemented correctly (session-iso
commit). The wire decode path is unchanged.

## Why Singletons (Not RW-locked Map Entries with Deletion)

An earlier sketch had entries that could be inserted and deleted at
runtime, each with its own lock. That model has three problems the
singleton model avoids:

- **Branch-delete races.** A writer with a stale entry pointer could
  write to an entry that's about to be removed from the map. The
  result is an orphaned struct with no readers. Detecting the race
  needs a "deleted" tombstone or a re-check after acquiring the
  entry lock. Singletons eliminate this: the entry stays alive; the
  pointer is always valid; branch delete just nils the `ws` field.
- **Map-Lock for first-write-per-branch.** Entry creation requires
  `branchWSMu.Lock()`. With deletion in the picture, the lock has
  to handle insert/delete contention. With singletons, the only
  writer of the map structure is first-touch, which happens once per
  branch per server lifetime. After warmup, the map is effectively
  read-only; readers use `branchWSMu.RLock()` (or no lock if the
  table is a `sync.Map`).
- **GC visibility ambiguity.** A deleted-then-rewritten branch can
  have an orphaned entry holding a WS the world has moved on from.
  Singletons avoid this: deletion sets `ws = nil`, and a subsequent
  reuse of the branch name finds the existing entry and refills it.

## Helpers

Direct `state.workingSets[branch]` access goes away. Three helpers
become the entry points:

```go
// Returns the entry pointer for a branch. Lazily creates it on
// first call. The pointer is stable for server lifetime.
func (s *dbState) branchEntry(branch string) *branchWS

// Reads the current WS for a branch. Lazily loads from doltDB if
// the entry's ws is nil (cold first read OR branch re-created
// after deletion). The post-load entry has both ws and wsHash
// populated.
func (s *dbState) loadBranchWS(ctx context.Context, branch string)
    (*doltdb.WorkingSet, error)

// Atomic compute-and-swap, persisted to disk. Holds entry.mu.Lock()
// across the entire read-compute-write-fsync sequence:
//   1. Load ws + wsHash from entry (lazy-init if first touch).
//   2. fn computes newWS from current ws.
//   3. ddb.UpdateWorkingSet(wsRef, newWS, prevHash=entry.wsHash, ...).
//      Fails with ErrOptimisticLockFailed if disk moved out from
//      under us (e.g., another process committed). Surfaced as an
//      error; caller decides whether to retry.
//   4. On success: entry.ws = newWS; entry.wsHash = newOnDiskHash.
// Replaces the current `state.mu.Lock(); read workingSets[b]; build
// newWS; UpdateWorkingSet; workingSets[b] = newWS` pattern.
func (s *dbState) updateBranchWS(ctx context.Context, branch string,
    fn func(*doltdb.WorkingSet) (*doltdb.WorkingSet, error)) error
```

Branch deletion uses `branchEntry(branch).ws = nil; entry.wsHash =
hash.Hash{}` under the entry's write lock. No `deleteBranchWS`
helper; no map-level delete.

### Computing the post-write `wsHash`

`ddb.UpdateWorkingSet` does not return the new on-disk hash; it
writes the WS chunk via `WriteValue` (which has the hash internally)
and updates the StoreRoot AddressMap pointing at that chunk. To
refresh `entry.wsHash` post-write, two options:

- **(a) Re-resolve from disk.** After successful UpdateWorkingSet,
  call `ddb.ResolveWorkingSet(ctx, wsRef)` and `HashOf()` on the
  result. One extra in-memory round trip per write (the chunk is in
  NBS memtable, so no disk I/O). Simple and correct.
- **(b) Expose the new hash from `ddb.UpdateWorkingSet`.** Dolt-side
  change. Out of scope per the GC epic's "no dolt refactor"
  decision.

Pick (a). The memtable lookup is in the µs range; for the
write-heavy paths this is below noise.

## Lock Ordering

```
state.mu  ->  branchWSMu  ->  branchWS.mu
```

The map-level RWMutex is taken briefly only at entry creation. After
the entry is in the map, readers acquire just `branchWS.mu` (R or W).
`state.mu` is no longer held to protect WS access at all.

## Call-Site Audit

~17 sites in `backend.go` and `helpers.go` reference
`state.workingSets[branch]`. Mechanical conversion:

| Pattern today | After |
|---|---|
| `ws, ok := state.workingSets[branch]` (read) | `ws, err := state.loadBranchWS(ctx, branch)` |
| `state.workingSets[branch] = newWS` (write) | inside `updateBranchWS`'s `fn` callback |
| `state.mu.Lock()` + read + compute + write + `state.mu.Unlock()` | `state.updateBranchWS(ctx, branch, fn)` |
| `delete(state.workingSets, branch)` | `state.branchEntry(branch).mu.Lock(); entry.ws = nil; entry.mu.Unlock()` |
| `workingSets: map[...]*doltdb.WorkingSet{defaultBranch: mainWS}` (init) | initialize `branchWS` map with one pre-warmed entry for `defaultBranch` |
| `workingRootViaSession(..., state.workingSets[branch], ...)` | `workingRootViaSession(..., entry-read, ...)` |

The rule for each site: drop `state.mu` where it was held *solely*
for the WS access. Keep `state.mu` where it also guards metadata
caches; the WS access is interleaved using the entry lock.

## Deferred Flusher

Removed.

Today's flusher (workspace-qsc.5) exists to drain j:false writes that
accumulate in memory. With autoCommit treating j:false as j:true,
every autoCommit write is durable inline. In-txn writes live on the
session and flush at commit via `dsess.CommitWorkingSet` /
`DoltCommit`. Nothing accumulates that needs a background drain.

Delete:
- `Backend.deferredFlushLoop`
- `Backend.flushAllDirty`
- `Backend.flusherStop` / `flusherDone` channels
- `SessionRegistry.ActiveShadows` (introduced for the flusher;
  audit for other callers before removing)

The qsc.5 race becomes inexpressible: no flusher, no race.

## GC Interaction

The GC design doc carries a wrinkle titled "`state.workingSets[branch]`
cache" with two options (force-flush at GC start, or include the
cache in GC roots). After this work the wrinkle changes shape: chunks
reachable from autoCommit writes ARE on disk (fsynced), so GC's
disk-ref walk covers them. Chunks reachable from in-txn writes live
on session `branchState.workingSet` and are covered by
`sess.VisitGCRoots`. The `branchWS.ws` entry is a cache of the on-disk
WS for a branch; GC does not need to traverse it because the disk
ref it mirrors is already in GC's root set.

Edit the GC doc to drop the wrinkle; replace with one line: "The
on-disk `working_set/heads/<branch>` ref is GC's root for every
non-in-txn write."

## Wrinkles

### First load under entry lock

`loadBranchWS` on a cold entry needs to call
`state.doltDB.ResolveWorkingSet(ctx, wsRef)`. That can be slow.
Holding `entry.mu.Lock()` across it blocks every concurrent
read/write on that branch.

Pragmatic answer: hold `entry.mu.Lock()` during the load. The cold
path is rare (first access per branch per server lifetime). After
the first load, all callers take `entry.mu.RLock()` for reads.

### Re-creating a deleted branch

After `entry.ws = nil` (branch deleted), a later `doltBranch`
creates a fresh branch with the same name. The next access finds the
existing entry with `ws == nil` and reloads from disk. Same code
path as first-time load. Singleton survives the delete/recreate
cycle naturally.

### Cache staleness across in-txn commits

When session A commits an in-txn write via `dsess.CommitWorkingSet`,
the on-disk working_set ref advances. The `branchWS.ws` cache on
dbState is not updated by this path. Session B reading the cache
sees the pre-commit WS.

Fix: post-commit, the commit path updates the cache. The dumbo
`commitDirtyBranchesForSession` (called from
`OnTransactionCommit`) already mirrors the result into
`state.workingSets[branch]` (see helpers.go). Convert that mirror
call to use `branchWS.mu`, store both `ws` and a refreshed
`wsHash` (via the same ResolveWorkingSet+HashOf pattern as
`updateBranchWS`).

If we miss a mirror site, the cache becomes stale. Readers see an
old WS; the next `updateBranchWS` on that branch will fail its
optimistic lock (`entry.wsHash` no longer matches disk), and the
helper handles that by re-resolving and retrying. So a stale cache
is self-healing at the cost of one extra round trip on the next
write -- not a correctness bug.

### `hydrateAllIndexes` at db open

Already reads from disk via `state.doltDB.ResolveWorkingSet`
(workspace-qsc.1). No change.

### Wire writeConcern parsing

The handler continues to parse writeConcern from the wire request,
including the `j` field. The decoded `SkipDurableSync` bool is no
longer used by autoCommit; it is preserved for future
session-isolation commit-fsync-skip work. Document the autoCommit
behavior in the dumbodb docs.

## Out of Scope

- **Session-isolation j:false commit-fsync-skip.** Distinct from
  the cache refactor; tracked separately if pursued.
- **`state.mu` decomposition for the metadata caches.** Workspace-i0u
  is the right home for moving the index / validator / capped caches
  off dbState; not in scope here.
- **`ResolveWorkingSet` performance work.** Out of scope unless
  profiling shows it on a hot path post-removal.

## Resolved Decisions

1. **Lock granularity:** one RWMutex per branch entry. The map
   itself has its own RWMutex, taken only at entry creation
   (first-touch per branch per server lifetime).
2. **Entry lifetime:** singleton. Created once on first access;
   never removed. Branch deletion nils the entry's `ws` field.
3. **autoCommit j:false:** silently treated as j:true. Wire decode
   unchanged. Documented. Other modes' interpretation of j:false is
   future work.
4. **Deferred flusher:** removed entirely. No 100ms tick, no
   session walk, no `ActiveShadows`.
5. **First-load:** under entry write lock; do not optimize the cold
   path with `sync.Once`.

## Test Coverage

- Concurrent writes on different branches must not contend. Two
  goroutines on different branches; assert throughput is roughly
  2x a single-branch baseline (or, more cheaply, instrument
  `state.mu` contention and assert near-zero on the WS path).
- Concurrent writes on the same branch serialize correctly via
  `entry.mu`. Two goroutines on the same branch, last-write-wins,
  no torn state.
- Branch delete then re-create: `entry.ws` is nilled, then
  re-populated on first access to the new branch. Same entry
  pointer.
- writeConcern j:false against autoCommit: the wire decode still
  works; the write still completes; the on-disk ref advances.
- In-txn commit refreshes the cache: session A commits an in-txn
  write; session B's next non-txn read sees the new WS.

The session-routed tests added in workspace-qsc.7 stay; the in-txn
contract is unchanged.

## Migration Order

Four tasks, in this order:

1. **ws-sn.1: Introduce singleton entries + helpers.** Add
   `branchWS` type, `branchWSMu`, `branchWS` map on `dbState`. Add
   `branchEntry`, `loadBranchWS`, `updateBranchWS`. Do NOT yet wire
   to call sites. New code is dead; no behavioral change.
2. **ws-sn.2: Convert all `state.workingSets` access sites.**
   Mechanical, ~17 sites. After this commit, `state.workingSets`
   is unreferenced.
3. **ws-sn.3: Drop autoCommit j:false interpretation and remove
   the deferred flusher.** `updateWorkingRoot`'s `skipSync`
   parameter still exists at the call interface for non-autoCommit
   modes, but inside `updateWorkingRoot` it is forced to false in
   autoCommit. `deferredFlushLoop`, `flushAllDirty`, flusher
   channels deleted. Bats covers j:false against autoCommit.
4. **ws-sn.4: Delete the `workingSets` field.** Remove the map
   field on `dbState`, any init code that seeds it, any leftover
   write sites flagged by the compiler. Update the GC design doc
   to drop the `state.workingSets[branch]` wrinkle.

Each task: single commit, go tests pass, bats tests pass, parity
tests pass, strictly additive testing.
