# Dolt-Compatible Storage Layout for DocuDolt

**Issue:** hq-ao0wx
**Date:** 2026-03-24
**Status:** Design (pre-implementation)

---

## 1. Current State

DocuDolt uses Dolt's NBS (Noms Block Store) as its storage engine, structured as a
Dolt-compatible repository layout. As of this writing, the layout is:

```
NBS root → STRT (StoreRoot)
│
├── STRT.refsAM (inline AddressMap)
│   ├── "refs/heads/main"       → DCMT hash
│   └── "workingSets/heads/main" → WRST hash
│
├── DCMT (Commit flatbuffer, file ID "DCMT")
│   ├── rootValue               → hash(ADRM)  ← BUG: should be hash(RTVL)
│   └── parents                 → []
│
├── WRST (WorkingSet flatbuffer, file ID "WRST")
│   ├── working_root_addr       → hash(ADRM)  ← BUG: should be hash(RTVL)
│   └── staged_root_addr        → hash(ADRM)  ← BUG: should be hash(RTVL)
│
└── ADRM (collections AddressMap, file ID "ADRM")
    ├── "myCollection"          → hash(PRLM)
    └── "otherCollection"       → hash(PRLM)

PRLM (prolly.Map, file ID "PRLM")
    key:   Int64(recordID)
    value: BytesAddr(bsonChunkHash)

BSON chunk (raw BSON document bytes)
```

### What Works Today

- `dolt fsck` passes — all chunks are reachable
- `dolt log` shows commit history
- Persistence across restarts works
- ADRM → STRT migration works

### What Fails Today

- `dolt status` panics or errors
- `dolt diff` fails
- `dolt sql` shows no tables

### Root Cause

Dolt's `LoadRootValueFromRootIshAddr` reads the working set's `working_root_addr`
and calls `decodeRootNomsValue`, which validates the file ID against `"RTVL"`
(RootValue) and `"DGRV"` (DoltgresRootValue). When docudolt stores an ADRM hash
there instead, the validation fails with `ErrNoRootValAtHash`.

The same applies to the commit's `rootValue`: dolt expects an RTVL chunk, but
docudolt stores an ADRM chunk directly.

---

## 2. Investigation: What Is a Dolt RootValue?

### File Identifiers (confirmed from dolt v0.40.5)

| Chunk type    | File ID | Description                         |
|---------------|---------|-------------------------------------|
| StoreRoot     | `STRT`  | NBS root; embeds refsAM inline      |
| Commit        | `DCMT`  | Commit metadata + parent refs       |
| WorkingSet    | `WRST`  | Working/staged root addresses       |
| **RootValue** | `RTVL`  | Dolt database state (tables, FK, …) |
| AddressMap    | `ADRM`  | Prolly tree of name → hash entries  |
| ProllyMap     | `PRLM`  | Prolly tree of key → value entries  |

### RootValue Flatbuffer Schema (`serial/rootvalue.fbs`)

```
table RootValue {
  feature_version: int64;       // must be ≤ DoltFeatureVersion (currently 7)
  tables: [ubyte];              // serialized ADRM: table_name → table_root_hash
  foreign_key_addr: [ubyte];    // 20-byte hash (zero = no foreign keys)
  collation: Collation;         // e.g. utf8mb4_0900_bin
  schemas: [DatabaseSchema];    // optional schema entries
}
file_identifier "RTVL";
```

### Minimal Valid RootValue (from `EmptyRootValue` in dolt source)

```go
// Minimum RTVL that dolt accepts without error:
// feature_version = 7  (DoltFeatureVersion)
// tables          = empty ADRM (prolly.NewEmptyAddressMap)
// foreign_key_addr = [0; 20]
// collation       = utf8mb4_0900_bin
// schemas         = absent
```

Size: approximately 120–160 bytes.

### WorkingSet Flatbuffer Schema (`serial/workingset.fbs`)

```
table WorkingSet {
  working_root_addr: [ubyte] (required); // 20-byte hash → RTVL chunk
  staged_root_addr:  [ubyte];            // 20-byte hash → RTVL chunk
  name:    string;
  email:   string;
  desc:    string;
  timestamp_millis: uint64;
  merge_state:   MergeState;
  rebase_state:  RebaseState;
}
file_identifier "WRST";
```

### How `dolt status` Walks the Tree

```
dolt status
  → datas.Database.GetDataset("workingSets/heads/main")  → WRST
  → LoadRootNomsValueFromRootIshAddr(ws.WorkingAddr)
      → vr.ReadValue(ws.WorkingAddr)                     → must be RTVL
      → decodeRootNomsValue → isRootValue                → checks "RTVL"
  → NewRootValue(rtvl bytes)
      → serial.TryGetRootAsRootValue                     → validates
      → checks feature_version ≤ DoltFeatureVersion
  → compares HEAD RTVL.tables vs working RTVL.tables
      → if same hash → "nothing to commit"
      → if different → lists modified table names
```

`dolt status` only compares the ADRM hashes embedded in the RTVL.tables field.
It does **not** decode individual table entries for a clean-state check.

---

## 3. Options Analysis

### Option A: Collections ADRM directly as RTVL.tables

Use the collections AddressMap as the `tables` byte vector inside the RTVL. Each
entry maps `collection_name → prolly.Map_root_hash`.

```
RTVL.tables → ADRM
    "users"     → hash(users_PRLM)
    "orders"    → hash(orders_PRLM)
```

**Pros:**
- Single source of truth for the collections map
- `dolt status` works (same ADRM hash → clean; different ADRM hash → dirty)
- Minimal code change: build RTVL wrapper around existing ADRM bytes

**Cons:**
- `dolt diff` across commits tries to decode each collection as a SQL table
  (reads the prolly.Map as a dolt table chunk; fails on file ID mismatch)
- `dolt sql` shows collection names as table names but can't read rows
- `dolt checkout` and `dolt merge` fail

**When `dolt status` is safe:**
- Working/staged/HEAD all point to the same RTVL → "nothing to commit" ✓
- After a write, docudolt updates all three to the new RTVL → remains clean ✓

### Option B: Stub RTVL (empty tables) + collections ADRM stored separately

Use a minimal RTVL with an empty tables ADRM for all dolt CLI purposes. Store
the collections ADRM reference via a separate mechanism (e.g., in commit message
metadata as a JSON-encoded hash, or as a custom extended attribute).

**Pros:**
- `dolt status` always shows clean (empty tables == empty tables)
- `dolt diff` shows no table changes (safe)
- `dolt log` / `dolt fsck` work

**Cons:**
- Collections data is "invisible" to dolt CLI
- Requires a separate side-channel to record the collections ADRM hash per commit
- More complex (two parallel data structures per commit)

### Option C: Full dolt SQL tables (each collection = dolt table)

Represent each MongoDB collection as a proper dolt SQL table, with a dolt
table schema wrapping the BSON payload. Each row's primary key is `RecordID`
and the single column is a `BLOB` of BSON bytes.

**Pros:**
- Full dolt CLI compatibility (`dolt diff`, `dolt sql`, `dolt checkout`)
- Enables dolt branch operations on MongoDB data

**Cons:**
- Major rework of the storage layer
- Requires dolt table format (additional flatbuffer nesting per collection)
- Scope is much larger than initial compatibility goal

---

## 4. Recommended Approach: Option A

**Use the collections ADRM as the RTVL.tables field.**

This is the minimal change that makes `dolt status` and `dolt fsck` work without
breaking the existing storage or data access patterns. The key insight is that
`dolt status` only hashes the tables ADRM; it does not decode individual entries
for a clean-state comparison. As long as docudolt always keeps HEAD commit RTVL ==
working set RTVL == staged RTVL, `dolt status` will show "nothing to commit".

### Rationale

1. `dolt status` is the most-used CLI command for health checking. Option A makes
   it work immediately.
2. The collections ADRM is already stored as the commit's data payload. Wrapping
   it in an RTVL shell adds ~120 bytes per commit, not a new data structure.
3. Option B requires a side-channel for the collections hash, adding complexity
   with no clear benefit over Option A for the current use case.
4. Option C is deferred to a future design (full SQL compatibility).

---

## 5. Target Storage Layout

After the change, each docudolt database has this chunk graph:

```
NBS root → STRT (StoreRoot, "STRT")
│
├── STRT.refsAM (inline ADRM)
│   ├── "refs/heads/main"        → hash(DCMT_n)
│   └── "workingSets/heads/main" → hash(WRST_n)
│
├── DCMT_n (Commit, "DCMT")
│   ├── rootValue                → hash(RTVL_n)   ← NEW: was hash(ADRM)
│   ├── parents                  → [hash(DCMT_n-1)]
│   └── meta                     → {author, email, desc, timestamp}
│
├── RTVL_n (RootValue, "RTVL")                    ← NEW chunk type
│   ├── feature_version          = 7
│   ├── tables                   → bytes(ADRM_n)  ← collections ADRM embedded inline
│   ├── foreign_key_addr         = [0; 20]
│   └── collation                = utf8mb4_0900_bin
│
├── WRST_n (WorkingSet, "WRST")
│   ├── working_root_addr        → hash(RTVL_n)   ← NEW: was hash(ADRM)
│   └── staged_root_addr         → hash(RTVL_n)   ← NEW: was hash(ADRM)
│
└── ADRM_n (collections AddressMap, "ADRM")
    ├── "myCollection"           → hash(PRLM_a)
    └── "otherCollection"        → hash(PRLM_b)

PRLM_a (prolly.Map, "PRLM")
    key:   Int64(recordID)
    value: BytesAddr(bsonChunkHash)
```

### Invariant

At all times: `DCMT_n.rootValue == WRST_n.working_root_addr == WRST_n.staged_root_addr`
(all three point to the same RTVL chunk). This ensures `dolt status` always shows
"nothing to commit, working tree clean".

### Diff Between Commits

To reconstruct the collections ADRM from a commit:

```
commit → RTVL.tables (bytes) → parse as ADRM → collection_name → PRLM root hash
```

---

## 6. Migration Strategy

### Existing Stores (STRT root with ADRM-valued commits)

Stores written by the previous code have commit `rootValue` pointing to an ADRM.
The migration must:

1. Detect: read the existing commit's root value chunk; check file ID.
2. Migrate: if file ID is `"ADRM"`, build an RTVL wrapping those ADRM bytes and
   write a new commit pointing to the RTVL.
3. Update: write a new working set pointing to the new RTVL.

This migration is non-destructive: old ADRM chunks remain in the store (NBS is
append-only). Only the STRT root and working set are updated.

**Migration trigger:** at `getOrOpenDB` time, when the `serial.StoreRootFileID`
path reads the head commit's root value and detects a non-RTVL file ID.

```go
case serial.StoreRootFileID:
    ds, err = doltDB.GetDataset(ctx, mainDataset)
    // ...
    if ds.HasHead() {
        headVal, _, _ := ds.MaybeHeadValue()
        sm := headVal.(dolttypes.SerialMessage)
        fileID := serial.GetFileID([]byte(sm))
        if fileID == serial.AddressMapFileID {
            // OLD format: rootValue is ADRM — migrate in-place
            migrateADRMCommitToRTVL(ctx, doltDB, vs, ns, ds)
        }
        // else fileID == serial.RootValueFileID → new format, nothing to do
    }
```

### New Stores

No migration needed; `getOrOpenDB` builds an RTVL from the start.

---

## 7. What Dolt CLI Commands Work After the Change

| Command               | Works? | Notes                                              |
|-----------------------|--------|----------------------------------------------------|
| `dolt fsck`           | ✓      | All chunks reachable                               |
| `dolt log`            | ✓      | Reads commit metadata and parent chain             |
| `dolt status`         | ✓      | Compares RTVL hashes; shows "nothing to commit"    |
| `dolt diff`           | ✗      | Tries to decode collections as SQL tables; fails   |
| `dolt sql`            | ✗      | No SQL schema defined; returns empty table list    |
| `dolt checkout`       | ✗      | Not applicable (single branch)                     |
| `dolt clone`          | ✗      | Requires remote push support (not yet implemented) |
| `dolt branch`         | ✓      | Branch listing reads STRT refsAM                   |

`dolt diff` fails because, when comparing two commits with different collections
AMs, it reads the hash values (prolly.Map roots) and tries to open them as dolt
table chunks. The prolly.Map's root node doesn't have the expected file ID.

`dolt diff` can be made to work in a future iteration (Option C or a hybrid where
each collection is wrapped in a dolt table format).

---

## 8. Concrete Implementation Steps

### Step 1: Add `buildRootValueFlatbuffer` function

```go
// buildRootValueFlatbuffer builds an RTVL flatbuffer wrapping the given
// collections AddressMap as the tables field.
func buildRootValueFlatbuffer(am prolly.AddressMap) serial.Message {
    builder := fb.NewBuilder(256)
    amBytes := []byte(tree.ValueFromNode(am.Node()).(dolttypes.SerialMessage))
    tablesOff := builder.CreateByteVector(amBytes)
    var emptyHash [hash.ByteLen]byte
    fkOff := builder.CreateByteVector(emptyHash[:])
    serial.RootValueStart(builder)
    serial.RootValueAddFeatureVersion(builder, 7) // DoltFeatureVersion
    serial.RootValueAddCollation(builder, serial.Collationutf8mb4_0900_bin)
    serial.RootValueAddTables(builder, tablesOff)
    serial.RootValueAddForeignKeyAddr(builder, fkOff)
    return serial.FinishMessage(builder, serial.RootValueEnd(builder), []byte(serial.RootValueFileID))
}
```

### Step 2: Update `commitCollectionsAM`

Replace the raw AM value passed to `doltDB.Commit` with an RTVL-wrapped value:

```go
func commitCollectionsAM(ctx context.Context, doltDB datas.Database, vs *dolttypes.ValueStore, ds datas.Dataset, am prolly.AddressMap, desc string) (datas.Dataset, prolly.AddressMap, error) {
    // Build RTVL wrapping the collections AM.
    rtvlMsg := buildRootValueFlatbuffer(am)
    rtvlValue := dolttypes.SerialMessage(rtvlMsg)

    // Commit the RTVL as the new rootValue.
    meta, err := datas.NewCommitMeta("docudolt", "docudolt@localhost", desc)
    // ...
    newDS, err := doltDB.Commit(ctx, ds, rtvlValue, datas.CommitOptions{Meta: meta})
    // ...
}
```

### Step 3: Update `updateWorkingSet`

Write the RTVL chunk and use its hash as the working/staged root:

```go
func updateWorkingSet(ctx context.Context, doltDB datas.Database, vs *dolttypes.ValueStore, am prolly.AddressMap) error {
    rtvlMsg := buildRootValueFlatbuffer(am)
    rtvlRef, err := vs.WriteValue(ctx, dolttypes.SerialMessage(rtvlMsg))
    if err != nil {
        return fmt.Errorf("writing RTVL: %w", err)
    }
    // ... use rtvlRef as WorkingRoot and StagedRoot
}
```

### Step 4: Update `getOrOpenDB` — reading collections AM from RTVL

When reading an existing STRT-format database, extract the ADRM from the RTVL:

```go
case serial.RootValueFileID:
    rtvl, err := serial.TryGetRootAsRootValue([]byte(sm), serial.MessagePrefixSz)
    // ...
    amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
    am, err = prolly.NewAddressMap(amNode, ns)

case serial.AddressMapFileID:
    // Legacy format: rootValue is a raw ADRM
    // → trigger migration (write new commit with RTVL wrapper)
```

### Step 5: Migration for existing ADRM-valued commits

Add a migration path in `getOrOpenDB` (STRT branch):

```go
// After loading ds for STRT root:
if ds.HasHead() {
    headVal, _, _ := ds.MaybeHeadValue()
    sm := headVal.(dolttypes.SerialMessage)
    if serial.GetFileID([]byte(sm)) == serial.AddressMapFileID {
        // Old format: head commit rootValue is ADRM
        amNode, _, _ := tree.NodeFromBytes([]byte(sm))
        am, _ = prolly.NewAddressMap(amNode, ns)
        // Write a new migration commit with RTVL rootValue
        ds, am, err = commitCollectionsAM(ctx, doltDB, vs, ds, am, "migrate: wrap collections AM in RTVL")
    } else {
        // New format: extract am from RTVL.tables
        // ...
    }
}
```

### Step 6: Update `migrateADRMtoSTRT`

The `migrateADRMtoSTRT` function already handles the case where the NBS root is
an ADRM (legacy pre-STRT format). After this change, the initial commit it creates
must also use an RTVL:

```go
// Instead of:
commit, err := datas.NewCommitForValue(ctx, cs, vs, ns, tree.ValueFromNode(am.Node()), ...)
// Use:
rtvlMsg := buildRootValueFlatbuffer(am)
commit, err := datas.NewCommitForValue(ctx, cs, vs, ns, dolttypes.SerialMessage(rtvlMsg), ...)
```

### Step 7: Pass `vs` to helper functions

`buildRootValueFlatbuffer` doesn't need `vs` (it doesn't write to the store; it
returns bytes). However, `updateWorkingSet` needs `vs` to call `WriteValue`.
Add `vs *dolttypes.ValueStore` as a parameter to `updateWorkingSet` and
`commitCollectionsAM`, threading it through from `dbState`.

Alternatively, store `vs` in `dbState` (currently only `doltDB`, `cs`, `ns` are
stored):

```go
type dbState struct {
    mu     sync.RWMutex
    cs     *nbs.GenerationalNBS
    ns     tree.NodeStore
    vs     *dolttypes.ValueStore   // NEW
    doltDB datas.Database
    ds     datas.Dataset
    am     prolly.AddressMap
    uuids  map[string]string
}
```

---

## 9. Testing

After the change, the following should pass:

1. **Unit test**: `TestInitialCommitMessage` — existing, verifies init commit.
2. **Unit test**: `TestPersistenceAcrossRestart` — existing, verifies round-trip.
3. **New unit test**: `TestRTVLFormat` — verifies the head commit's rootValue
   has file ID `"RTVL"` and that the embedded ADRM matches the expected collections.
4. **New unit test**: `TestWorkingSetRTVL` — verifies the working set's
   `working_root_addr` and `staged_root_addr` both point to RTVL chunks.
5. **Integration test** (manual): After inserting a document, run `dolt status`
   in the database directory; must show "On branch main, nothing to commit".
6. **Migration test**: Open a store written by the old code (ADRM-valued commits);
   verify it migrates without error and `dolt status` passes afterward.

---

## 10. Deferred Work

| Item | Notes |
|------|-------|
| `dolt diff` compatibility | Requires wrapping each PRLM as a dolt table chunk (Option C) |
| `dolt sql` compatibility | Requires schema metadata in RTVL + dolt table format |
| Multi-branch support | Currently all data is on `refs/heads/main` |
| Remote push/pull | `dolt clone` and `dolt push` need remote protocol support |
| Foreign key tracking | Currently always zero; could map to MongoDB index metadata |
