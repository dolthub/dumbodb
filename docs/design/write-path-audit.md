# Write Path Audit

**Question:** does every write reach the merge, and are any errors dropped on
the way?

**Answer:** no, and yes. One condition decides it, and four ways of failing it
send a write down a path where no merge runs.

## 1. There is one choke point

Every data mutation reaches `updateWorkingRoot`
(`internal/backends/dolt/helpers.go`). Documents, DDL, views and the catalog
all funnel through `updateAddressMap` / `updateAddressMapWithSync`, and both
call it. Twelve call sites, one destination:

- `insert`, `update`, `delete` (`collection.go`)
- `create` / `drop` / `rename` collection, `createView`, `collMod`
  (`database.go`)
- collection catalog writes (`collection_catalog.go`)

That is good news. Routing writes through a merge is one decision in one
function, not twelve.

## 2. The condition, and the four ways to miss it

`updateWorkingRoot` keeps the write on the session -- to be reconciled at
commit -- only when all four hold:

```go
sess != nil &&
    sess.GetTransaction() != nil &&
    dbNameDsessFriendly(state.name) &&
    !alwaysAutoCommit(state.name)
```

Otherwise it publishes a complete working set built from a snapshot taken
before any lock, ignoring whatever is currently there. No base, no three-way
merge.

| conjunct | fails when | who lands there |
|---|---|---|
| `sess.GetTransaction() != nil` | nothing started a transaction | **every ordinary write.** Nothing starts one except an explicit `commitTransaction` flow or `--session-isolation` |
| `dbNameDsessFriendly(name)` | the **base** name contains `/` or `@` | only a database literally named with `@` and an all-digit suffix (see below) |
| `!alwaysAutoCommit(name)` | the name is `admin` | anything writing through the `admin` database |
| `sess != nil` | no session on the context | background loops: capped-collection cleanup, GC |

The first row is the one the compare-and-swap defect rides on.

The second row is narrower than it looks and needs stating carefully.
`dbNameDsessFriendly` is applied to `state.name`, which is the **base**
database name: `splitEncodedDBName` has already removed the rootish suffix, and
`dbState.name` is documented as the directory name without it. So a write
addressed to `mydb@feature` is checked as `mydb` and takes the session route
normally. The check only fails when the base name itself contains `@` or `/`,
which `splitEncodedDBName` produces deliberately for an all-digit suffix -- a
database named `prefix@1775505756999075683` stays one name rather than being
misparsed as a branch. That is a guard against dsess misreading a name, not a
merge bypass, and the population it affects is databases whose names look like
that.

## 3. Errors dropped or ignored

- **`backend.go`, branch creation.** `_ = updateWorkingSet(ctx, db.doltDB,
  emptyWS, params.Name)` -- the error is explicitly discarded. If persisting a
  new branch's working set fails, the branch exists with no working set and
  nothing says so.
- **`backend.go`, first open of a database.** A failed initial working-set
  persist is logged at warn and execution continues.
- **`updateWorkingRoot`, `skipSync`.** The parameter is discarded on the
  non-transactional path (`_ = skipSync`). A caller asking to defer the sync
  silently does not get it. The comment explains the reason -- there is no
  flusher to drain a cache-only write -- so the behaviour is deliberate, but
  the signature still advertises a choice the callee does not honour.
- **The commit-time publish** passes a prevHash re-resolved from disk
  immediately before writing, so its compare-and-swap cannot fail. See
  `working-set-publish.md`.

## 4. What the merge sees today

Two paths, two granularities, neither chosen:

- **Branch merge** (`dumboMerge`, replay commands) reaches
  `captureConflictsForCollection`, which now consults the collection's merge
  mode for every three-way row decision. Default `fieldTouched`.
- **Commit-time merge** (`mergePendingIntoCommitted`) still calls
  `merge.MergeRoots` with no policy, so a transaction reconciling against the
  tip compares whole documents.

So the mode governs branch merges and does not yet govern the path a write
takes when it does reach a transaction.

## 5. Consequences to fix, in order

1. **Start a transaction for every write**, so the first conjunct holds. This
   is the prerequisite for a merge mode to affect ordinary traffic at all;
   measured, the mode changes nothing without it.
2. **Make branch-qualified writes take the same path.** `dbNameDsessFriendly`
   excluding `@` means the branch-addressed writes the parity suite depends on
   never merge.
3. **Give the commit-time merge the same policy** the branch merge now has,
   so one rule decides conflicts wherever a merge runs.
4. **Stop dropping the errors in section 3**, and either honour `skipSync` or
   remove it from the signature.
