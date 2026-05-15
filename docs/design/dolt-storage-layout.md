# Dolt-Compatible Storage Layout for DumboDB

**Issue:** hq-ao0wx
**Date:** 2026-03-24 (updated 2026-05-15)

---

Each MongoDB database maps to one NBS store at `<dataDir>/<dbName>/`.

```
NBS root -> STRT (StoreRoot)
|
|-- STRT.refsAM (inline AddressMap)
|   |-- "refs/heads/<branch>"        -> hash(DCMT)
|   \-- "workingSets/heads/<branch>" -> hash(WRST)
|
|-- DCMT (Commit, "DCMT")
|   |-- rootValue   -> hash(RTVL)
|   |-- parents     -> [hash(DCMT_prev)]
|   \-- meta        -> {author, email, desc, timestamp}
|
|-- RTVL (RootValue, "RTVL")
|   |-- feature_version = 7
|   |-- tables      -> bytes(ADRM)
|   |-- foreign_key_addr = [0; 20]
|   \-- collation   = utf8mb4_0900_bin
|
|-- WRST (WorkingSet, "WRST")
|   |-- working_root_addr -> hash(RTVL_working)
|   \-- staged_root_addr  -> hash(RTVL_head)
|
|-- ADRM (collections AddressMap, "ADRM")
|   |-- "myCollection"    -> hash(DTBL)
|   \-- "otherCollection" -> hash(DTBL)
|
|-- DSCH (TableSchema, "DSCH")  <- one per database, shared across all collections
|   |-- column[0]: _id  BINARY(20) NOT NULL PK  (ByteStringEnc)
|   \-- column[1]: doc  JSON NOT NULL            (JSONAddrEnc)
|
\-- DTBL (Table, "DTBL")  <- one per collection
    |-- schema            -> hash(DSCH)
    |-- primary_index     -> bytes(PRLM root node)
    \-- secondary_indexes -> bytes(ADRM: index_name -> hash(DTBL_index))

PRLM (prolly.Map, "PRLM")
    key:   ByteString(SHA-512[:20] of canonical BSON {"_id": value})
    value: JSONAddr(hash of JSON prolly tree for the document body)
```

## Migration

| Detected root format | Action |
|---|---|
| ADRM (pre-STRT) | Wrap in RTVL, write initial STRT commit |
| STRT with ADRM-valued commit rootValue | Write new commit with RTVL |
| STRT with RTVL-valued commit | Current format; no migration needed |
