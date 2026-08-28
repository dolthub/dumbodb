# Publishing a Working Set

**Status:** Problem statement. No implementation proposed for now; see
section 6 for when this is picked up.
**Problem:** concurrent writes to one branch overwrite each other, and the
optimistic lock that exists to prevent it cannot detect the case it exists for.

## 1. The problem

Two clients update different fields of the same document at the same time.
Both are acknowledged. One update is then absent from the stored document.

Measured over 60 rounds per configuration, with both writers released at the
same instant:

| mode | filter | rounds | lost |
|---|---|---|---|
| default | CAS on `v` | 60 | 8 |
| default | plain update, no CAS | 60 | 12 |
| `--auto-commit` | CAS on `v` | 60 | 9 |
| `--auto-commit` | plain update, no CAS | 60 | 10 |

It happens with or without a compare-and-swap filter, so this is a lost write
rather than a CAS problem. Across 39 failing rounds the surviving document was
never a field-merged combination of the two writes: it was always one writer's
document, whole. No merge is involved.

## 2. How a write is published

`updateWorkingRoot` (`internal/backends/dolt/helpers.go:705`):

```go
ws, err := state.getOrInitBranchWS(ctx, branch)   // :706  snapshot the working set
newRV, err := fn(ws.WorkingRoot())                // :711  apply the change to that snapshot
newWS := ws.WithWorkingRoot(newRV)...             // :725  a complete working set built from it
...
state.updateBranchWS(ctx, branch, func(_ *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
    return newWS, nil                             // :749  publish, ignoring current state
})
```

Three properties follow, and together they are the defect:

- **The snapshot at `:706` is taken outside any lock.** The writer's view of
  the branch is fixed before it contends with anyone.
- **`fn` produces a finished root, not a delta.** By the time anything is
  serialized, the writer's change has been folded into a complete picture of
  the branch. There is no separate "my change" left to reconcile.
- **The publish ignores the current state.** The callback receives the working
  set as it stands and names it `_`.

So each write publishes a whole-world snapshot: "the branch looks like this
now", derived from a view that may already be stale.

## 3. Why the optimistic lock does not catch it

`updateBranchWS` (`internal/backends/dolt/branch_ws.go:166`) does hold a lock
and does pass a prevHash to Dolt:

```go
if err := s.doltDB.UpdateWorkingSet(ctx, wsRef, newWS, e.wsHash, meta, &rsc); err != nil {
```

`e.wsHash` belongs to `branchWS` (`branch_ws.go:39`), the singleton entry for a
`(database, branch)` pair -- "the returned pointer is stable for the life of
the dbState; concurrent callers see the same pointer". It holds the hash of
whatever was last written **through that cache**, and is refreshed from disk
after each write.

That is the wrong reference point. Two writers, A and B, both reading root R0:

1. A snapshots R0, builds `newWS_A` = R0 + A's change.
2. B snapshots R0, builds `newWS_B` = R0 + B's change.
3. A publishes. prevHash matches disk, the write succeeds, `e.wsHash` becomes
   the hash of `newWS_A`.
4. B publishes. prevHash is `e.wsHash`, which is now A's hash, which is what
   is on disk -- **so the compare-and-swap succeeds.**
5. `newWS_B` is on disk. It was derived from R0 and does not contain A's
   change. A's acknowledged write is gone.

The check answers "has the ref moved since the cache last synced". The question
that matters is "has the ref moved since *this writer* read the state it
derived its value from". Those differ exactly when two writers overlap, which
is the only situation the lock exists for.

The transaction-commit path has the same shape for a different reason
(`internal/backends/dolt/backend.go:1256`):

```go
// Use the current on-disk working set hash as the optimistic-lock prevHash.
// ws.HashOf() returns an error for in-memory WS created via WithWorkingRoot,
// so we resolve from disk instead.
```

Re-resolving from disk immediately before writing makes the compare-and-swap
unable to fail by construction. The comment records the obstacle honestly: a
working set built in memory with `WithWorkingRoot` cannot hash itself. **That
obstacle shapes the fix.** The fork-point hash has to be captured when the
working set is read, from the on-disk object, and carried alongside the
in-memory one. It cannot be recovered later from the value being written.

This is what Dolt does: `DoltTransaction.doCommit` takes `existingWSHash` from
the working set it merged against and passes that as prevHash, then retries on
`datas.ErrOptimisticLockFailed`.

## 4. Why a mutex cannot fix this

`e.mu` is real and is held across the publish. It does not help, because the
value being published was computed before the lock was taken. Serializing the
writes does not make the second writer's picture of the branch current.

The deeper issue is that `branchWS` is shared mutable state standing in for the
current state of a branch, so it needs a mutex at all. In Dolt's model each
session holds its own working set, and the only shared object is the ref in the
chunk store, where concurrency is settled by compare-and-swap and retry. No
process-level mutex arbitrates between writers.

The singleton was introduced to fix lock contention: `dbState.mu` guarded every
mutable field and was a chokepoint, so the per-branch entry split it
(`branch-ws-singletons.md`). That is a good reason for a cache. It is not a
reason for the cache to be what the optimistic lock is keyed on. A read cache
and a fork point are different jobs, and the two were conflated.

## 5. Two fixes, and one subsumes the other

**Narrow.** Capture the working set's hash where it is read, before it is
mutated, and thread it through as the prevHash. A stale writer then gets
`ErrOptimisticLockFailed` instead of clobbering, and re-reads and re-applies.
This makes the existing lock honest and turns silent loss into a detectable,
retryable failure. It does not remove the shared snapshot.

**Structural.** Every writer accumulates in its own working set against a
pinned base, and publishes by compare-and-swap against the hash it forked from,
retrying on failure. Nothing is shared, so nothing needs the mutex, and the
singleton reverts to being a cache for readers. This is the Dolt model, and it
is what makes a three-way merge possible at all: a merge needs base, ours and
theirs kept apart, which `fn`-produces-a-finished-root destroys.

The structural fix is tracked as workspace-oug.1. The narrow fix is worth
having only if the structural one is delayed, since it is subsumed.

## 6. When this is picked up

After the four merge strictness levels are clearly supported by the Dolt model.
The levels decide what counts as a collision; this decides whether a collision
is detectable at all. Sequencing the levels first keeps the reconciliation
semantics settled before the publish path is rebuilt around them.

## 7. Measured versus derived

- **Measured:** the loss rates in section 1, and that the survivor is never a
  field-merged document.
- **Read from the code:** every file and line cited in sections 2 and 3.
- **Derived:** the A/B interleaving in section 3 is the reading of that code
  which explains the measurements. It has not been observed directly with
  instrumentation.
