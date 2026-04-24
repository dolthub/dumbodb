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

For context: since MongoDB 5.0, the default write concern is `{w:"majority"}`
which implies journal sync — MongoDB waits for the write to be flushed to the
on-disk journal before acknowledging. On a standalone server (non-replica-set),
this means every write is crash-safe by default. Both MongoDB and DumboDB
currently fsync on every write, so the benchmark performance gap is purely
engine overhead (WiredTiger vs prolly trees), not a durability shortcut.

With session isolation, DumboDB changes this model: regular writes stay in
memory (no commit, no fsync), and `doltCommit` is the explicit durability
checkpoint. This means DumboDB would be *less* durable per-write than MongoDB's
default, but with the tradeoff of faster writes and version-control-native
semantics. Users who need per-write durability can call `doltCommit` after
every write.

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

## Key Concept: Branch Sessions

A MongoDB session (`lsid`) is server-scoped — one session can touch multiple
databases. In DumboDB, databases map to branches via rootish connection strings:

```js
const main = db.getSiblingDB("mydb")              // mydb on default branch
const feat = db.getSiblingDB("mydb__d_feat")       // mydb on feat branch
```

From the driver's perspective, these are two separate databases. Both commands
carry the same `lsid`. But they target different branches with different commit
histories and different AddressMaps.

This means **the isolation unit is the (session, database, branch) tuple** — not
the session alone. We call this a **branch session**:

```
Session ABC (lsid)
  ├── mydb / main    → branchSession (forked AM, dirty flag, fork point)
  ├── mydb / feat    → branchSession (forked AM, dirty flag, fork point)
  └── otherdb / main → branchSession (forked AM, dirty flag, fork point)
```

A single client session can have uncommitted changes on multiple branches
simultaneously. Each branch session forks independently and merges independently
via `doltCommit`.

## Proposed Architecture

```
Client A ──► Handler ──► session A ──┬── mydb/main branchSession (forked AM)
                                     └── mydb/feat branchSession (forked AM)
                                              │
                                         doltCommit (on feat)
                                              │
                                         three-way merge ──► feat HEAD AM ──► cs.Commit()

Client B ──► Handler ──► session B ──── mydb/main branchSession (forked AM)
```

### Data Structures

```go
// branchSession holds the isolated working state for one (session, db, branch)
// tuple. This is the fundamental isolation unit.
type branchSession struct {
    mu       sync.Mutex
    forkHash hash.Hash           // branch HEAD hash at fork time (merge base)
    am       prolly.AddressMap   // branch-session-scoped address map
    dirty    bool                // true if any writes have occurred
}

// sessionState holds all branch sessions for one client session (lsid).
type sessionState struct {
    mu       sync.Mutex
    branches map[string]*branchSession  // key = "dbname/branch"
    lastUsed time.Time                  // for timeout-based cleanup
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
    sessions sync.Map  // map[string]*sessionState  (key = lsid UUID string)
}
```

### Lifecycle

1. **First request with new lsid** → create sessionState with empty branches map.

2. **First write to a specific branch** (e.g., `mydb__d_feat`) → create a
   branchSession, fork the current branch AM (`state.am`) and record
   `forkHash = cs.Root()`. Subsequent reads/writes on that branch in this
   session use this forked AM.

3. **Reads** → if a branchSession exists for this (session, db, branch), read
   from its AM (sees own writes). If no branchSession exists (session hasn't
   written to this branch yet), read from the shared branch HEAD directly.

4. **Writes (insert/update/delete)** → mutate the branchSession's AM. No
   `cs.Commit()`, no fsync. Set `dirty = true`. Write prolly map nodes to the
   value store (they need to exist in the chunk store for the AM to reference
   them) but don't update the branch root.

5. **doltCommit on a specific branch** → merge that branchSession into the
   branch HEAD:
   ```
   base   = branchSession.forkHash  (branch HEAD when this session forked)
   ours   = branchSession.am        (session's current state on this branch)
   theirs = state.am                (current branch HEAD, may have advanced)
   
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

**Reading a branch you haven't written to:**
No branchSession exists yet → reads go directly to the shared branch HEAD.
The session only forks on first write. This keeps read-only access cheap.

**Reading a branch you HAVE written to:**
Reads come from the branchSession's AM — you see your own uncommitted changes,
but not changes from other sessions that committed after your fork point. To
pick up other sessions' committed changes, either commit (merge) or explicitly
reset your branchSession.

**Multiple doltCommits in one session on the same branch:**
Each commit merges the branchSession into the branch HEAD, advances the fork
point to the new HEAD, and continues. The branchSession stays alive with a
fresh fork point — no need to reconnect.

**doltCommit on one branch, dirty state on another:**
Each branchSession is independent. Committing `mydb__d_feat` does not affect
the dirty state on `mydb__d_main`. The client can commit branches independently.

**doltBranch/doltCheckout within a session:**
Creating a new branch doesn't affect existing branchSessions. "Checking out"
a different branch (connecting via a different rootish) creates a new
branchSession for that branch if the session writes to it.

**Concurrent doltCommits from different sessions on the same branch:**
Serialized by `state.mu`. Second committer merges against the first's result
using three-way merge. Both succeed unless they conflict on the same documents.

**doltMerge between branches within a session:**
If the session has dirty branchSessions on both the source and target branches,
the merge should require both to be committed first (or error). We don't want
to merge uncommitted state across branches — that's a recipe for confusion.

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
