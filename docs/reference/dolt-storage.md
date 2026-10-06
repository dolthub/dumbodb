# Dolt Storage

Verified against the Dolt version pinned in `go.mod`:
`github.com/dolthub/dolt/go v0.40.5-0.20260924184407-4a2e8ce2f155`. Paths
below are relative to that module (`$(go env GOMODCACHE)/github.com/dolthub/dolt/go@<version>/`).
When the pinned version changes, check this document against the new source.

## Serialized objects

Every Dolt object is a chunk addressed by its hash. Modern objects are
flatbuffer messages. The schemas are in `serial/*.fbs` and the generated Go is
in `gen/fb/serial/`.

Message layout (`gen/fb/serial/fileidentifiers.go`, `FinishMessage`):
- One kind byte (`MessageTypesKind`) and a 3-byte big-endian size come first
  (`MessagePrefixSz` = 4).
- The flatbuffer follows, carrying a 4-character file identifier.
- `serial.GetFileID(bytes)` reads the identifier, and readers check it before
  they decode.

File identifiers of the types DumboDB uses:

| ID | Type | Purpose |
|---|---|---|
| `STRT` | `StoreRoot` | Root of the whole store: an embedded AddressMap from dataset ID to address |
| `WRST` | `WorkingSet` | A branch's working and staged roots, plus merge/rebase state |
| `DCMT` | `Commit` | Root value address, parents, height, author/committer metadata |
| `DTAG` | `Tag` | Tag pointing at a commit |
| `RTVL` | `RootValue` | Feature version and the table AddressMap |
| `DTBL` | `Table` | Schema address, primary index, secondary indexes, artifacts |
| `DSCH` | `TableSchema` | Table schema |
| `ADRM` | `AddressMap` | Prolly-tree node mapping string keys to chunk addresses |
| `TUPM` | `ProllyTreeNode` | Prolly-tree node of key/value tuples (row data and index data) |
| `BLOB` | `Blob` | Chunked byte tree for out-of-band values |
| `ARTM` | `MergeArtifacts` | Artifact map node (`prolly.ArtifactMap`); conflicts use `ArtifactTypeConflict` |

### Chunk graph

```
StoreRoot (STRT)
  address_map: dataset ID -> address
    "refs/heads/<branch>"         -> Commit (DCMT)
    "workingSets/heads/<branch>"  -> WorkingSet (WRST)
    "refs/tags/<tag>"             -> Tag (DTAG)

Commit (DCMT)        root -> RootValue
WorkingSet (WRST)    working_root_addr, staged_root_addr -> RootValue
RootValue (RTVL)     tables: AddressMap (table name -> Table)
Table (DTBL)         schema -> TableSchema (DSCH)
                     primary_index: embedded prolly map root (TUPM)
                     secondary_indexes: embedded AddressMap (index name -> index map)
                     artifacts -> ArtifactMap (ARTM)
```

- A working-set dataset ID is `workingSets/` followed by the head ref path
  without `refs/`. For example, branch `main` has `workingSets/heads/main`
  (`libraries/doltcore/ref/workingset_ref.go`).
- A `WorkingSet` stores root value addresses, not the root values themselves.
- In a Table, `primary_index` and `secondary_indexes` are embedded bytes, not
  addresses: the root node of each map is stored inline in the Table message.
- `durable.TableFromAddr` (`libraries/doltcore/doltdb/durable/table.go`)
  requires the addressed chunk to be a `DTBL` message. Any other file ID fails
  with "table ref is unexpected noms value; GetFileID == <id>". Dolt code that
  walks a RootValue's table map (status, diff, merge) therefore expects every
  value in that map to be a Table.

### Prolly trees

`ProllyTreeNode` and `AddressMap` are both prolly-tree nodes (`serial/prolly.fbs`,
`serial/addressmap.fbs`). Each node holds:
- sorted key items with their offsets;
- either value items (leaf nodes) or child addresses (internal nodes);
- varint subtree counts;
- a total `tree_count` and a `tree_level`, where 0 means a leaf.

`ProllyTreeNode` also records `value_address_offsets` and `key_address_offsets`.
These mark tuple fields that hold addresses of out-of-band values, so chunk
walkers such as GC can find them.

## Chunk stores

`chunks.ChunkStore` (`store/chunks/chunk_store.go`) is the storage interface:
- `Get`/`Has`/`Put` work on chunks.
- `Root` returns the persisted root hash as of open or the last `Rebase`.
- `Commit(current, last)` persists novel chunks and moves the root (see
  below).

`NomsBlockStore` (`store/nbs/store.go`) is the on-disk implementation. It
keeps a memtable of novel chunks and a set of table files, which are listed in
a manifest along with the root hash.

`GenerationalNBS` (`store/nbs/generational_chunk_store.go`) combines two
`NomsBlockStore`s and an optional ghost store:
- `Get` and `Has` try `newGen`, then `oldGen`, then `ghostGen`.
- `Put` always writes to `newGen`.
- `Root` and `Commit` delegate to `newGen`.

## Optimistic locking

Dolt has a single piece of mutable state: the store root. Every ref update is
an optimistic compare-and-swap at two levels.

### Level 1: store root CAS (chunk store)

`ChunkStore.Commit(current, last)` atomically persists all novel chunks and
moves the root from `last` to `current`. If the persisted root is not `last`,
it returns `false`.

`NomsBlockStore.commit` (`store/nbs/store.go`):
- It holds the in-process `nbs.mu` for the whole commit.
- It flushes the memtable to a table file, then calls `manifest.Update` with
  the lock hash it last saw. The manifest lock hash is computed from the root
  plus the table specs (`generateLockHash`).
- If another writer changed the manifest (the lock hash differs), the store
  rebases onto the new manifest. Then:
  - If the root moved (`errOptimisticLockFailedRoot`, or
    `errLastRootMismatch` when `last` is already stale), `Commit` returns
    `false`.
  - If only the set of table files changed and the root did not
    (`errOptimisticLockFailedTables`), `commit` retries internally.
- Before publishing, it refuses a new root that has dangling references
  (`errorIfDangling`).

### Level 2: dataset CAS (datas.Database)

`datas.database.update` (`store/datas/database_common.go`) is the only way to
change the StoreRoot map. It loops:

1. Read the current root.
2. Load the dataset map.
3. Apply an edit function.
4. Write the new `StoreRoot`.
5. Call `ChunkStore.Commit(newRoot, oldRoot)` through `tryCommitChunks`.

If step 5 fails because the root moved, `update` re-reads the root and
re-applies the edit, so concurrent updates to *other* datasets never reach the
caller. If the edit function returns an error, `update` returns it at once
without retrying.

The per-dataset checks happen inside that edit function, against the freshly
read map. They are the errors a caller can actually see:

| Method | Precondition checked inside `update` | Error on mismatch |
|---|---|---|
| `UpdateWorkingSet(ds, spec, prevHash)` | Current working-set entry == `prevHash` | `ErrOptimisticLockFailed` |
| `CommitWithWorkingSet(commitDS, wsDS, ..., prevWsHash, ...)` | Working-set entry == `prevWsHash`, and branch head == the head the caller read | `ErrOptimisticLockFailed` for the working set, `ErrMergeNeeded` for the head |
| `Commit` / `WriteCommit` (`doCommit`) | Branch head == the head the caller read | `ErrMergeNeeded`; `ErrAlreadyCommitted` if the head already equals the new commit |
| `FastForward` | New commit descends from the current head (checked before `update`), and the head did not move | `ErrMergeNeeded` |
| `SetHead(ds, addr, wsPath, preconditions...)` | Caller-supplied `Precondition` functions; head type unchanged | Precondition's error; type-change error |
| `Tag` (`doTag`) | Tag does not exist | "tag ... already exists" |

`CommitWithWorkingSet` moves the branch head and its working set in one root
CAS, so a commit and its working set are published together or not at all.

`SetHead` with a working-set path overwrites that branch's working set with a
clean one that matches the new commit. It does not compare against a previous
working-set hash.

### Working-set prevHash

`DoltDB.UpdateWorkingSet` and `DoltDB.CommitWithWorkingSet`
(`libraries/doltcore/doltdb/doltdb.go`) take `prevHash`. It is the hash of the
stored `WorkingSet` message (`WRST`), not the hash of its root value. A caller
gets it by loading the working set (`WorkingSet.HashOf()`). Writing back with a
stale `prevHash` fails with `ErrOptimisticLockFailed`. That is how Dolt detects
two writers that both read the same working set and then both try to replace
it.
