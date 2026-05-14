# Session Isolation, Transactions, and Commit-Based Durability

**Status:** Design  
**Author:** mayor  
**Date:** 2026-04-24 (updated 2026-05-14)

> **2026-05-14 update.** This design is now built on Dolt's `dsess` package
> (`dolt/go/libraries/doltcore/sqle/dsess`), which already implements the
> per-(session, db, branch) isolation, three-way merge on commit, and conflict
> surfacing that this doc originally proposed by hand. The "Branch Sessions"
> concept maps directly to `dsess.branchState`; the fork-point snapshot to
> `DoltTransaction.dbStartPoints`; the merge-on-commit to
> `DoltTransaction.doCommit`. Pessimistic document locking for default-mode
> MongoDB transactions is added as a thin layer above dsess.

---

## Problem

DumboDB currently has no session isolation. Two clients connected to the same
branch share a single `dbState.am` (AddressMap) protected by a mutex. Writes
are immediately visible to all other clients -- last-writer-wins, no isolation.

Every write calls `cs.Commit()` to update the chunk store root pointer. This
adds per-write overhead on top of prolly tree mutation and BSON serialization.
The exact cost breakdown has not been profiled -- the overhead may come from
the root commit, tree mutation, serialization, or a combination.

For context: since MongoDB 5.0, the default write concern is `{w:"majority"}`
which implies journal sync -- MongoDB waits for the write to be flushed to the
on-disk journal before acknowledging. On a standalone server (non-replica-set),
this means every write is crash-safe by default. Both MongoDB and DumboDB
currently fsync on every write, so the benchmark performance gap is purely
engine overhead (WiredTiger vs prolly trees), not a durability shortcut.

With session isolation, DumboDB changes this model: regular writes stay in
memory (no commit, no fsync), and `doltCommit` (or `commitTransaction`) is the
explicit durability checkpoint. This means DumboDB would be *less* durable
per-write than MongoDB's default, but with the tradeoff of faster writes and
version-control-native semantics.

For users who want MongoDB-compatible durability (every write crash-safe),
DumboDB supports an `autoCommit` mode. When enabled, every write operation
automatically triggers a commit after it completes -- equivalent to MongoDB's
default behavior. This is the simplest migration path for existing MongoDB
applications that don't use version control features and expect per-write
durability.

## Design Goals

1. **Session-scoped writes** -- each client connection works on its own snapshot.
   Other sessions don't see uncommitted changes.
2. **Commit as the durability boundary** -- regular writes (insert/update/delete)
   are fast and memory-only. `doltCommit` or `commitTransaction` is the explicit
   signal to merge changes into the branch and flush to disk.
3. **Three-way merge on commit** -- when a session commits, its changes merge
   into the branch HEAD using the fork point as the common ancestor. Conflicts
   are surfaced via the existing `doltConflicts` machinery.
4. **Read-your-own-writes** -- within a session, reads see the session's own
   uncommitted changes.

## Current Architecture

```
Client A --\
            |--> Handler --> Backend --> dbState.am (shared) --> cs.Commit() (fsync)
Client B --/
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

Every write path (`InsertAll`, `UpdateAll`, `DeleteAll`) acquires `state.mu`,
mutates a `prolly.Map`, calls `updateAddressMap()` to update `state.am` and
`cs.Commit()` to fsync. All synchronous, all blocking.

## Background: How MongoDB Transactions Work

MongoDB uses pessimistic locking for transactions:

1. Client A calls `startTransaction()`, then writes to doc `_id: 1`
   -> Server acquires a write lock on that document
2. Client B calls `startTransaction()`, then tries to write to doc `_id: 1`
   -> Server **blocks** Client B (or returns `WriteConflict` after timeout)
3. Client A calls `commitTransaction()` -> lock released, changes visible
4. Client B retries and succeeds

Key properties:
- Conflicts are **prevented** (via locks), not detected after the fact
- `abortTransaction()` discards all changes and releases locks
- Reads within a transaction see a **snapshot** from when the transaction started
- Multiple collections can participate in a single transaction

## Approach: Two Modes of Operation

DumboDB supports two concurrency models, selected at server startup:

### Default mode (no flag)

MongoDB-compatible transactions via `startTransaction` / `commitTransaction` /
`abortTransaction`. Uses **pessimistic document-level locking** -- when a
transaction writes to a document, other transactions that try to write to the
same document **block** until the first transaction commits or aborts. If the
lock wait exceeds a timeout, the blocked transaction gets a `WriteConflict`
error and must retry.

This matches MongoDB's behavior: two transactions can never both succeed with
conflicting writes to the same document.

Non-transactional writes (outside `startTransaction`) flow through the same
machinery as implicit single-statement transactions.

### `--session-isolation` mode

Version-control-native isolation via `doltCommit`. Uses **optimistic
concurrency with three-way merge** -- each session works on its own forked
snapshot. At `doltCommit` time, changes are merged back using the fork point
as the common ancestor. If two sessions modified the same document, the second
committer gets a merge conflict (surfaced via `doltConflicts`).

In this mode, `startTransaction` returns an error:

```
{ ok: 0, errmsg: "Transactions are not available in session-isolation mode. Use doltCommit instead.", code: 263 }
```

## Key Concept: Branch Sessions

A MongoDB session (`lsid`) is server-scoped -- one session can touch multiple
databases. In DumboDB, databases map to branches via rootish connection strings:

```js
const main = db.getSiblingDB("mydb")             // mydb on default branch
const feat = db.getSiblingDB("mydb@feat")        // mydb on feat branch
```

From the driver's perspective, these are two separate databases. Both commands
carry the same `lsid`. But they target different branches with different commit
histories and different working sets.

This means **the isolation unit is the (session, database, branch) tuple** --
not the session alone:

```
Session ABC (lsid)
  |-- mydb / main    -> branch session (working set, dirty flag, fork point)
  |-- mydb / feat    -> branch session (working set, dirty flag, fork point)
  \-- otherdb / main -> branch session (working set, dirty flag, fork point)
```

A single client session can have uncommitted changes on multiple branches
simultaneously. Each branch session forks independently and merges
independently via `doltCommit`.

## Implementation Foundation: dsess

The Dolt SQL session package `dolt/go/libraries/doltcore/sqle/dsess` already
implements the per-(session, db, branch) isolation model this design needs. We
adopt it wholesale rather than re-implementing it inside the Mongo backend.

### Concept mapping

| This design | dsess |
|---|---|
| Branch session | `branchState` (`database_session_state.go:102`) -- has working set, dirty flag, head commit |
| Session state (per-lsid) | `DoltSession` (`session.go:48`) with `dbStates map[string]*DatabaseSessionState`, each holding `heads map[string]*branchState` |
| Fork point | `DoltTransaction.dbStartPoints[dbName].rootHash` (`transactions.go:87,158`) -- noms root snapshotted at `StartTransaction` |
| Forked state | `branchState.workingSet.WorkingRoot()` -- a `doltdb.RootValue` |
| Three-way merge on commit | `DoltTransaction.doCommit` -> `mergeRoots` -> `merge.MergeRoots(base=startState, ours=workingSet, theirs=existingWs)` (`transactions.go:385`) |
| Branch-level commit serialization | `Provider().TxLocks()` keymutex on `dbname + " " + workingSetRef` (`transactions.go:418`) |
| Durability flush on commit | `doltDb.CommitWithWorkingSet` / `UpdateWorkingSet` (`transactions.go:280, 302`) |
| `doltConflicts` machinery | `validateWorkingSetForCommit` + the `dolt_conflicts` / `dolt_schema_conflicts` tables + `dolt_allow_commit_conflicts` session var |
| Optimistic retry on concurrent commits | `maxTxCommitRetries = 5` loop on `datas.ErrOptimisticLockFailed` |
| `abortTransaction` | `DoltSession.Rollback` -> `clear()` |
| `autoCommit` mode | `DoltCommitOnTransactionCommit` session var (`session.go:481`) -- already auto-creates a dolt commit on transaction commit |

### Why RootValue, not AddressMap

The earlier draft of this doc modeled the per-session forked state as a
`prolly.AddressMap`. With dsess, the isolation unit is `doltdb.RootValue` --
the root that contains the entire database state. What lives inside that root
(tables in the SQL world, collections in the Mongo world) is orthogonal to
session isolation. We map Mongo collections onto Dolt tables (or onto entries
in a structure inside the root -- TBD by the storage design) and dsess does
not need to know.

### What dsess does not provide

Two things this design needs that dsess does not yet have:

1. **lsid-keyed session registry with reconnection and timeout.** dsess assumes
   one `DoltSession` per SQL connection. MongoDB sessions are keyed by `lsid`
   and survive TCP reconnects. We propose adding `dsess.SessionRegistry`
   upstream.
2. **Pessimistic document-level locking** for default-mode MongoDB
   transactions. dsess is intentionally optimistic-merge-on-commit;
   pessimistic locking is MongoDB-specific semantics and does not belong inside
   dsess. We add a `DocLockManager` in DumboDB as a thin layer above dsess.

## Proposed Architecture

```
Client A --> Handler --> session A --+-- mydb/main branchState (working set)
                                     \-- mydb/feat branchState (working set)
                                              |
                                         commit (on feat)
                                              |
                                         dsess three-way merge --> feat HEAD --> fsync

Client B --> Handler --> session B ---- mydb/main branchState (working set)
```

### Data Structures

Per-(session, db, branch) state comes from dsess. DumboDB adds two pieces on
top: an lsid-keyed session registry and a document lock manager.

```go
// SessionRegistry maps MongoDB lsid -> dsess.DoltSession. Proposed upstream
// addition to dsess.
type SessionRegistry struct {
    mu       sync.Mutex
    sessions map[string]*registeredSession  // key = lsid UUID string
    timeout  time.Duration                  // default 30m (matches Mongo)
}

type registeredSession struct {
    sess     *dsess.DoltSession
    lastUsed time.Time
    // gcRoot keeps the chunk-store GC from collecting roots the session is
    // sitting on across reconnect windows.
    gcRoot   gcctx.SessionRoot
}

// DocLockManager provides pessimistic per-document locking for default-mode
// MongoDB transactions. One instance per (db, branch). Lives in DumboDB, NOT
// in dsess -- pessimistic semantics is Mongo-specific.
type DocLockManager struct {
    mu    sync.Mutex
    // collection -> docID hash -> owning lsid
    locks map[string]map[hash.Hash]string
}
```

Sessions are keyed by MongoDB's `lsid` (logical session ID), which DumboDB
already receives on every command but currently ignores
(`internal/handler/common/insert.go:41`, tagged `ferretdb:"lsid,ignored"`).
The per-(session, db, branch) working state -- forked root, dirty flag, write
session, head pointers -- is all held by `dsess.branchState` inside the
registered `DoltSession`.

### Session Registry (upstream proposal)

Proposed upstream as `dsess.SessionRegistry`. dsess currently assumes one
`DoltSession` per SQL connection, but `DoltSession` is a struct that holds
`dbStates`, not tied to a connection -- so the structure is amenable to lsid
keying. The registry handles:

- **Lookup or create** by `lsid` on each Mongo command.
- **Reconnection**: same `lsid` from a new TCP connection resumes the same
  `DoltSession` (including any in-progress `DoltTransaction`).
- **Timeout cleanup**: idle sessions older than the timeout are discarded;
  uncommitted working sets are lost (matching MongoDB's
  `logicalSessionTimeoutMinutes`).
- **GC safepoint integration**: long-lived idle sessions hold roots that must
  not be collected. dsess already has `gcctx.GCSafepointController` for SQL
  sessions; the registry plugs into it.
- **`endSession`** explicit teardown.

This is the one place we extend dsess rather than wrap it, because the
GC-safepoint and lifecycle integration is internal to dsess.

### Default-Mode Implementation

Default mode layers `DocLockManager` on top of dsess. dsess handles isolation,
read-your-own-writes, and commit; the lock manager turns dsess's optimistic
semantics into MongoDB's pessimistic semantics.

- **`startTransaction`** -> `DoltSession.StartTransaction`. dsess snapshots the
  current noms root for every db under management. The session's `branchState`
  for this db/branch becomes the fork.
- **Writes in a transaction** -> resolve the write filter to the set of `_id`s
  affected, then `DocLockManager.Acquire(lsid, db, branch, ids)`. If any id is
  held by another lsid, return `WriteConflict` (error code 112) immediately.
  Then mutate via dsess's `WriteSession.GetTableWriter` -> `TableWriter`. The
  writes land in the session's working set, invisible to other sessions.
- **`commitTransaction`** -> `DoltSession.CommitTransaction`. dsess runs the
  three-way merge against current HEAD (will be FF or non-overlapping by
  construction, because locks prevented overlap), updates working set or
  creates a new dolt commit per `DoltCommitOnTransactionCommit`. Then
  `DocLockManager.Release(lsid)`. The optimistic-retry loop in dsess
  effectively never retries because locks prevented contention.
- **`abortTransaction`** -> `DoltSession.Rollback` discards the working set;
  `DocLockManager.Release(lsid)`.

Non-transactional writes (no `startTransaction`) go through dsess as implicit
single-statement transactions with `DoltCommitOnTransactionCommit` honored per
the `autoCommit` configuration. No document locks are taken -- the per-branch
commit keymutex (`Provider().TxLocks()`) inside dsess serializes them.

### `--session-isolation` Mode Implementation

- Every write implicitly forks via dsess (the working set is the fork).
- `DocLockManager` is not consulted -- two sessions can write to the same
  document concurrently.
- `doltCommit` triggers dsess's three-way merge with conflict detection.
- Conflicts are surfaced via `dolt_conflicts` (existing dsess machinery).
- `startTransaction` returns an error directing users to `doltCommit`.

### Lifecycle

1. **First request with new lsid** -> `SessionRegistry.GetOrCreate(lsid)`
   creates a fresh `dsess.DoltSession`.
2. **`startTransaction` (default mode)** -> `DoltSession.StartTransaction`
   snapshots the noms root for every db under management into
   `DoltTransaction.dbStartPoints` -- that snapshot hash is the fork point.
3. **First touch of a branch in a session** -> dsess lazily materializes the
   `branchState` on demand via `lookupDbState`. The working set is loaded from
   the working-set ref on disk; that is the session's view of the branch.
4. **Reads** -> dsess returns the `branchState`'s working set. If the session
   has writes on this branch, reads see them (read-your-own-writes). If the
   session has no writes here, the working set is the branch HEAD as of
   transaction start.
5. **Writes (insert/update/delete)**:
   - **Default mode, inside a Mongo txn**: resolve filter -> `_id` set,
     `DocLockManager.Acquire(lsid, db, branch, ids)`, then write via
     `dsess.WriteSession.GetTableWriter` -> `TableWriter`. Lock acquisition
     failure -> `WriteConflict` (112) immediately. Working set is updated;
     `branchState.dirty = true`.
   - **Default mode, no txn**: implicit single-statement transaction through
     dsess; no lock manager needed.
   - **`--session-isolation` mode**: write via dsess `TableWriter`. No lock
     manager. Conflicts are resolved at `doltCommit` time.
6. **`commitTransaction` (default mode)** -> `DoltSession.CommitTransaction`
   runs dsess's three-way merge against current HEAD, updates working set or
   creates a new dolt commit per `DoltCommitOnTransactionCommit`, then
   `DocLockManager.Release(lsid)`.
7. **`abortTransaction` (default mode)** -> `DoltSession.Rollback` discards
   the working set; `DocLockManager.Release(lsid)`.
8. **`doltCommit` (`--session-isolation` mode)** -> `DoltSession.DoltCommit`.
   Three-way merge against current HEAD. Conflicts surface via
   `dolt_conflicts`. On success, the session's fork point advances to the new
   HEAD and the working set is reset; on conflict, the working set stays
   dirty until resolved.
9. **Disconnect without commit** -> the session stays in the registry. A
   reconnect with the same `lsid` resumes it.
10. **Session timeout** -> registry discards the session; uncommitted working
    set is lost; locks are released. Configurable, default 30 min.
11. **`endSession`** -> explicit version of timeout.

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
sess := b.sessions.GetOrCreate(lsid)
sqlCtx := b.makeSQLContext(ctx, sess)         // shim *sql.Context

if inMongoTxn(ctx) {
    ids := resolveFilterToIDs(ctx, filter)    // for update/delete; trivial for insert
    if err := b.docLocks(db, branch).Acquire(lsid, collection, ids); err != nil {
        return writeConflictError(err)        // code 112
    }
}

state, _, _ := sess.LookupDbState(sqlCtx, qualifiedDbName)
tw, _ := state.WriteSession().GetTableWriter(sqlCtx, tableName, dbName, setter, false)
// ...call tw.Insert / tw.Update / tw.Delete...
// dsess marks branchState.dirty; nothing flushes to disk yet.
```

### Merge

Handled by dsess. See `DoltTransaction.doCommit` / `mergeRoots` in
`dolt/go/libraries/doltcore/sqle/dsess/transactions.go`. The same
`merge.MergeRoots` machinery used for `doltMerge` between branches drives
session merge-on-commit.

## Wire Protocol Surface

### WriteConcern

MongoDB's `writeConcern` lets clients choose per-write durability guarantees.
With session isolation, DumboDB simplifies this -- all regular writes are
memory-only, and commit is the single durability signal:

| Client request | DumboDB behavior |
|---|---|
| Regular write (insert/update/delete) | Writes to the session's working set in memory. Fast, not crash-safe. |
| `doltCommit` / `commitTransaction` | Merges session into branch, flushes to disk. Crash-safe. |

The MongoDB `writeConcern` parameter is accepted but effectively irrelevant --
individual writes are always memory-only regardless of what the client
requests. Commits are always crash-safe regardless of what the client
requests. This is a deliberate departure from MongoDB's model: DumboDB's
durability story is version-control-native, not per-write configurable.

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

**Reconnection:** If a client disconnects (network glitch, process restart)
and reconnects with the same `lsid`, DumboDB looks up the session in the
registry and resumes where the client left off -- uncommitted writes are
still there.

**Session timeout:** Sessions persist for a configurable period of inactivity
(default: 30 minutes). After timeout, the session state is discarded and
uncommitted changes are lost. Matches MongoDB's
`logicalSessionTimeoutMinutes`.

**`endSession` command:** Explicit client signal to discard session state.
Any uncommitted changes are lost.

**Session transfer:** The wire protocol allows two different connections to
share an `lsid`. This means two processes could collaborate on the same
uncommitted working set. We don't need to encourage this, but it falls out
naturally from keying on `lsid` rather than connection.

**Retryable writes (future):** MongoDB uses `lsid` + `txnNumber` to deduplicate
retried writes. If a client retries an insert after a timeout, the server
recognizes the `(lsid, txnNumber)` pair and returns the cached result instead
of double-inserting. Not required for Phase 1, but the session registry makes
it straightforward to add later.

### Non-Session Clients

Clients that don't send `lsid` (e.g., simple `mongosh` without explicit
sessions) get an implicit session scoped to their TCP connection. DumboDB
generates an internal session ID on connect, and discards it on disconnect.
Behavior is identical -- writes accumulate in the implicit session, commit
flushes.

### Session Security: Discovery and Hijacking

Since `lsid` is client-generated, a concern arises: can another user discover
and attach to someone else's session? In MongoDB, `$listSessions` and
`$listLocalSessions` aggregation stages enumerate active sessions (with admin
privileges). In standard MongoDB this is benign because sessions don't hold
uncommitted data. In DumboDB, where sessions hold uncommitted working sets,
session discovery would allow data access.

**Options (no decision required now):**

1. **Don't implement `$listSessions`** -- simplest first pass. Without a
   discovery mechanism, the `lsid` UUID (128 bits) is effectively unguessable.
   No enumeration = no hijacking vector. Likely the Phase 1 approach.
2. **Scope sessions to authenticated users** -- tie sessions to
   `(lsid, username)` so only the user who created the session can resume it.
   MongoDB already does this when auth is enabled. Right long-term answer, but
   DumboDB currently has **no authentication at all**, so this is deferred
   until auth lands.
3. **Server-generated session IDs** -- DumboDB ignores the client's `lsid` and
   generates its own, returning it to the client. More secure but breaks wire
   protocol expectations. Not recommended.

## Edge Cases

**Reading a branch you haven't written to:**
The `branchState` is created lazily on first touch. Reads from a clean
`branchState` return the branch HEAD's working set as of session start. No
data is forked; this is cheap.

**Reading a branch you HAVE written to:**
Reads come from the `branchState`'s working set -- you see your own
uncommitted changes, but not changes from other sessions that committed after
your fork point. To pick up other sessions' committed changes, either commit
(merge) or explicitly reset via a new transaction.

**Multiple commits in one session on the same branch:**
Each commit merges the session's working set into the branch HEAD, advances
the fork point to the new HEAD, and continues. The `branchState` stays alive
with a fresh fork point -- no need to reconnect. dsess handles this in
`commitBranchState`.

**Commit on one branch, dirty state on another:**
Each `branchState` is independent. Committing `mydb@feat` does not affect
the dirty state on `mydb@main`. The client can commit branches independently
(subject to dsess's "one dirty branch per transaction" rule -- see
`ErrDirtyWorkingSets`; we may need to extend dsess here for multi-branch
Mongo sessions).

**`doltBranch` / `doltCheckout` within a session:**
Creating a new branch doesn't affect existing `branchState`s. "Checking out"
a different branch (connecting via a different rootish) creates a new
`branchState` for that branch if the session writes to it.

**Concurrent commits from different sessions on the same branch:**
Serialized by `Provider().TxLocks()` keymutex in dsess. Second committer
merges against the first's result using three-way merge. In default mode,
locks prevented overlap so the merge is always clean. In `--session-isolation`
mode, conflicts surface via `dolt_conflicts`.

**`doltMerge` between branches within a session:**
If the session has dirty `branchState`s on both source and target, the merge
should require both to be committed first (or error). We don't want to merge
uncommitted state across branches.

## What This Enables

- **Fast writes** -- no fsync per operation, just in-memory working-set mutation
- **Session isolation** -- MVCC-like semantics without the complexity
- **Explicit durability** -- commit = "I'm done, make it permanent"
- **Conflict detection** -- two sessions editing the same doc get a clean error
- **Natural fit with version control** -- every `doltCommit` is a versioned
  checkpoint with message, author, and parent chain

## Open Behavioral Choices: Parity Tests, Not Design Decisions

Several semantic questions arise from this design where MongoDB's behavior is
either underspecified or has subtle variants. Rather than deciding these in
this document, we encode the decisions as parity tests in
[`dumbodb-parity-testing`](https://github.com/dolthub/dumbodb-parity-testing).
The harness runs the same operation against MongoDB 8 and DumboDB and compares
results, classified per test as:

- **`DumboDBFull`** -- divergence fails CI. Use when MongoDB's behavior is the
  contract we commit to matching.
- **`DumboDBXFail`** -- divergence recorded but not a CI failure. Use when we
  knowingly deviate (e.g., simpler/different semantics that we accept).
- **`DumboDBMongoOnly`** -- runs on MongoDB only. Use for features we have no
  intent to support.

The choices below get parity coverage instead of paragraphs of prose here:

| Question | How parity testing decides |
|---|---|
| Lock wait policy: no-wait (immediate `WriteConflict`) vs bounded-wait (`maxTransactionLockRequestTimeoutMillis`)? | Tests in default-mode probe two concurrent txns hitting the same `_id`. Whatever MongoDB returns is the oracle. If we ship no-wait while Mongo bounded-waits, mark `DumboDBXFail` until parity. |
| Filter -> `_id` set resolution for update/delete: how does Mongo behave when an updated doc set changes mid-txn? | Tests run filter-based updates against concurrently-modified docs and compare. |
| Snapshot semantics for reads inside a txn: read-your-own-writes plus snapshot of others' state at txn start. | Tests interleave reads/writes across two sessions and compare returned docs. |
| `writeConcern` honored vs ignored. | The design says we ignore it (everything is memory-only between commits). Tests assert this is acceptable -- almost certainly `DumboDBFull` since results match Mongo for `w:1`/`w:majority` on a standalone. |
| `endSession` semantics, especially with uncommitted state. | Tests call `endSession` mid-txn and verify behavior matches Mongo (abort + discard). |
| Retryable writes (`lsid` + `txnNumber` dedup). | `DumboDBMongoOnly` initially; promote to `DumboDBFull` once implemented. |

The harness keeps these honest as we evolve. The design doc does not have to
guess.

## Correctness Testing

Session isolation introduces concurrency and merge semantics that don't exist
in DumboDB today. The scenarios below should be covered by integration tests
in `dumbodb` and parity tests in `dumbodb-parity-testing`, the latter using
`DumboDBFull` / `DumboDBXFail` / `DumboDBMongoOnly` to classify each case
per the table in the previous section.

Tests 1-12 cover the `--session-isolation` mode flow. Tests 13-17 cover the
default-mode (pessimistic transaction) flow.

### Test 1: Uncommitted writes invisible to read-only sessions

Two clients connect to the same branch. Client A inserts a document. Client B
(read-only, has not written) reads the collection. Since A hasn't committed,
B should not see A's document. After A commits, B reads from the updated
branch HEAD and sees the document.

```
Client A: insert {_id: 1, x: "from A"}            <- A's branchState is now dirty
Client B: find {} -> should return [] (empty)     <- B's branchState is clean, reads HEAD
Client A: doltCommit                              <- A's insert merges into branch HEAD
Client B: find {} -> should return [{_id: 1}]     <- B re-reads HEAD on next transaction
```

### Test 2: Dirty session is pinned to fork point

When Client B has its own uncommitted writes, it reads from its working set,
which was forked at B's transaction start. B does NOT see commits made by A
after B's fork point -- until B commits (merges).

```
Client A: insert {_id: 1, x: "from A"}
Client B: insert {_id: 2, x: "from B"}            <- B forks at current HEAD
Client A: doltCommit                              <- branch HEAD advances
Client B: find {} -> should return [{_id: 2}]     <- B sees own write, NOT A's commit
Client B: doltCommit                              <- three-way merge picks up both
Client B: find {} -> should return [{_id: 1}, {_id: 2}]
```

### Test 3: Read-your-own-writes

A single client inserts a document and reads it back within the same session,
before committing.

```
Client A: insert {_id: 1, x: "hello"}
Client A: find {_id: 1} -> [{_id: 1, x: "hello"}]   <- reads from working set
Client A: doltCommit
Client A: find {_id: 1} -> [{_id: 1, x: "hello"}]
```

### Test 4: Non-conflicting concurrent writes merge cleanly

Two clients write to different documents on the same branch, then both commit.
No conflicts -- three-way merge combines both sets of changes.

```
Client A: insert {_id: 1, x: "from A"}            <- A forks from HEAD C0
Client B: insert {_id: 2, x: "from B"}            <- B forks from HEAD C0
Client A: find {} -> [{_id: 1}]                   <- A sees own write only
Client B: find {} -> [{_id: 2}]                   <- B sees own write only
Client A: doltCommit -> succeeds                  <- HEAD advances to C1
Client A: find {} -> [{_id: 1}, {_id: 2}]         <- A's fork resets to C1; sees both
Client B: find {} -> [{_id: 2}]                   <- B still pinned to C0
Client B: doltCommit -> succeeds                  <- merge(base=C0, ours=B, theirs=C1)
Client B: find {} -> [{_id: 1}, {_id: 2}]
```

### Test 5: After commit, session returns to read-only and sees new commits

After committing, a session's `branchState` resets to the new HEAD. Subsequent
reads pick up any commits made by other sessions in the meantime.

```
Client A: insert {_id: 1, x: "from A"}
Client A: doltCommit -> succeeds                  <- HEAD advances to C1
Client B: insert {_id: 2, x: "from B"}
Client B: doltCommit -> succeeds                  <- HEAD advances to C2
Client A: find {} -> [{_id: 1}, {_id: 2}]         <- A re-reads HEAD (C2)
Client B: find {} -> [{_id: 1}, {_id: 2}]
```

### Test 6: Conflicting writes produce a conflict

Two clients modify the same document on the same branch, then both commit.
The second committer should get a merge conflict.

```
Setup: collection has {_id: 1, x: "original"}
Client A: update {_id: 1}, {$set: {x: "A's version"}}
Client B: update {_id: 1}, {$set: {x: "B's version"}}
Client A: doltCommit -> succeeds
Client B: doltCommit -> conflict error
```

### Test 7: Conflict resolution and continue

Continues from Test 6. After a conflict, the session can resolve and
re-commit.

```
Client B: doltCommit -> conflict on _id:1
Client B: doltConflicts -> shows (base, ours, theirs) versions
Client B: doltResolveConflict (choose "ours" or "theirs" or manual)
Client B: doltCommit -> succeeds
```

### Test 8: Abandoned session cleanup

A client writes without committing, then disconnects. The write never reached
the branch HEAD, so no other client should see it. After timeout, the session
registry releases the entry.

```
Client A: insert {_id: 99, x: "never committed"}
Client A: disconnect (no doltCommit)
Client B: find {_id: 99} -> [] (empty)
Wait for session timeout
Verify: SessionRegistry no longer holds A's DoltSession
```

### Test 9: Multi-branch isolation within one session

A single client writes to two branches in the same session. Each branch has
its own `branchState`. Commits are independent.

```
Client A on mydb (main): insert {_id: 1, x: "main write"}
Client A on mydb@feat:   insert {_id: 2, x: "feat write"}
Client A on mydb@feat:   doltCommit -> succeeds (only feat)
Client B on mydb (main): find {} -> should NOT see {_id: 1}
Client B on mydb@feat:   find {} -> should see {_id: 2}
Client A on mydb (main): doltCommit -> succeeds
Client B on mydb (main): find {} -> now sees {_id: 1}
```

### Test 10: Session reconnection preserves uncommitted state

A client writes, disconnects, reconnects with the same lsid, and continues
working with the same `branchState`.

```
Client A (lsid: ABC): insert {_id: 1, x: "before disconnect"}
Client A: disconnect
Client A (lsid: ABC): reconnect
Client A: find {_id: 1} -> [{_id: 1, x: "before disconnect"}]
Client A: doltCommit -> succeeds
```

### Test 11: Concurrent commits -- serialization order

Three clients all fork from the same branch HEAD. Each commit merges against
the result of the previous commit (the current HEAD at commit time), not
against the original fork point. The keymutex in dsess serializes them.

```
All three fork from branch HEAD C0
Client A: insert {_id: 1}
Client B: insert {_id: 2}
Client C: insert {_id: 3}
Client A: doltCommit -> merge(base=C0, ours=A, theirs=C0) -> HEAD is C1
Client B: doltCommit -> merge(base=C0, ours=B, theirs=C1) -> HEAD is C2
Client C: doltCommit -> merge(base=C0, ours=C, theirs=C2) -> HEAD is C3
Final state: all three documents present
```

### Test 12: Delete + modify conflict

One client deletes a document, another modifies it. The second committer
should get a conflict.

```
Setup: collection has {_id: 1, x: "original"}
Client A: delete {_id: 1}
Client B: update {_id: 1}, {$set: {x: "modified"}}
Client A: doltCommit -> succeeds (_id:1 removed from HEAD)
Client B: doltCommit -> conflict (base had _id:1, A deleted it, B modified it)
```

### Test 13: Basic startTransaction / commitTransaction (default mode)

```
Client A: startTransaction
Client A: insert {_id: 1, x: "in txn"}
Client B: find {} -> should return [] (A hasn't committed)
Client A: commitTransaction
Client B: find {} -> should return [{_id: 1, x: "in txn"}]
```

### Test 14: abortTransaction discards changes (default mode)

```
Client A: startTransaction
Client A: insert {_id: 1, x: "will abort"}
Client A: find {_id: 1} -> [{_id: 1}] (read-your-own-writes)
Client A: abortTransaction
Client A: find {_id: 1} -> []
```

### Test 15: Document locking -- concurrent txn blocked on same doc

```
Setup: collection has {_id: 1, x: "original"}
Client A: startTransaction
Client A: update {_id: 1}, {$set: {x: "A"}}     <- acquires lock on _id:1
Client B: startTransaction
Client B: update {_id: 1}, {$set: {x: "B"}}     <- WriteConflict (doc locked by A)
Client A: commitTransaction                       <- releases lock
Client B: (retries) update {_id: 1} -> succeeds
Client B: commitTransaction
```

### Test 16: Non-conflicting transactions succeed

```
Client A: startTransaction
Client A: insert {_id: 1}                         <- locks _id:1
Client B: startTransaction
Client B: insert {_id: 2}                         <- locks _id:2 (no conflict)
Client A: commitTransaction
Client B: commitTransaction
Both documents present.
```

### Test 17: startTransaction rejected in --session-isolation mode

```
Server started with --session-isolation
Client A: startTransaction -> error: "Transactions are not available in
  session-isolation mode. Use doltCommit instead."
```

## Migration Path

1. **Phase 0 -- storage shape.** Decide how Mongo collections map onto Dolt's
   `RootValue` (each collection as a table vs an internal collections registry
   table). dsess does not constrain this; downstream merge behavior does.
2. **Phase 1 -- dsess integration.** Wire DumboDB's backend through
   `dsess.DoltSession`. Make every Mongo command construct a `*sql.Context`
   and call into dsess's `LookupDbState` / `WriteSession.GetTableWriter`.
   `doltCommit` becomes `DoltSession.DoltCommit`. At this phase, sessions are
   still per-connection (no lsid registry yet).
3. **Phase 2 -- `dsess.SessionRegistry` (upstream).** Add the lsid-keyed
   registry to dsess with timeout, reconnect, and GC-safepoint integration.
   Switch DumboDB's session lookup from per-connection to per-lsid.
4. **Phase 3 -- `DocLockManager`.** Implement pessimistic per-doc locking in
   DumboDB. Wire `startTransaction` / `commitTransaction` / `abortTransaction`
   commands through it.
5. **Phase 4 -- `--session-isolation` mode flag.** Gate `DocLockManager` and
   the `startTransaction` rejection on the flag.
6. **Phase 5 -- parity coverage.** Backfill `dumbodb-parity-testing` for the
   behavioral questions enumerated above. Promote `DumboDBXFail` cases to
   `DumboDBFull` as we close gaps.

## Files to Change

### In dumbodb

| File | Change |
|---|---|
| `backend.go` | Hold a `*SessionRegistry` and a `DocLockManager` per (db, branch). Drop direct `dbState.am` mutation. |
| `backend.go` (DumboDBCommit) | Delegate to `dsess.DoltSession.DoltCommit`. |
| `collection.go` | `InsertAll`/`UpdateAll`/`DeleteAll` go through `dsess.WriteSession.GetTableWriter` -> `TableWriter`. Acquire/release `DocLockManager` when inside a Mongo txn. |
| `helpers.go` | Replace `updateAddressMap` with dsess working-set updates. |
| `conninfo/conn_info.go` | Thread lsid through context. |
| `handler/common/insert.go` | Stop ignoring `lsid`; pass to backend. |
| `handler/common/update_params.go` | Same. |
| `handler/common/delete_params.go` | Same. |
| `handler/msg_session.go` | Implement `startSession`, `endSession` against `SessionRegistry`. |
| `handler/msg_transaction.go` | NEW: `startTransaction`, `commitTransaction`, `abortTransaction`. In default mode wires `DocLockManager`; in `--session-isolation` mode returns the redirect error. |
| `cmd/dumbodb/main.go` | Add `--session-isolation` flag. |
| `internal/sqlctx/...` | NEW: shim that constructs `*sql.Context` / `sql.Session` around a `dsess.DoltSession` so the Mongo handler can call into dsess. |

### In dolt (upstream proposals)

| File | Change |
|---|---|
| `go/libraries/doltcore/sqle/dsess/session_registry.go` | NEW: `SessionRegistry` keyed by external session ID, with timeout, reconnect, and GC-safepoint integration. |
| `go/libraries/doltcore/sqle/dsess/session.go` | Decouple `DoltSession` creation from `sql.BaseSession` enough that the registry can manage its lifecycle independently of a SQL connection. |

### In dumbodb-parity-testing

| File | Change |
|---|---|
| `tests/transaction_test.go` | NEW: coverage for `startTransaction` / `commitTransaction` / `abortTransaction`, document-lock conflicts, and the behavioral questions listed in "Open Behavioral Choices". |
| `tests/session_test.go` | NEW: coverage for `lsid` reconnect semantics, `endSession`, session timeout. |
