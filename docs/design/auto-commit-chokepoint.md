# Auto-Commit, Modeled on Dolt's Own Implementation

Tracking issue: workspace-di7

## Problem

`--auto-commit` promises "a Dolt commit for every write." Today it delivers
that only for DML. The three DML paths (`InsertAll`, `UpdateAll`, `DeleteAll`)
each call `commitCollectionsAMAs` after updating the working set; every DDL
path (`CreateCollection`, `DropCollection`, `RenameCollection`,
`CreateIndexes`, `DropIndexes`) advances the working root but issues no commit.
The DDL change sits uncommitted until the next DML write sweeps it into that
write's commit.

Observed (one root cause): mongo-express's create-database flow makes a
throwaway `delete_me` collection and drops it; the drop coalesced into the
following `orders.json` import commit. And `createIndex` produced no commit at
all.

## How Dolt actually does auto-commit (the reference)

Studied in the Dolt source (`/workspace/dolt/go`, matching the version dumbodb
vendors, `v0.40.5-0.2026...e239b95`). Two distinct concepts:

- `@@autocommit` (MySQL semantics): persist the working set at the end of each
  statement. No history commit.
- `@@dolt_transaction_commit` (`dsess.DoltCommitOnTransactionCommit`): create a
  Dolt *history* commit at each transaction commit. **This is exactly what
  dumbodb's `--auto-commit` means.**

The reference implementation has four properties worth copying verbatim:

1. **One chokepoint: transaction commit.** `DoltSession.CommitTransaction`
   (sqle/dsess/session.go:446) is the single place that decides -- it reads the
   dirty working sets and, if `dolt_transaction_commit` is on, builds a Dolt
   commit; otherwise it just writes the working set. The commit is never
   sprinkled into the INSERT/UPDATE/DELETE operators; it is driven from this one
   path.

   The subtlety is what counts as a "transaction." With `@@autocommit` on -- the
   default, meaning no open `BEGIN` -- go-mysql-server wraps EACH statement in
   its own implicit transaction and commits it when that statement's iterator
   closes (`TransactionCommittingIter.Close`, gms
   sql/rowexec/transaction_iters.go:113). So a single INSERT/UPDATE/DELETE run
   outside an explicit `BEGIN ... COMMIT`, with `dolt_transaction_commit` set,
   DOES produce a Dolt commit -- one statement, one implicit transaction, one
   commit. An explicit `BEGIN ... COMMIT` is what batches several statements
   into a single transaction and therefore a single commit. Either way the
   commit fires from `CommitTransaction`, at transaction end.

   This is the behavior dumbodb's `--auto-commit` wants: a lone write commits on
   its own, and the commit decision lives at one boundary, not in each writer.

2. **The working-set update and the commit are ONE atomic write.**
   `doltCommit` (sqle/dsess/transactions.go:190) is a `transactionWrite` that
   ends in a single call:

       doltDb.CommitWithWorkingSet(ctx, headRef, workingSet.Ref(),
           &pending, workingSet, currHash, meta, &rsc)

   `CommitWithWorkingSet` (doltdb/doltdb.go:1755) installs the new working set
   AND advances HEAD together, guarded by `currHash` -- the SAME working-set
   hash optimistic lock that plain `UpdateWorkingSet` uses. There is no
   "commit, then separately rewrite the working set."

3. **A `PendingCommit` carries the root to commit; the tree is clean by
   construction.** `NewPendingCommit` (doltdb/commit.go:300) packages
   `roots.Staged` as the commit's root value. The `workingSet` handed to
   `CommitWithWorkingSet` already has working == staged == the committed root,
   so after the one write the branch is clean. No follow-up reset.

4. **Empty-commit guard is structural.** In `CommitTransaction`, if nothing is
   staged, `PendingCommitAllStaged` returns nil and it falls back to a
   working-set-only write. No empty commits, no special-casing no-op writes.

Also: Dolt allows only ONE dirty branch per transaction commit
(`ErrDirtyWorkingSets`, session.go:466). Multi-branch-in-one-commit is a
non-goal there.

## Where the first attempt went wrong

The initial dumbodb design (still in the working tree as of this writing)
inverted Dolt's structure:

- It hooked auto-commit *per write* inside `updateWorkingRoot`, then re-coalesced
  multi-write commands with a bolted-on, command-scoped "batch" of deferred
  closures hung on `ConnInfo`, plus per-handler `Begin/Flush` wrapping of six
  handlers.
- It committed with `commitCollectionsAMAs` (`datas.Database.Commit`) as a write
  SEPARATE from the working-set update, then did a THIRD write (`persistAM`) to
  reset the tree clean. Three non-atomic writes where Dolt does one; a crash
  between them leaves history and working set inconsistent (the old DML code
  even carried a comment admitting this).

It worked and passed tests, but it reimplemented -- badly -- what
`CommitWithWorkingSet` already does atomically, and it scattered the decision
across the write paths and handlers instead of the one boundary Dolt uses.

## Revised design (mirror the reference)

### Chokepoint: the command boundary

dumbodb processes one wire command at a time per connection, inside
`dispatchThroughSession` (clientconn/conn.go:601), which runs the handler within
`shadow.Use` / `shadow.Commit`. **That is dumbodb's `CommitTransaction`.** One
wire command == one implicit transaction.

After the handler returns, if `--auto-commit` is on and the connection's branch
working root differs from HEAD, commit that branch once. Nothing commits
mid-command. This single hook replaces: the per-write hook in
`updateWorkingRoot`, the `ConnInfo` deferred-closure batch, and all six
per-handler `Begin/Flush` wrappers.

Consequences fall out for free, no special code:
- bulkWrite: all its writes accumulate in the working set; the boundary commits
  once. One commit.
- Insert into a not-yet-materialized collection: the implicit create and the
  insert are both in the working set at the boundary. One commit.
- Every DDL command (drop, createIndex, rename): dirties the root, commits at
  its boundary. The reported bug is fixed by construction.
- A no-op write leaves working == HEAD; the guard skips it.

### Atomic primitive: `commitBranchWS`

Add a sibling to `updateBranchWS` in branch_ws.go that commits instead of just
updating. It holds the same `entry.mu`, reads the current `entry.wsHash` as the
optimistic lock, builds a `PendingCommit` (`Staged` = the new working root,
`Head` = current HEAD root), hands a clean working set (working == staged ==
new root) to `doltDB.CommitWithWorkingSet`, and refreshes `entry.ws` /
`entry.wsHash` from disk afterward -- exactly the shape of the existing
`updateBranchWS`, one atomic write. Commits land on the connection's own branch
ref (`refs/heads/<branch>`), fixing today's always-`main` scoping bug.

This deletes `commitCollectionsAMAs`-for-auto-commit and the `persistAM` reset;
`CommitWithWorkingSet` does both jobs in one write and leaves the tree clean.

### Commit messages

Dolt does not give us per-statement messages (it uses a fixed "Transaction
commit" or the `dolt_transaction_commit_message` var). dumbodb should do better:
**pack as much information as the command has into the message.** The write path
records a proposed message and the boundary uses it verbatim.

The message should carry the operation, the collection, and any cheaply
available specifics -- affected index names, old/new names for a rename, the
count of documents written -- e.g.:

    auto: insert 3 docs into orders
    auto: drop collection delete_me
    auto: create index category_1 on products
    auto: rename collection old to new

For a command that performs several heterogeneous writes (bulkWrite), the
message summarizes the whole command (counts of inserts/updates/deletes), not a
bare `auto: bulkWrite`. The exact wording is an implementation detail; the rule
is maximal useful detail on one line.

## Decisions and constraints

- **Never produce a double commit.** A single logical operation results in at
  most one Dolt commit. The boundary only commits when the branch's working root
  actually differs from HEAD, so a command that already advanced history itself
  (see below) leaves a clean tree and the boundary adds nothing.

- **`--auto-commit` and `--session-isolation` are mutually exclusive.** The
  server must refuse to start when both flags are set (fail fast with a clear
  error), rather than trying to reconcile two different commit-ownership models.
  This removes the "which path owns the boundary" question entirely: auto-commit
  only ever runs on the non-session-isolation path.

- **Real multi-statement transactions still own their own boundary.** When a
  client transaction is open, the existing session commit path commits at the
  client's COMMIT; the per-command auto-commit hook does not fire mid
  transaction. (`dispatchThroughSession` already distinguishes InTransaction.)

- **Version-control commands: audited, no per-command special-casing needed**
  (workspace-qbq). Reads (log, diff, status, branchStatus, conflicts) never
  touch the root. The history-advancing ones (commit, merge, cherry-pick,
  revert, rebase) create their own commits via their own paths and reset the
  tree clean; reset/branch/tag/undrop/gc create no data commit. Crucially,
  EVERY VC command bypasses `updateWorkingRoot`, so none records a touched
  branch and the boundary never fires for them -- double commits are excluded
  structurally, not by per-command checks. Explicit `doltCommit` under
  auto-commit is redundant (the tree is already clean between commands) and
  harmless.

- **Conflict resolution disables auto-commit until `--continue`.** While a
  merge / cherry-pick / revert / rebase is paused on conflict
  (`dbState.mergeState != nil` for the branch), the command boundary must NOT
  auto-commit that branch, and explicit `doltCommit` is likewise refused. Writes
  are NOT blocked: resolving a conflict requires editing the conflicting
  documents, and arbitrary edits may be needed too -- mid-conflict, all bets are
  off. Those edits flow into the working set and accumulate uncommitted. The
  operation's `--continue` is the sole commit point and captures the whole
  accumulated working set (resolutions plus any other edits) as one commit;
  `--abort` discards it (resets to the pre-operation state). Applies to all four
  conflict-producing operations. Plumbing already exists: `mergeState` is
  tracked per-branch (`intoBranch`) and persisted to disk, so the boundary and
  the commit guard just consult `mergeState != nil && intoBranch == branch`.
  Tracked as workspace-hb9.6.

## Verification plan (expansive)

The invariant under test everywhere: a write command produces **exactly one**
Dolt commit when it changes data, **zero** when it does not, and **never two**.
Every case asserts the commit-count delta and the message, on the connection's
branch. Grouped by test task below.

### Group 1 -- single-command write matrix (task hb9.5)

A. DML, each command exactly one commit, message names op + collection (+counts):
- insert one doc; insert many docs in one command (spanning internal batch
  chunks); insert into an existing collection.
- update `$set` single; update `multi:true` (many docs); replacement update.
- delete one; delete many.
- findAndModify: update, remove, upsert-insert, upsert-match.

B. No-op writes -> zero commits (empty-commit guard):
- update matching nothing; delete matching nothing; findAndModify no-match
  no-upsert; bulkWrite whose ops all match nothing.

C. Implicit creation folds into one commit:
- insert into a not-yet-materialized collection -> one commit (not create +
  insert); insert into a brand-new database (assert the db-init commit
  explicitly, then one write commit); createIndex on a collection with no
  primary map yet -> one commit.

D. Bulk / aggregation, one command one commit:
- bulkWrite: N inserts; mixed insert/update/delete; ordered stop-on-error
  (commits only what landed); unordered partial failure; multiple collections in
  one db (one commit); (edge) multiple dbs (one commit per db).
- aggregate `$out`: into new collection; replacing existing; zero results
  (empty collection created).
- aggregate `$merge`: whenNotMatched insert; whenMatched merge/replace.

E. DDL, each command one commit, specific message:
- createCollection (plain; capped; with validator); dropCollection;
  renameCollection (message old -> new); createIndexes (one; several in one
  command -> message lists names); dropIndexes (one; several; by name).

F. Message content: op + collection present; counts where cheap
  (`insert 3 docs`); index names; rename old/new; bulkWrite summarizes
  insert/update/delete counts.

G. Branch scoping: a write on a non-main branch commits to THAT branch, not
  main; two connections on different branches each commit to their own branch.

H. Reported-bug regression: create(delete_me) -> drop(delete_me) -> import =
  three distinct commits.

### Group 2 -- conflict-resolution workflows (task hb9.7)

For EACH of merge, cherry-pick, revert, rebase:
- clean (no conflict): exactly one commit (the op's own); boundary adds nothing.
- conflicting run:
  - the op pauses; no commit is created.
  - edit a conflicting doc -> no auto-commit.
  - edit an unrelated doc -> no auto-commit.
  - resolveConflict -> no commit.
  - explicit doltCommit while paused -> refused.
  - `--continue` -> exactly one commit that CONTAINS the resolutions AND the
    unrelated edits.
  - (separate run) `--abort` -> working set reset to pre-op; edits discarded; no
    commit.
  - after continue/abort, a subsequent ordinary write auto-commits again
    (mergeState cleared, guard released).
- rebase multi-commit: pause/resolve/continue repeats across several replayed
  commits; no auto-commit until the final continue.

### Group 3 -- VC interaction, startup, persistence (task hb9.8)

I. VC commands never double-commit:
- reads (log/status/diff/branchStatus/conflicts): no commit.
- self-contained history ops (commit / clean merge / clean cherry-pick / revert
  / clean rebase): exactly their own commit, never a second boundary commit.
- reset: moves to an existing commit, no spurious new commit.
- explicit doltCommit on an already-clean tree: "nothing to commit".
- branch / tag / undrop / gc: no data commit.

J. Startup guard: `--auto-commit` + `--session-isolation` -> refuses to start
  with a clear error; `--auto-commit` alone -> starts.

K. Persistence & atomicity invariants:
- after every committing command, the working tree is clean (working == HEAD).
- mergeState survives a server restart: a conflict paused before restart still
  suppresses auto-commit after restart until `--continue`.
- (by construction, not a unit test) the single `CommitWithWorkingSet` write
  leaves no window where the working set moved but HEAD did not.
