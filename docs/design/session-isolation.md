# Session Isolation, Transactions, and Commit-Based Durability

**Status:** Design  
**Author:** mayor  
**Date:** 2026-04-24 (updated 2026-04-29)

## Two Modes of Operation

DumboDB supports two concurrency models, selected at server startup:

### Default mode (no flag)

MongoDB-compatible transactions via `startTransaction` / `commitTransaction` /
`abortTransaction`. Uses **pessimistic document-level locking** — when a
transaction writes to a document, other transactions that try to write to the
same document **block** until the first transaction commits or aborts. If the
lock wait exceeds a timeout, the blocked transaction gets a `WriteConflict`
error and must retry.

This matches MongoDB's behavior: two transactions can never both succeed with
conflicting writes to the same document.

Non-transactional writes (outside `startTransaction`) use shared state with
document-level mutex, same as MongoDB's default.

### `--session-isolation` mode

Version-control-native isolation via `doltCommit`. Uses **optimistic concurrency
with three-way merge** — each session works on its own forked snapshot. At
`doltCommit` time, changes are merged back using the fork point as the common
ancestor. If two sessions modified the same document, the second committer gets
a merge conflict (surfaced via `doltConflicts`).

In this mode, `startTransaction` returns an error:
```
{ ok: 0, errmsg: "Transactions are not available in session-isolation mode. Use doltCommit instead.", code: 263 }
```

## How MongoDB Transactions Work

MongoDB uses pessimistic locking for transactions:

1. Client A calls `startTransaction()`, then writes to doc `_id: 1`
   → Server acquires a write lock on that document
2. Client B calls `startTransaction()`, then tries to write to doc `_id: 1`
   → Server **blocks** Client B (or returns `WriteConflict` after timeout)
3. Client A calls `commitTransaction()` → lock released, changes visible
4. Client B retries and succeeds

Key properties:
- Conflicts are **prevented** (via locks), not detected after the fact
- `abortTransaction()` discards all changes and releases locks
- Reads within a transaction see a **snapshot** from when the transaction started
- Multiple collections can participate in a single transaction

### How DumboDB Implements This (Default Mode)

The session registry and branch session infrastructure (described below) serves
both modes. In default mode:

- `startTransaction` → creates a branch session (forked AM), sets `inTransaction = true`
- Writes within the transaction → mutate the branch session's AM
- **Document locking**: when a transaction writes to a document, its `_id` is
  added to a per-branch lock set. Other transactions that try to write to the
  same `_id` receive `WriteConflict` (error code 112) immediately
- `commitTransaction` → flush the branch session's AM to the shared state
  (no three-way merge needed — locks guarantee no conflicts). `cs.Commit()` for
  durability. Release all document locks.
- `abortTransaction` → discard the branch session's AM. Release all document locks.

Non-transactional writes (no `startTransaction`) continue to use the shared
`state.am` directly with mutex serialization, same as today.

### How DumboDB Implements This (`--session-isolation` Mode)

- Every write implicitly forks (no `startTransaction` needed)
- No document locking — two sessions can write to the same document concurrently
- `doltCommit` triggers a three-way merge with conflict detection
- Conflicts are surfaced, not prevented
- `startTransaction` returns an error directing users to `doltCommit`

---

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
semantics.

For users who want MongoDB-compatible durability (every write crash-safe),
DumboDB supports an `autoCommit` mode. When enabled, every write operation
automatically triggers a `doltCommit` after it completes — equivalent to
MongoDB's default behavior. This is the simplest migration path for existing
MongoDB applications that don't use version control features and expect
per-write durability.

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
const feat = db.getSiblingDB("mydb@feat")       // mydb on feat branch
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
// tuple. This is the fundamental isolation unit for both transaction modes.
type branchSession struct {
    mu            sync.Mutex
    forkHash      hash.Hash           // branch HEAD hash at fork time (merge base)
    am            prolly.AddressMap   // branch-session-scoped address map
    dirty         bool                // true if any writes have occurred
    inTransaction bool                // true between startTransaction/commit/abort
    txnNumber     int64               // MongoDB transaction number
    lockedDocs    map[string]hash.HashSet // docs locked by this txn (key = collection name)
}

// sessionState holds all branch sessions for one client session (lsid).
type sessionState struct {
    mu       sync.Mutex
    branches map[string]*branchSession  // key = "dbname/branch"
    lastUsed time.Time                  // for timeout-based cleanup
}

// branchLocks tracks document-level locks for the default (transaction) mode.
// One per (db, branch) — shared across all sessions on that branch.
type branchLocks struct {
    mu    sync.Mutex
    locks map[string]map[hash.Hash]string  // collection → docID hash → owning lsid
}
```
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

2. **First write to a specific branch** (e.g., `mydb@feat`) → create a
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
Each branchSession is independent. Committing `mydb@feat` does not affect
the dirty state on `mydb@main`. The client can commit branches independently.

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

## Correctness Testing

Session isolation introduces concurrency and merge semantics that don't exist
in DumboDB today. These tests should live in `tests/` as Go integration tests
running against a live DumboDB server, using multiple concurrent mongo clients.

### Test 1: Uncommitted writes invisible to read-only sessions

Two clients connect to the same branch. Client A inserts a document. Client B
(read-only, has not written) reads the collection. Since A hasn't committed,
B should not see A's document. After A commits, B — still having no dirty fork
of its own — reads from the updated branch HEAD and sees the document.

```
Client A: insert {_id: 1, x: "from A"}          ← A forks, gets a dirty branchSession
Client B: find {} → should return [] (empty)     ← B has no fork, reads branch HEAD (unchanged)
Client A: doltCommit                              ← A's insert merges into branch HEAD
Client B: find {} → should return [{_id: 1, x: "from A"}]  ← B still reads HEAD (now updated)
```

### Test 2: Dirty session is pinned to fork point

When Client B has its own uncommitted writes, it reads from its forked AM,
which is pinned to the branch HEAD at the time of B's first write. B does
NOT see commits made by A after B's fork point — until B commits (merges).

```
Client A: insert {_id: 1, x: "from A"}
Client B: insert {_id: 2, x: "from B"}          ← B forks, pinned to current HEAD
Client A: doltCommit                              ← branch HEAD advances
Client B: find {} → should return [{_id: 2}]     ← B sees own write, NOT A's commit
Client B: doltCommit                              ← three-way merge picks up both
Client B: find {} → should return [{_id: 1}, {_id: 2}]
```

### Test 3: Read-your-own-writes

A single client inserts a document and reads it back within the same session,
before committing. The write creates a forked branchSession; reads within that
session use the fork.

```
Client A: insert {_id: 1, x: "hello"}            ← A forks
Client A: find {_id: 1} → should return [{_id: 1, x: "hello"}]  ← reads from fork
Client A: doltCommit
Client A: find {_id: 1} → should still return [{_id: 1, x: "hello"}]
```

### Test 4: Non-conflicting concurrent writes merge cleanly

Two clients write to different documents on the same branch, then both commit.
No conflicts — three-way merge combines both sets of changes.

```
Client A: insert {_id: 1, x: "from A"}           ← A forks from HEAD C0
Client B: insert {_id: 2, x: "from B"}           ← B forks from HEAD C0
Client A: find {} → [{_id: 1}]                    ← A sees own write only (pinned to C0)
Client B: find {} → [{_id: 2}]                    ← B sees own write only (pinned to C0)
Client A: doltCommit → succeeds                   ← HEAD advances to C1
Client A: find {} → [{_id: 1}, {_id: 2}]          ← A's fork resets to C1 after commit; sees both
Client B: find {} → [{_id: 2}]                    ← B still pinned to C0, doesn't see A's commit yet
Client B: doltCommit → succeeds                   ← B merges against C1 (base=C0, ours=B's AM, theirs=C1)
Client B: find {} → [{_id: 1}, {_id: 2}]          ← B's fork resets to C2; sees both
Client C: find {} → [{_id: 1}, {_id: 2}]          ← C is read-only, sees HEAD
```

### Test 5: After commit, session returns to read-only and sees new commits

After committing, a session's branchSession is cleared. The session returns to
read-only mode — subsequent reads go to the branch HEAD, picking up any commits
made by other sessions in the meantime.

```
Client A: insert {_id: 1, x: "from A"}           ← A forks
Client A: doltCommit → succeeds                   ← HEAD advances to C1, A's fork is cleared
Client B: insert {_id: 2, x: "from B"}           ← B forks from C1
Client B: doltCommit → succeeds                   ← HEAD advances to C2, B's fork is cleared
Client A: find {} → [{_id: 1}, {_id: 2}]          ← A has no fork, reads HEAD (C2), sees B's commit
Client B: find {} → [{_id: 1}, {_id: 2}]          ← B has no fork, reads HEAD (C2), sees A's commit
```

### Test 6: Conflicting writes produce a conflict

Two clients modify the same document on the same branch, then both commit.
The second committer should get a merge conflict.

```
Setup: collection already has {_id: 1, x: "original"}
Client A: update {_id: 1}, {$set: {x: "A's version"}}    ← A forks
Client B: update {_id: 1}, {$set: {x: "B's version"}}    ← B forks from same HEAD
Client A: doltCommit → succeeds
Client B: doltCommit → conflict error (both modified _id:1 relative to the common base)
```

### Test 7: Conflict resolution and continue

Continues from Test 6. After a conflict, the session can resolve and re-commit.

```
Client B: doltCommit → conflict on _id:1
Client B: doltConflicts → shows the conflict (base, ours, theirs versions)
Client B: doltResolveConflict (choose "ours" or "theirs" or manual)
Client B: doltCommit → succeeds
```

### Test 8: Abandoned session cleanup

A client writes without committing, then disconnects. Since the write never
reached the branch HEAD (it was only in the session's forked AM), no other
client should see it.

```
Client A: insert {_id: 99, x: "never committed"}  ← A forks
Client A: disconnect (no doltCommit)
Client B: find {_id: 99} → should return [] (empty, B reads HEAD which was never updated)
Wait for session timeout
Verify: session registry no longer holds A's branchSession
```

### Test 9: Multi-branch isolation within one session

A single client writes to two branches in the same session. Each branch has
its own branchSession. Commits are independent.

```
Client A on mydb (main): insert {_id: 1, x: "main write"}    ← forks main branchSession
Client A on mydb@feat: insert {_id: 2, x: "feat write"}   ← forks feat branchSession
Client A on mydb@feat: doltCommit → succeeds (only feat)
Client B on mydb (main): find {} → should NOT see {_id: 1}    ← B reads main HEAD (unchanged)
Client B on mydb@feat: find {} → should see {_id: 2}       ← B reads feat HEAD (updated by A's commit)
Client A on mydb (main): doltCommit → succeeds
Client B on mydb (main): find {} → now sees {_id: 1}          ← B reads updated main HEAD
```

### Test 10: Session reconnection preserves uncommitted state

A client writes, disconnects, reconnects with the same lsid, and continues
working with the same forked branchSession.

```
Client A (lsid: ABC): insert {_id: 1, x: "before disconnect"}  ← forks
Client A: disconnect
Client A (lsid: ABC): reconnect                                  ← same lsid resumes session
Client A: find {_id: 1} → should return [{_id: 1, x: "before disconnect"}]  ← reads from fork
Client A: doltCommit → succeeds
```

### Test 11: Concurrent commits — serialization order

Three clients all fork from the same branch HEAD. Each commit merges against
the result of the previous commit (the current HEAD at commit time), not
against the original fork point.

```
All three fork from branch HEAD C0
Client A: insert {_id: 1}
Client B: insert {_id: 2}
Client C: insert {_id: 3}
Client A: doltCommit → merge(base=C0, ours=A, theirs=C0) → HEAD is C1
Client B: doltCommit → merge(base=C0, ours=B, theirs=C1) → HEAD is C2
Client C: doltCommit → merge(base=C0, ours=C, theirs=C2) → HEAD is C3
Final state: all three documents present
```

### Test 12: Delete + modify conflict

One client deletes a document, another modifies it. The second committer
should get a conflict — the base document was changed by both sides
(deleted vs modified).

```
Setup: collection has {_id: 1, x: "original"}
Client A: delete {_id: 1}                                 ← A forks
Client B: update {_id: 1}, {$set: {x: "modified"}}        ← B forks from same HEAD
Client A: doltCommit → succeeds (_id:1 removed from HEAD)
Client B: doltCommit → conflict (base had _id:1, A deleted it, B modified it)
```

## Transaction Tests (Default Mode)

These tests apply when the server runs WITHOUT `--session-isolation`:

### Test 13: Basic startTransaction / commitTransaction

```
Client A: startTransaction
Client A: insert {_id: 1, x: "in txn"}
Client B: find {} → should return [] (A hasn't committed)
Client A: commitTransaction
Client B: find {} → should return [{_id: 1, x: "in txn"}]
```

### Test 14: abortTransaction discards changes

```
Client A: startTransaction
Client A: insert {_id: 1, x: "will abort"}
Client A: find {_id: 1} → [{_id: 1, x: "will abort"}] (read-your-own-writes)
Client A: abortTransaction
Client A: find {_id: 1} → [] (changes discarded)
```

### Test 15: Document locking — concurrent txn blocked on same doc

```
Setup: collection has {_id: 1, x: "original"}
Client A: startTransaction
Client A: update {_id: 1}, {$set: {x: "A"}}     ← acquires lock on _id:1
Client B: startTransaction
Client B: update {_id: 1}, {$set: {x: "B"}}     ← WriteConflict error (doc locked by A)
Client A: commitTransaction                       ← releases lock
Client B: (retries) update {_id: 1} → succeeds
Client B: commitTransaction
```

### Test 16: Non-conflicting transactions succeed

```
Client A: startTransaction
Client A: insert {_id: 1}                         ← locks _id:1
Client B: startTransaction
Client B: insert {_id: 2}                         ← locks _id:2 (different doc, no conflict)
Client A: commitTransaction
Client B: commitTransaction
Both documents present.
```

### Test 17: startTransaction rejected in --session-isolation mode

```
Server started with --session-isolation
Client A: startTransaction → error: "Transactions are not available in
  session-isolation mode. Use doltCommit instead."
```

## Files to Change

| File | Change |
|---|---|
| `backend.go` | Add session registry, getOrCreateSession(), branchLocks |
| `collection.go` | InsertAll/UpdateAll/DeleteAll use session AM |
| `helpers.go` | updateAddressMap becomes session-aware |
| `backend.go` (DumboDBCommit) | Add three-way merge before commit (session-isolation mode) |
| `conninfo/conn_info.go` | Thread lsid through context |
| `handler/common/insert.go` | Stop ignoring lsid, pass to backend |
| `handler/common/update_params.go` | Same |
| `handler/common/delete_params.go` | Same |
| `handler/msg_session.go` | Implement startSession, endSession |
| `handler/msg_transaction.go` | NEW: startTransaction, commitTransaction, abortTransaction |
| `cmd/dumbodb/main.go` | Add `--session-isolation` flag |
