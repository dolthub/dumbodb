# Session Isolation and Commit-Based Durability

**Status:** Design  
**Author:** mayor  
**Date:** 2026-04-24

## Problem

DumboDB currently has no session isolation. Two clients connected to the same
branch share a single `dbState.am` (AddressMap) protected by a mutex. Writes
are immediately visible to all other clients — last-writer-wins, no isolation.

Every write calls `cs.Commit()` to update the chunk store root pointer. This
adds per-write overhead on top of prolly tree mutation and BSON serialization.
The exact cost breakdown has not been profiled — the overhead may come from the
root commit, tree mutation, serialization, or a combination.

For context: MongoDB by default acknowledges writes after they reach server
memory, without waiting for data to be written to the on-disk journal. This is
called "unacknowledged journal" mode. Data is flushed to disk periodically in
the background (every ~100ms). The tradeoff is speed vs. crash safety — if the
server crashes before the background flush, the most recent writes are lost.
MongoDB users who need crash safety on every write must explicitly opt in.

DumboDB currently does the opposite: every single write goes through the full
commit path, guaranteeing crash safety but at a performance cost. With session
isolation, we can flip this default — writes stay in memory until the user
explicitly calls `doltCommit`, which is both the durability flush and the
version-control checkpoint.

## Design Goals

1. **Session-scoped writes** — each client connection works on its own snapshot.
   Other sessions don't see uncommitted changes.
2. **doltCommit as the durability boundary** — regular writes (insert/update/delete)
   are fast and memory-only. `doltCommit` is the explicit signal to merge changes
   into the branch and flush to disk.
3. **Three-way merge on commit** — when a session commits, its changes merge into
   the branch HEAD using the fork point as the common ancestor. Conflicts are
   surfaced via the existing `doltConflicts` machinery.
4. **Read-your-own-writes** — within a session, reads see the session's own
   uncommitted changes.

## Current Architecture

```
Client A ──┐
            ├──► Handler ──► Backend ──► dbState.am (shared) ──► cs.Commit() (fsync)
Client B ──┘
```

Key structures (`internal/backends/dolt/backend.go:52-59`):

```go
type dbState struct {
    mu    sync.RWMutex
    cs    *nbs.GenerationalNBS    // chunk store (persistent)
    ns    tree.NodeStore          // node store (wraps cs)
    am    prolly.AddressMap       // THE shared address map
    uuids map[string]string       // collection UUIDs
}
```

Every write path (InsertAll, UpdateAll, DeleteAll) acquires `state.mu`, mutates
a prolly.Map, calls `updateAddressMap()` to update `state.am` and `cs.Commit()`
to fsync. All synchronous, all blocking.

## Proposed Architecture

```
Client A ──► Handler ──► session A ──► sessionAM (forked from branch HEAD)
                                          │
                                     doltCommit
                                          │
                                     three-way merge ──► branch HEAD AM ──► cs.Commit()
                                          │
Client B ──► Handler ──► session B ──► sessionAM (forked from branch HEAD)
```

### Session State

```go
// sessionState holds per-connection write state.
type sessionState struct {
    mu       sync.Mutex
    forkHash hash.Hash           // branch HEAD hash at fork time (merge base)
    am       prolly.AddressMap   // session-scoped address map (copy-on-write from branch)
    dirty    bool                // true if any writes have occurred
}
```

Sessions are keyed by MongoDB's `lsid` (logical session ID), which DumboDB
already receives on every command but currently ignores
(`internal/handler/common/insert.go:41`, tagged `ferretdb:"lsid,ignored"`).

### Session Registry

```go
// Added to Backend
type Backend struct {
    // ...existing fields...
    sessions sync.Map  // map[string]*sessionState  (key = lsid string)
}
```

### Lifecycle

1. **First request with new lsid** → create sessionState, fork the current
   branch AM (`state.am`) and record `forkHash = cs.Root()`.

2. **Reads** → read from `session.am` (sees own writes, not other sessions').

3. **Writes (insert/update/delete)** → mutate `session.am`. No `cs.Commit()`,
   no fsync. Set `session.dirty = true`. Write prolly map nodes to the value
   store (they need to exist in the chunk store for the AM to reference them)
   but don't update the branch root.

4. **doltCommit** → merge session into branch:
   ```
   base  = session.forkHash    (the branch HEAD when session forked)
   ours  = session.am          (session's current state)
   theirs = state.am           (current branch HEAD, may have advanced)
   
   merged = three_way_merge(base, ours, theirs)
   ```
   If no conflicts: update `state.am = merged`, `cs.Commit()` (fsync),
   create a dolt commit with metadata. Reset session: `forkHash = new HEAD`,
   `am = merged`, `dirty = false`.
   
   If conflicts: surface via `doltConflicts` (existing machinery). Session
   stays dirty until conflicts are resolved.

5. **Disconnect without commit** → discard session state. Uncommitted changes
   are lost. This is intentional — `doltCommit` is the durability contract.

6. **Session timeout** → same as disconnect. Configurable timeout (e.g., 30 min).

### Write Path Change

Current (`collection.go:InsertAll`):
```go
state.mu.Lock()
// ...mutate prolly map...
state.updateAddressMap(ctx, ...)  // updates state.am + cs.Commit()
state.mu.Unlock()
```

Proposed:
```go
session := b.getOrCreateSession(ctx, lsid)
session.mu.Lock()
// ...mutate prolly map using session.am...
session.am = newAM  // update session's AM only
session.dirty = true
session.mu.Unlock()
// NO cs.Commit(), NO fsync
```

### Merge Implementation

The three-way merge uses Dolt's existing prolly.AddressMap diff machinery:

1. Diff `base..ours` → set of collections changed by this session
2. Diff `base..theirs` → set of collections changed by other sessions
3. For collections changed only by one side: take that side's hash
4. For collections changed by both sides: per-collection merge:
   - Diff the prolly.Map entries (keyed by hashed `_id`)
   - Same `_id` modified by both → conflict
   - Non-overlapping changes → merge cleanly
5. Assemble the merged AddressMap

This is the same algorithm as `doltMerge` but operating at the session level
rather than the branch level. The merge code already exists in backend.go for
the `doltMerge` command — extract and reuse it.

### WriteConcern Integration

MongoDB's `writeConcern` lets clients choose per-write durability guarantees.
With session isolation, DumboDB simplifies this — all regular writes are
memory-only, and `doltCommit` is the single durability signal:

| Client request | DumboDB behavior |
|---|---|
| Regular write (insert/update/delete) | Writes to session AM in memory. Fast, not crash-safe. |
| `doltCommit` | Merges session into branch, flushes to disk. Crash-safe. |

The MongoDB `writeConcern` parameter is accepted but effectively irrelevant —
individual writes are always memory-only regardless of what the client requests.
`doltCommit` is always crash-safe regardless of what the client requests. This
is a deliberate departure from MongoDB's model: DumboDB's durability story is
version-control-native, not per-write configurable.

### Session Identity and Reconnection

MongoDB's `lsid` (logical session ID) is a client-generated UUID sent with
every command in the wire protocol:

```js
// Driver creates a session
const session = client.startSession()
// session.id = { id: UUID("abc-123-...") }

// Every command includes lsid:
// { insert: "col", documents: [...], lsid: { id: UUID("abc-123-...") } }
```

The session is tied to the `lsid`, **not the TCP connection**. This means:

**Reconnection:** If a client disconnects (network glitch, process restart) and
reconnects with the same `lsid`, DumboDB looks up the session in the registry
and resumes where the client left off — uncommitted writes are still there.

**Session timeout:** Sessions persist for a configurable period of inactivity
(default: 30 minutes). After timeout, the session state is discarded and
uncommitted changes are lost. This matches MongoDB's `logicalSessionTimeoutMinutes`.

**endSession command:** Explicit client signal to discard session state. Any
uncommitted changes are lost. DumboDB should implement this alongside the
session registry.

**Session transfer:** The wire protocol allows two different connections to
share an `lsid`. This means two processes could collaborate on the same
uncommitted working set. We don't need to encourage this, but it falls out
naturally from keying on `lsid` rather than connection.

**Retryable writes (future):** MongoDB uses `lsid` + `txnNumber` to deduplicate
retried writes. If a client retries an insert after a timeout, the server
recognizes the `(lsid, txnNumber)` pair and returns the cached result instead
of double-inserting. Not required for Phase 1, but the session registry makes
it straightforward to add later.

### What About Non-Session Clients?

Clients that don't send `lsid` (e.g., simple `mongosh` without explicit
sessions) get an implicit session scoped to their TCP connection. DumboDB
generates an internal session ID on connect, and discards it on disconnect.
Behavior is identical — writes accumulate in the implicit session, `doltCommit`
flushes.

### Session Security: Discovery and Hijacking

Since `lsid` is client-generated, a concern arises: can another user discover
and attach to someone else's session? In MongoDB, `$listSessions` and
`$listLocalSessions` aggregation stages enumerate active sessions (with admin
privileges). In standard MongoDB this is benign because sessions don't hold
uncommitted data. In DumboDB, where sessions hold uncommitted working sets,
session discovery would allow data access.

**Options (no decision required now):**

1. **Don't implement `$listSessions`** — simplest first pass. Without a
   discovery mechanism, the `lsid` UUID (128 bits) is effectively unguessable.
   No enumeration = no hijacking vector. This is the likely Phase 1 approach.

2. **Scope sessions to authenticated users** — tie sessions to `(lsid, username)`
   so only the user who created the session can resume it. MongoDB already does
   this when auth is enabled. This is the right long-term answer, but DumboDB
   currently has **no authentication at all**, so this is deferred until auth
   lands.

3. **Server-generated session IDs** — DumboDB ignores the client's `lsid` and
   generates its own, returning it to the client. More secure but breaks wire
   protocol expectations. Not recommended.

### Edge Cases

**Session reads branch HEAD instead of session state:**
Not allowed. Once a session forks, it reads its own AM. To see other sessions'
committed changes, the session must either commit (which merges) or explicitly
reset/re-fork.

**Multiple doltCommits in one session:**
Each commit merges, advances the fork point, and continues. The session stays
alive with a fresh fork point.

**doltBranch/doltCheckout within a session:**
Switching branches discards the current session's dirty state (or errors if
dirty). The session re-forks from the new branch's HEAD.

**Concurrent doltCommits from different sessions:**
Serialized by `state.mu`. Second committer merges against the first's result.
Both succeed unless they conflict on the same documents.

## What This Enables

- **Fast writes** — no fsync per operation, just in-memory AM mutation
- **Session isolation** — MVCC-like semantics without the complexity
- **Explicit durability** — `doltCommit` = "I'm done, make it permanent"
- **Conflict detection** — two sessions editing the same doc get a clean error
- **Natural fit with version control** — every doltCommit is a versioned
  checkpoint with message, author, and parent chain

## Migration Path

1. **Phase 1:** Add session registry, fork-on-first-write, write to session AM
   instead of shared AM. Keep `doltCommit` doing what it already does but add
   the merge step. Reads from session AM.
2. **Phase 2:** Remove synchronous `cs.Commit()` from individual write ops.
   Only `doltCommit` fsyncs.
3. **Phase 3:** Session timeout, cleanup, metrics.

## Files to Change

| File | Change |
|---|---|
| `backend.go` | Add session registry, getOrCreateSession() |
| `collection.go` | InsertAll/UpdateAll/DeleteAll use session AM |
| `helpers.go` | updateAddressMap becomes session-aware |
| `backend.go` (DumboDBCommit) | Add three-way merge before commit |
| `conninfo/conn_info.go` | Thread lsid through context |
| `handler/common/insert.go` | Stop ignoring lsid, pass to backend |
| `handler/common/update_params.go` | Same |
| `handler/common/delete_params.go` | Same |
