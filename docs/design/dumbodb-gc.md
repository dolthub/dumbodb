# DumboDB Garbage Collection

**Status:** Design
**Date:** 2026-05-28
**Depends on:** workspace-qsc (closed), workspace-4xp (closed) --
session-routed writes for in-txn chunks + singleton WS entries with
inline `nbs.Commit` for autoCommit chunks. Together those make
`VisitGCRoots` + on-disk ref walk cover everything GC needs.

## Problem

DumboDB persists every write as a new chunk in dolt's `GenerationalNBS`
chunk store. Chunks accumulate; nothing reclaims unreachable ones.

dolt already implements the sweep: `doltdb.DoltDB.GC(ctx, gcConfig,
safepoint)` enumerates branch / remote / internal refs as roots, walks
them, and asks the chunk store to drop everything else. The safepoint
controller coordinates with in-flight readers so chunks are not pulled
out from under them.

What's missing on the dumbo side: an entry point that calls `DoltDB.GC`
with a safepoint controller appropriate for dumbo's session model. That
is what this epic adds.

## Target Model

A new `dumboGC` runCommand triggers GC on demand. The command:

1. Routes through `Shadow.Commit` like any durable runCommand, so it
   participates in the lifecycle bracketing every other command uses.
2. Acts on the runCommand's target database -- the database the client
   chose with `getSiblingDB("name")` or its connection URI.
3. Resolves the wire database name to a single underlying `DoltDB` via
   the existing `SplitRevisionDbName` path. `mydb@feature` and
   `mydb@main` resolve to the same `DoltDB` (one chunk store per
   logical database, holding every branch's chunks). GC sweeps that
   store; every branch in it is in scope.
4. Constructs a session-aware safepoint controller and calls
   `state.doltDB.GC(ctx, gcConfig, sc)`.
5. Returns a single-database report (sizes before/after, duration).

No auto-GC. No kill-connections fallback. No dolt-side refactor. Each
of those is reconsidered separately if and when the manual command
proves insufficient.

## Wire Surface

```
db.runCommand({
  dumboGC: 1,
  mode: "default" | "full" // optional; default: "default"
})
```

- `dumboGC: 1` triggers a default-mode GC on the database the command
  is invoked against. The `1` is a placeholder argument, matching the
  dolt convention used by other dumbo version-control commands
  (`dumboCommit: 1`, `doltBranch: 1`).
- The target database is implicit: whatever `db.runCommand(...)` is
  scoped to via `getSiblingDB("name")` or the connection URI. Branch
  selectors in the wire name (`mydb@feature`) resolve to the same
  underlying `DoltDB` as the base name (`mydb`), so it does not matter
  which branch-qualified handle the client uses.
- `mode: "full"` maps to `chunks.GCMode_Full` (rewrite every chunk,
  including old-gen). `mode: "default"` maps to `chunks.GCMode_Default`
  (only sweep new-gen / unreferenced old-gen). `mode` is the only knob
  exposed in v1; `archive` / `incremental-file-size` (dolt's other
  GC parameters) stay at their defaults.

Response shape:

```
{
  ok: 1,
  db: "test",
  mode: "default",
  durationMs: 1234,
  sizeBefore: 9876543,
  sizeAfter: 5432109,
  chunksBefore: 12345,
  chunksAfter: 6789
}
```

`db` echoes the resolved base database name (stripped of any branch
selector). `chunksBefore`/`chunksAfter` are the chunk-store object
counts before/after the sweep -- separate from byte sizes because a
full-mode GC can shrink chunk count without shrinking bytes (and vice
versa for `default` mode that just unlinks new-gen chunks). On failure
the response is `{ ok: 0, errmsg: "...", code: <n> }` per the standard
wire-error convention.

## Safepoint Controller

Mirrors `dprocedures.sessionAwareSafepointController` (the dolt
session-aware variant), implemented inside the dumbo backend:

```go
// internal/backends/dolt/gc.go
type sessionAwareSafepoint struct {
    controller   *gcctx.GCSafepointController
    callSession  *dsess.DoltSession
    doltDB       *doltdb.DoltDB
    waiter       *gcctx.GCSafepointWaiter
    keeper       func(hash.Hash) bool
    dbname       string
}

func (sc *sessionAwareSafepoint) BeginGC(ctx context.Context, keeper func(hash.Hash) bool) error {
    sc.doltDB.PurgeCaches()
    sc.keeper = keeper
    if err := sc.callSession.VisitGCRoots(ctx, sc.dbname, keeper); err != nil {
        return err
    }
    sc.waiter = sc.controller.Waiter(ctx, sc.callSession, func(ctx context.Context, s gcctx.GCRootsProvider) error {
        return s.VisitGCRoots(ctx, sc.dbname, sc.keeper)
    })
    return nil
}

func (sc *sessionAwareSafepoint) EstablishPreFinalizeSafepoint(ctx context.Context) error {
    return sc.waiter.Wait(ctx)
}

func (sc *sessionAwareSafepoint) EstablishPostFinalizeSafepoint(ctx context.Context) error {
    return nil
}

func (sc *sessionAwareSafepoint) CancelSafepoint() {
    canceled, cancel := context.WithCancel(context.Background())
    cancel()
    sc.waiter.Wait(canceled)
}
```

This is ~30 lines and copies dolt's structure. Copy is preferred over
extracting from `dprocedures` because:

- The implementation is small and stable.
- Extracting requires moving `sessionAwareSafepointController` out of a
  package that imports the SQL engine; the right home would be a new
  package under `doltcore/doltdb/gcctx` or `doltcore/dsess`, and that's
  a non-trivial cross-cutting change.
- Each dumbo runCommand path has slightly different lifecycle
  requirements (e.g., dumbo does not have `dSess.SetTransaction(nil)`
  after GC the way `dprocedures` does); copying lets us adapt rather
  than parameterize.

If a future change in dolt's controller is load-bearing for correctness,
we revisit -- either by syncing the copy or by lifting at that point.

## Orchestration

```go
// internal/backends/dolt/gc.go
func (b *Backend) DumboDBGC(ctx context.Context, params *backends.GCParams) (*backends.GCResult, error)
```

`params.DBName` is the wire-level name as received (possibly carrying
a branch selector). The orchestrator:

1. Strips any branch selector via `doltdb.SplitRevisionDbName` to get
   the base name, then resolves it to the backing `*dbState` via
   `b.lookupDbStateForDsess`. Error if absent.
2. Pulls the calling session from `ctx` (the same path
   `pushWSToSession` uses). The session is required; in-process
   callers without a session error out. GC needs a `GCRootsProvider`
   to seed `BeginGC`'s root walk.
3. Snapshots `sizeBefore` and `chunksBefore` from the chunk store
   (NBS exposes size / chunk-count via `StorageSize()` / equivalents;
   verify exact method names at implementation time).
4. Constructs `gcConfig` from `params.Mode`.
5. Constructs the `sessionAwareSafepoint` controller.
6. Calls `state.doltDB.GC(ctx, gcConfig, sc)`.
7. Records `sizeAfter`, `chunksAfter`, and `durationMs`, returns the
   single-db report.

`state.mu` is NOT held during `DoltDB.GC`. The chunk store has its own
locking for GC; holding `state.mu` would block every concurrent write
on that database for the full GC duration, which can be seconds.
Concurrent writes during GC are handled by the safepoint -- writes
quiesce briefly at the pre-finalize boundary.

## Wrinkles

### The `dumboGC` command itself sits inside `Shadow.Commit`

`Shadow.Commit` brackets every durable runCommand with
`sess.CommandBegin()` / `sess.CommandEnd()`. Those calls register the
session as "in-flight" with `GCSafepointController`, and
`waiter.Wait(ctx)` would block until the session is quiesced. The
running GC command would deadlock waiting for itself.

dolt avoids this in `dprocedures.RunDoltGC` by passing `callSession`
into the safepoint controller and treating it specially: the controller
visits `callSession` synchronously in `BeginGC` (already in the snippet
above) and skips it during the wait phase. `gcctx.Waiter` takes the
`thisSession` argument exactly to exclude it.

So the deadlock is avoided as long as we plumb `callSession` correctly
into both `BeginGC`'s direct visit AND `Waiter(ctx, callSession, ...)`.
Same as dolt; mirror the structure.

### Background loops bypass the keeper

`handler.Handler.cleanupAllCappedCollections` runs on a background
goroutine that mutates working sets without going through any
`CommandBegin`/`CommandEnd` bracketing. `GCSafepointController` does
not see it. If GC is in flight while capped cleanup is mid-write,
chunks the cleanup has read but not yet committed could be swept.

(Note: the pre-workspace-4xp `Backend.deferredFlushLoop` had the same
problem, but that loop is gone -- ws-sn.3 removed it when autoCommit
writes became inline-durable. Capped cleanup is the only remaining
unbracketed background mutator.)

Fix: bracket the capped-cleanup tick with the keeper. Each tick that
does work must call `b.gcController.SessionCommandBegin(rootsProvider)`
/ `SessionCommandEnd(rootsProvider)` around its work. The natural
rootsProvider is the per-tick view of session state; the existing
internal `ConnInfo` the cleanup builds is the obvious carrier.

The keeper bracket is the same shape as `Shadow.Use`/`Shadow.Commit`
uses, which mirrors dolt's internal pattern. Adopting it for the one
remaining background loop is cheaper than reasoning about which
paths are GC-safe.

This is the load-bearing wrinkle. It needs a sub-task before the
runCommand can ship.

### Branch WS cache and GC root set

`dbState.branchWS[branch].ws` is a cache of the on-disk
`working_set/heads/<branch>` ref (workspace-4xp). Every write goes
through `updateBranchWS`, which atomically advances the entry's
`ws`/`wsHash` AND `ddb.UpdateWorkingSet` -> `nbs.Commit` -> journal
fsync. Disk is always at least as fresh as the cache, never behind.

Consequence: GC's disk-ref walk covers every chunk reachable from
the cache automatically; the cache itself does not need to be in
GC's root set. In-txn writes that have not yet reached disk live
on `session.branchState.workingSet` and are picked up by
`sess.VisitGCRoots` -- the standard dsess path.

No special handling at GC start. The runCommand simply calls
`ddb.GC`; the safepoint controller stalls in-flight writes; the
root walk is complete.

### Cursor state and long-lived reads

A client cursor is backed by reads against the chunk store; the
underlying chunk pointers are held inside the iterator. If GC sweeps
mid-cursor-iteration, the next batch fetch fails.

The safepoint controller addresses this: each command (cursor batch
fetches included) brackets with `CommandBegin`/`CommandEnd`. Between
`BeginGC` and `EstablishPreFinalizeSafepoint`, the controller waits
for every in-flight command to end. New `CommandBegin` calls after
that point block until GC's `PostFinalize`. So a batch fetch either
completes before the safepoint or waits until GC is done -- no
mid-read sweep.

Open cursors that are idle between fetches are NOT bracketed; their
underlying state lives in `internal/clientconn/cursor`. The chunk
pointers they hold can be invalidated by GC. The mitigations:
- Cursor batches re-read from the working set / commit they pinned, so
  chunks reachable from that root are kept. As long as the cursor's
  root commit is reachable from a branch ref or session, its chunks
  survive.
- Cursors against a working set that gets overwritten mid-cursor are
  already a pre-existing inconsistency window unrelated to GC.

No additional work needed for cursors in v1.

## Out of Scope

- **Auto-GC.** Post-`dumboCommit` hooks, size-based triggers,
  `AutoGCController` machinery. Reconsidered separately.
- **Kill-connections safepoint.** Dumbo has no equivalent: severing a
  Shadow returns `ErrShadowInvalidated`, surfaces as
  `NoSuchTransaction` (251), and clients lose lsid / cursor state.
  The cost profile makes the fallback worse than just waiting or
  failing.
- **dolt-side refactor.** No `RunGC` extraction. We reuse public dolt
  APIs (`DoltDB.GC`, `gcctx.GCSafepointController`, public session
  methods) and copy the ~30-line safepoint controller. Revisited only
  if a concrete dolt-private API blocks correctness.
- **Cross-database orphan collection.** Each `*dbState` has its own
  `DoltDB` with its own NBS; nothing is shared across databases.
- **Replication / cluster GC coordination.** DumboDB is single-node.
- **Versioned-metadata caches under workspace-i0u.** Out of scope per
  the qsc design doc; tracked separately.

## Resolved Decisions

1. **Trigger model:** `dumboGC` runCommand only. No auto-GC.
2. **Safepoint strategy:** session-aware only. No kill-connections.
3. **Dolt refactor:** none yet. Copy `sessionAwareSafepointController`
   verbatim into the dumbo backend. Revisit if dolt evolves the
   controller in a way that needs syncing.
4. **Scope:** the runCommand's target database, resolved via the
   standard `getSiblingDB`/connection-URI path. Branch selectors in the
   wire name (`mydb@feature`) collapse to the base database; one chunk
   store per logical database, holding every branch's chunks, and GC
   sweeps that store.
5. **Modes:** `default` and `full` exposed; archive / incremental
   stay at defaults.

## Open Questions for the Implementation

(These are sub-decisions whose right answer becomes obvious during
implementation; calling them out so they aren't forgotten.)

- **Size / chunk-count APIs.** Resolved:
  `chunks.TableFileStore.Size(ctx) (uint64, error)` (implemented by
  `nbs.NomsBlockStore`) and `nbs.GenerationalNBS.Count() (uint32,
  error)`. Both are public and stable.
- **Capped cleanup bracketing.** Lands in its own task before the
  runCommand task -- the runCommand cannot be safe without it.

## Migration Order

The epic decomposes into:

1. **gc.1 -- Keeper-bracket the capped-cleanup loop.** Wrap each
   `Handler.cleanupAllCappedCollections` tick with
   `gcController.SessionCommandBegin`/`SessionCommandEnd` so GC's
   safepoint sees it. No user-visible change. (Pre-workspace-4xp
   this task also covered `deferredFlushLoop`; that loop is gone, so
   capped cleanup is the only remaining unbracketed mutator.)
2. **gc.2 -- Backend GC method + safepoint controller.** Add
   `Backend.DumboDBGC` and `sessionAwareSafepoint` in
   `internal/backends/dolt/gc.go`. No wire command yet; tested via go
   tests. (Pre-workspace-4xp this task also force-flushed
   `state.workingSets` at GC start; that field is gone and inline
   `nbs.Commit` keeps disk fresh, so no force-flush is needed.)
3. **gc.3 -- Wire command + handler.** Register `dumboGC` in
   `handler/commands.go`; build `msg_dumbo_gc.go` that decodes
   `mode` and calls `Backend.DumboDBGC`. Bats coverage.
4. **gc.4 -- Sweep validation parity test.** Insert a workload, drop
   it, run GC, verify chunk count / store size drops via `dolt sql -q`
   against the underlying store (same shape as `conflicts_native.bats`
   teardown).

Each task: single commit, go tests pass, bats tests pass, parity tests
pass, strictly additive testing.
