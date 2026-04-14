# Dolt Table Format for DumboDB Collections

**Issue:** hq-r2cpj
**Date:** 2026-03-24
**Status:** Implemented (Option C)

---

## 1. Investigation Findings

### 1.1 Table File ID

```
serial.TableFileID = "DTBL"
```

Defined in `gen/fb/serial/fileidentifiers.go`. This is what dolt checks when
reading a chunk referenced from the tables ADRM (via `TableFromAddr`).

The exact error that `dolt status` produces with Option A (RTVL.tables ADRM
pointing to raw prolly.Map roots):

```
table ref is unexpected noms value; GetFileID == TUPM
```

`TableFromAddr` in `libraries/doltcore/doltdb/durable/table.go`:
```go
id := serial.GetFileID(sm)
if id != serial.TableFileID {
    err = errors.New("table ref is unexpected noms value; GetFileID == " + id)
    return nil, err
}
```

### 1.2 Table Flatbuffer Schema (`serial/table.fbs`)

```flatbuffers
table Table {
  schema:[ubyte] (required);         // 20-byte hash → DSCH chunk (TableSchema)
  primary_index:[ubyte] (required);  // inline TUPM bytes (prolly.Map root node)
  secondary_indexes:[ubyte];         // inline ADRM bytes (empty AM for no indexes)
  auto_increment_value:uint64;
  conflicts:Conflicts;               // all zero hashes for clean state
  violations:[ubyte];                // 20-byte zero hash
  artifacts:[ubyte];                 // 20-byte zero hash
}
file_identifier "DTBL";
```

**Key observation**: the `schema` field is a 20-byte hash address (like `foreign_key_addr`
in RTVL), NOT inline bytes. The DSCH chunk is written separately to the value store.

### 1.3 Schema File ID and Structure (`serial/schema.fbs`)

```
serial.TableSchemaFileID = "DSCH"
```

```flatbuffers
table TableSchema {
  columns:[Column] (required);
  clustered_index:Index (required);
  secondary_indexes:[Index];
  checks:[CheckConstraint];
  collation:Collation;
  has_features_after_try_accessors:bool;
  comment:string;
}
file_identifier "DSCH";
```

Each `Column`:
```flatbuffers
table Column {
  name:string (required);
  sql_type:string;
  default_value:string;
  comment:string;
  display_order:int16;
  tag: uint64;              // stable column identity (survives renames)
  encoding:Encoding;        // physical storage encoding
  primary_key:bool;
  nullable:bool;
  auto_increment:bool;
  hidden:bool;
  generated:bool;
  virtual:bool;
  on_update_value:string;
}
```

Each `Index` (clustered or secondary):
```flatbuffers
table Index {
  name:string;
  comment:string;
  index_columns:[uint16];   // column indices in columns vector
  key_columns:[uint16];     // column indices for key tuple fields
  value_columns:[uint16];   // column indices for value tuple fields
  primary_key:bool;
  unique_key:bool;
  system_defined:bool;
  prefix_lengths:[uint16];
  spatial_key:bool;
  ...
}
```

### 1.4 Minimal Valid Table Descriptor for DumboDB Collections

Schema: `_id BIGINT NOT NULL PK, doc LONGBLOB NOT NULL`

```
columns:
  [0] _id:  sql_type="bigint",   encoding=Int64Enc (9),     primary_key=true,  nullable=false, tag=1
  [1] doc:  sql_type="longblob", encoding=BytesAddrEnc (21), primary_key=false, nullable=false, tag=2

clustered_index:
  index_columns = [0]   (column 0 = _id)
  key_columns   = [0]
  value_columns = [1]   (column 1 = doc)
  primary_key   = true
  unique_key    = true

secondary_indexes: [] (empty)
checks: [] (empty)
collation: utf8mb4_0900_bin
```

### 1.5 TypeDescriptors vs Existing Prolly.Map

Our current prolly.Map already uses exactly the right encodings:
- `keyDesc = val.NewTupleDescriptor(val.Type{Enc: val.Int64Enc, Nullable: false})`
- `valDesc = val.NewTupleDescriptor(val.Type{Enc: val.BytesAddrEnc, Nullable: false})`

These map directly to `BIGINT NOT NULL PK` (Int64Enc) and `LONGBLOB NOT NULL`
(BytesAddrEnc) in the schema above. **No prolly.Map reconstruction is needed.**

### 1.6 How `dolt sql SHOW TABLES` Works

Dolt reads:
1. Working set → RTVL.tables (ADRM bytes)
2. Parse ADRM → collection names as table names
3. For each name, optionally reads the DTBL to get schema for column info

The table name comes from the ADRM key (the collection name string), not from
inside the DTBL. So `SHOW TABLES` works once ADRM entries point to valid DTBLs.

### 1.7 How `dolt diff` Works

Dolt compares two RTVLs (HEAD and working):
1. Parses both AMs from RTVL.tables
2. Finds differing entries (by hash comparison)
3. For each differing entry, calls `TableFromAddr` → expects DTBL file ID
4. Reads primary_index bytes from DTBL → constructs prolly.Map
5. Diffs the two maps row-by-row

This works with proper DTBL format.

---

## 2. Implementation: Option C Storage Layout

After the change, each ADRM entry maps:
```
collection_name → hash(DTBL chunk)
```

Where the DTBL chunk contains:
```
DTBL.schema        = 20-byte hash → DSCH chunk
DTBL.primary_index = inline TUPM bytes (prolly.Map root node)
DTBL.secondary_indexes = inline ADRM bytes (empty)
```

The DSCH chunk (shared across all collections in a DB) contains the table schema
for `_id BIGINT NOT NULL PK, doc LONGBLOB NOT NULL`.

Full chunk graph after Option C:

```
NBS root → STRT
│
├── STRT.refsAM → "refs/heads/main" → DCMT
├── DCMT.rootValue → RTVL
├── WRST.working_root_addr → RTVL (latest working state)
│
├── RTVL.tables (inline ADRM bytes)
│   └── ADRM: "myCollection" → hash(DTBL)
│                              "otherCollection" → hash(DTBL2)
│
├── DTBL.schema = hash(DSCH)        ← NEW
├── DTBL.primary_index = TUPM bytes ← primary index is our prolly.Map
│
├── DSCH.columns = [_id BIGINT PK, doc LONGBLOB]  ← shared schema chunk
│
└── TUPM (prolly.Map nodes, written to chunk store during mutations)
```

---

## 3. dolt CLI Compatibility After Option C

| Command               | Works? | Notes                                              |
|-----------------------|--------|----------------------------------------------------|
| `dolt fsck`           | ✓      | All chunks reachable                               |
| `dolt log`            | ✓      | Reads commit metadata                              |
| `dolt status`         | ✓      | ADRM → DTBL (file ID check passes)                |
| `dolt diff`           | ✓      | DTBL primary_index → valid prolly.Map rows        |
| `dolt sql SHOW TABLES`| ✓      | Table names from ADRM keys                         |
| `dolt sql DESCRIBE t` | ✓      | Reads DSCH for column info                         |
| `dolt checkout`       | ~      | Single branch only (no branch switching)           |
| `dolt clone`          | ✗      | Remote push not yet implemented                    |
