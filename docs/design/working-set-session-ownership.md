# Working-Set Ownership: Move In-Flight State Onto Sessions

**Status:** Design
**Date:** 2026-05-26
**Side quest of:** GC integration. Not strictly required to ship GC, but the
current shape makes GC fragile and `dbState.mu` a write chokepoint.

## Problem

Per-session in-flight working-set state currently lives on `dbState`, not on
the session:

```go
// internal/backends/dolt/backend.go:144-148
type dbState struct {
    ...
    workingSets map[string]*doltdb.WorkingSet           // branch -> committed working set
    pendingWS   map[pendingWSKey]*pendingTxnState        // (owner, branch) -> overlay
}
```

Two consequences:

1. **`dbState.mu` is a write chokepoint.** Every write on any branch in a
   database takes the same lock, even when two clients are working on
   independent branches.
2. **The state is invisible to `dsess.DoltSession.VisitGCRoots`.** GC reaches
   chunks via two paths: ref enumeration (`DoltDB.Datasets().IterAll()`) and
   per-session `VisitGCRoots`. Refs cover `workingSets[branch]` after a
   `working_set/heads/<branch>` flush, but not before; and they never cover
   `pendingWS` overlays (those are in-memory only). A session with uncommitted
   writes would have its chunks collected.

Dolt does not have an equivalent backend-owned bucket. Uncommitted state is
exclusively session-owned, surfaced by `DoltSession.VisitGCRoots` walking
`dbStates[db].heads[branch].writeSession.workingSet` (`dsess/session.go:803-884`).
DumboDB diverged from this model when the backend-side overlay was introduced
and the refactor never landed.

## Target Model

Mirror Dolt's model. Every uncommitted root is owned by exactly one
`*dsess.DoltSession` and reachable only through that session.

- **Committed branch state** -> the on-disk `working_set/heads/<branch>` ref.
  Read it from `DoltDB` on first touch in a session; do not cache it in a
  backend-owned map. `Datasets().IterAll()` covers it during GC.
- **Per-session view of a branch (including uncommitted edits)** -> lives in
  `session.dbStates[db].heads[branch]`, specifically in
  `branchState.writeSession.workingSet`. Writes go through
  `WriteSession.GetTableWriter`; the session's working root advances in-place.
- **Fork point / base for three-way merge** -> already captured by dsess in
  `DoltTransaction.dbStartPoints[db].rootHash` when `StartTransaction` is
  called (`dsess/transactions.go:87,158`). Today's `pendingTxnState.base` is
  the same concept, duplicated outside dsess.

After the refactor, `dbState` no longer holds working-set state. It keeps
genuinely per-database fields: chunk store, node store, `*doltdb.DoltDB`,
indexes, UUIDs, collection metadata, capped/view/timeseries registries,
merge-in-progress marker.

## Mapping

| Today (`dbState`-owned) | Target (session-owned) |
|---|---|
| `dbState.workingSets[branch]` (committed view) | Read from `working_set/heads/<branch>` ref on demand; cached per-session in `branchState.workingSet` |
| `dbState.workingSets[branch]` (mutated on every write under autoCommit + non-isolation) | Per-session `branchState.workingSet`, updated via `WriteSession.GetTableWriter`; flushed via `doltdb.UpdateWorkingSet` on commit |
| `dbState.pendingWS[(owner, branch)].current` | `branchState.workingSet` on that owner's session |
| `dbState.pendingWS[(owner, branch)].base` | `DoltTransaction.dbStartPoints[db].rootHash` (already snapshotted by dsess on `StartTransaction`) |
| `dbState.persistAM(branch, am)` (version-control bypass) | Same surface, but writes through the calling session: build the RootValue, call `session.SetRoot(ctx, db, rv)` (or the writeSession equivalent), then `UpdateWorkingSet` to persist the ref |
| `dbState.setAM(branch, am)` (version-control bypass, no ref flush) | `session.SetRoot(ctx, db, rv)` only, no ref flush |
| `Backend.OnTransactionCommit(owner)` iterating `b.dbs` and calling `commitPendingForOwner` | Drive commit through the owner's session: `session.CommitTransaction(ctx)` (dsess handles per-db iteration internally) |
| `Backend.OnSessionEnd(owner) -> abortPendingForOwner` | `session.Rollback` + `SessionEnd` (dsess discards branchStates) |
| Reads that pull `state.workingSets[defaultBranch].WorkingRoot()` directly (`backend.go:592, 713, 1333, 1346, 2557`; `collection.go:1501, 1789, 1943`; `index_persist.go:350`) | Replace with `session.LookupDbState(ctx, qualified)` -> `branchState.WorkingSet().WorkingRoot()` |

## Wrinkles

### `dbState.mu` does not go away entirely

It still guards the non-working-set fields: `indexes`, `secIndexMaps`,
`collIndexAMs`, `validators`, `capped`, `insertionOrder`, `views`,
`timeSeries`, `collSchemaHash`, `mergeState`, `dirtyBranches`. These are
per-database caches/registries, not per-session. The lock contention story
gets better because the write path stops taking it on every insert/update,
but the lock still exists.

We should audit whether each remaining field is correctly per-database or
whether it should also move (e.g., `dirtyBranches` looks per-session in
spirit).

### Call-site classification

The `state.workingSets[defaultBranch].WorkingRoot()` sites:

| Site | Operation | Has a calling session? | Refactor target |
|---|---|---|---|
| `backend.go:592` (`Status`) | Read-only runCommand | Yes | Calling session |
| `backend.go:713` (`ListDatabases`) | Read-only runCommand | Yes | Calling session |
| `backend.go:1333,1346` (inside `DumboDBCommit`) | Write (commit) | Yes | Calling session |
| `backend.go:2557` (`DumboDBReset` soft) | Write | Yes | Calling session |
| `collection.go:1501, 1789, 1943` (auto-commit) | Write | Yes | Calling session |
| `index_persist.go:350` (`hydrateAllIndexes`) | Read-only, db-open | No | See below |

Every site except `hydrateAllIndexes` has a calling client session and
routes through it. There is no need for an internal backend-owned session.

### `hydrateAllIndexes` and the broader version-controlled-metadata problem

`hydrateAllIndexes` reads `workingSets[defaultBranch]` at db open to
populate `state.indexes`, `state.secIndexMaps`, `state.collIndexAMs`. Those
are per-database caches of "the indexes that exist for each collection."
The same pattern holds for `state.uuids`, `state.validators`,
`state.capped`, `state.views`, `state.timeSeries`, `state.collSchemaHash`:
all are populated assuming the *default branch's* working root is the
authoritative source.

This is wrong. Indexes, validators, capped-collection metadata, views, and
time-series metadata are all stored inside the working root and are
therefore branch-scoped. A collection can be capped on `main` but not on
`feat`; an index can exist on one branch and not another. The current
caches collapse all branches' metadata into one per-database view and lose
that distinction.

**This is a pre-existing bug, not something this refactor introduces.** It
is also bigger than this refactor: the right fix is to move every
metadata cache off `dbState` and onto whatever `branchState` (or its
DumboDB equivalent) the calling session is operating on, populating each
on first touch by reading the branch's working root.

**Scope for this refactor:** preserve existing behavior for the metadata
caches. `hydrateAllIndexes` keeps doing what it does today, but the
working-root lookup switches from `workingSets[defaultBranch]` to a direct
`doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+defaultBranch))`
call. Same wrong shape, different (correct) source. A separate design
follows up on moving all the metadata caches onto branch-scoped state.

### Version-control bypass paths (`persistAM`, `setAM`)

`dumboMerge`, `dumboReset`, `dumboBranch`, and the post-merge persistence step
in `commit_session_isolation.go:82` (`persistAM(merged)`) all produce a
RootValue externally and assign it to a branch. They do not go through
`TableWriter`.

This is fine -- dolt's `dolt_merge` / `dolt_reset` SQL procedures do the same.
The required change is mechanical: instead of `state.workingSets[branch] = newWS`
followed by `updateWorkingSet(...)`, call into the session
(`session.SetWorkingSet(ctx, db, branch, newWS)`-equivalent, dsess's exact
API to be confirmed during implementation) and let dsess track the change.

The fact that these paths exist means: **the write path is not exclusively
`TableWriter`**. Any session-ownership story has to accommodate "session also
holds the result of bulk operations that built a RootValue externally."

### Default-mode (non-isolated) writes between two sessions

Today, two clients writing to the same (db, branch) under default mode (no
`--session-isolation`) share `workingSets[branch]`. `DocLockManager` prevents
write conflicts at the document level, but both clients are mutating the same
in-memory working set under `dbState.mu`.

After the refactor, each session has its own `branchState.workingSet`. Commit
becomes a three-way merge (almost always a fast-forward, because doc locks
prevented overlap). Commits are serialized by dsess's `Provider().TxLocks()`
keymutex on `dbname + " " + workingSetRef` (`dsess/transactions.go:418`).

This is closer to the dsess design intent and removes the
write-amplification of taking `dbState.mu` on every operation. It also moves
the semantics: a default-mode write is no longer immediately visible to other
sessions reading from `workingSets[branch]`; it is visible only after the
implicit single-statement transaction commits. For `autoCommit = true` (the
MongoDB-default-durability mode), this is the existing semantics. For
`autoCommit = false`... we currently have no such mode in default
(non-isolated) operation, so there is nothing to break.

### `WorkingSetHashes` and what GC sees mid-write

Once a session writes through `WriteSession.GetTableWriter` and closes the
writer, `prollyWriteSession.FlushTable` (`writer/prolly_write_session.go:169`)
updates the in-memory working set. From that moment, `VisitGCRoots` on this
session walks `head.writeSession.GetWorkingSet()` and finds every new chunk
(`dsess/session.go:840`). The keeper contract is satisfied without any
DumboDB-specific code -- this is the entire payoff of moving onto the session.

### Initialization on first touch

dsess's `lookupDbState` lazily materializes a `branchState` for a (session,
db, branch) tuple on first use, loading the working set from the
`working_set/heads/<branch>` ref. DumboDB today eagerly populates
`workingSets[branch]` when a database is opened. After the refactor, eager
population is unnecessary: the first command in a session triggers the lazy
load.

We need to confirm `dumbodbProvider.SessionDatabase` (`dsess_provider.go`)
returns the right shape so dsess can drive this lazy load. The provider
already exists; the wiring should be a small adjustment, not new code.

## Out of Scope For This Refactor

- Anything in `--session-isolation` mode beyond what's needed to keep the
  existing parity tests passing. The mode flag stays; the difference between
  default and `--session-isolation` collapses to "does `DocLockManager`
  participate?" because both modes now use per-session branchStates.
- GC itself. This refactor's value is making GC safe and tractable; the GC
  work follows in its own design.
- The `autoCommit` mode semantics. Per-write durability still flushes the ref
  on every commit; the new model just inserts a `writeSession` step before
  the flush.

## Resolved Decisions

1. **No long-lived internal session.** Only one read site (`hydrateAllIndexes`
   at db open) has no calling session, and it resolves the working set
   directly off `DoltDB`. Every other site is reachable from a real client
   session. `Backend.NewSession()` keeps creating a fresh session per call
   for the operations that need an ephemeral one.

2. **`dirtyBranches` is a fsync-coalescing index, and it moves onto the
   session.** Justification of its existence: it batches `UpdateWorkingSet`
   (NBS journal fsync) calls for `j:false` writes. When a write comes in with
   `skipSync=true` (`helpers.go:501`), the in-memory working set is updated
   but the ref-write is deferred. The flusher loop (`backend.go:504`) drains
   the set every 100ms, so N writes to the same branch within a window
   amortize into one fsync.

   Post-refactor, the in-memory working set lives on a session's
   `branchState`, and dsess already has `branchState.dirty`. The deferred
   flusher reworks to iterate active sessions in `SessionRegistry` and
   flush each session's dirty branchStates via dsess's normal commit/flush
   path. The per-database `dbState.dirtyBranches` field is deleted; the
   `deferredFlushLoop` and `flushAllDirty` become session-driven.

3. **Session-aware API.** `persistAM` / `setAM` take an explicit session
   parameter and update the session's branchState. Every caller already has
   a session in context; making it explicit removes the ambiguity.

4. **Delete `dbState.workingSet()` and `dbState.setWorkingSet()`.** Wrong
   abstraction post-refactor. Anything else this refactor renders dead
   (accessors, helpers, the `pendingWSKey` / `pendingTxnState` types, the
   `OnSessionEnd` / `OnTransactionCommit` per-db iteration helpers if dsess
   subsumes them) goes with it. No deprecation period; this branch is the
   transition.

## Migration Order

A landing order that keeps the tree green at each step:

1. **Read paths.** Convert the `state.workingSets[defaultBranch].WorkingRoot()`
   read sites to go through the calling session, except `hydrateAllIndexes`
   which switches to a direct `doltDB.ResolveWorkingSet` lookup. `workingSets`
   is still written the old way; reads now have two paths during the
   transition.
2. **`pendingWS` -> session.** Move per-session overlays onto
   `branchState.writeSession`. `dumboCommit` (session-isolation mode)
   delegates to dsess's three-way merge instead of `mergePendingIntoCommitted`.
   `OnTransactionCommit` / `OnSessionEnd` drive the owner's session
   directly.
3. **Write paths.** Convert insert/update/delete in `collection.go` to go
   through `WriteSession.GetTableWriter`. `dbState.workingSets[branch]` is
   no longer written by client operations.
4. **Version-control paths.** `persistAM` / `setAM` get a session parameter
   and update the calling session's branchState. `dumboMerge` / `dumboReset`
   / `dumboBranch` flow through the calling session.
5. **Flusher.** Rework `deferredFlushLoop` to iterate `SessionRegistry`
   and flush each session's dirty branchStates via dsess's normal flush
   path. Delete `dbState.dirtyBranches`.
6. **Delete the map.** Remove `dbState.workingSets`, `dbState.pendingWS`,
   `pendingWSKey`, `pendingTxnState`, `dbState.workingSet()`,
   `dbState.setWorkingSet()`, and any helpers that are no longer reachable.
   Audit `dbState.mu` for any remaining contention hot paths.
7. **Tests.** **Strictly additive: existing tests must not be altered.**
   Every existing session-registry, session-lifecycle, persist, reset,
   diff, and parity test must pass unmodified at each step. If a behavior
   shift would force an existing test to change, that is a signal the
   refactor has broken a contract the tests were guarding -- stop and
   reconsider, do not edit the test. New tests are added alongside to
   cover cases the existing suite did not exercise (cross-session
   isolation under load, dirty-branch flush serialization through dsess,
   GC root coverage for in-flight writes).

Each step is its own bd issue with the prior as a dependency.
